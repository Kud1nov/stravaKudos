// Package scheduler drives the bot's main work loop.
//
// Thin-drip: one `tick()` every SCAN_INTERVAL. Each tick picks the athlete
// with the oldest last_checked_at and pulls their feed. At ~80 athletes and
// 90s/tick, each athlete is revisited ~every 2h — matching the v1 cadence
// but with strict single-request-at-a-time pacing so we never burst into
// Strava's rate limiter.
//
// Two additional tickers (Task 9): rosters refresh (24h) and api_calls GC
// (7d).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/akudinov/stravakudos/internal/store"
	"github.com/akudinov/stravakudos/internal/strava"
)

// Config holds the knobs the scheduler needs — a subset of config.Config.
type Config struct {
	ScanInterval          time.Duration
	RosterRefreshInterval time.Duration
	MaxKudosPerPass       int
	KudosJitterMin        time.Duration
	KudosJitterMax        time.Duration
}

// StravaAPI is the narrow interface the scheduler needs from strava.Client.
type StravaAPI interface {
	GetProfile(ctx context.Context, token string) (int64, error)
	GetFollowers(ctx context.Context, token string, athleteID int64) ([]strava.Athlete, error)
	GetFriends(ctx context.Context, token string, athleteID int64) ([]strava.Athlete, error)
	GetFeed(ctx context.Context, token string, athleteID int64) ([]strava.FeedItem, error)
	PostKudos(ctx context.Context, token string, activityID int64) (int, error)
}

// Store is the narrow interface the scheduler needs from store.Store.
// Using the real type (store.Athlete) — no cycle because store depends on
// nothing domain-specific.
type Store interface {
	PickStaleAthlete(ctx context.Context) (store.Athlete, bool, error)
	UpsertAthlete(ctx context.Context, id int64, name, relation string) error
	MarkColdAthlete(ctx context.Context, id int64) error
	MarkFeedCheck(ctx context.Context, athleteID int64, status string) error
	BumpFeedCheckError(ctx context.Context, athleteID int64, status string) (int, error)
	SetFeedCheckCooldown(ctx context.Context, athleteID int64, after time.Time) error
	AlreadyKudoed(ctx context.Context, activityID int64) (bool, error)
	UpsertBootstrapKudos(ctx context.Context, activityID, athleteID int64) error
	LogKudos(ctx context.Context, activityID, athleteID int64, apiStatus int) error
	RecordAPICall(ctx context.Context, endpoint string, status int) error
	GCOlderThan(ctx context.Context, retain time.Duration) (int64, error)
}

// Auth is the scheduler's view of AuthManager.
type Auth interface {
	Ensure(ctx context.Context) (string, error)
	Reauth(ctx context.Context) (string, error)
}

// RateLimiter is the scheduler's view of ratelimit.Limiter.
type RateLimiter interface {
	Wait(ctx context.Context) error
	Pause1Hour(reason string)
	IsPaused() bool
}

// Thresholds for per-athlete error handling (see error matrix in plan §4).
const (
	// After this many consecutive errors for one athlete, push their next
	// check 1 hour into the future (per-athlete cooldown).
	CooldownErrorThreshold = 5
	// After this many consecutive errors, mark the athlete 'cold'
	// (PickStaleAthlete stops returning them).
	ColdDemotionThreshold = 20

	coolDownDuration = time.Hour
)

// Scheduler is the owner of all the tickers.
type Scheduler struct {
	strava StravaAPI
	store  Store
	auth   Auth
	rl     RateLimiter
	cfg    Config
	log    *slog.Logger

	// watchdog is called after each successful tick. In production it's
	// daemon.SdNotify(WATCHDOG=1); in tests it's a no-op or a counter.
	watchdog func()

	// now lets tests freeze time for the kudos-jitter delay.
	now func() time.Time
}

// New constructs a Scheduler.
func New(api StravaAPI, s Store, auth Auth, rl RateLimiter, cfg Config, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		strava:   api,
		store:    s,
		auth:     auth,
		rl:       rl,
		cfg:      cfg,
		log:      log,
		watchdog: func() {},
		now:      time.Now,
	}
}

// SetWatchdog replaces the watchdog callback (e.g. with go-systemd's SdNotify).
func (s *Scheduler) SetWatchdog(fn func()) *Scheduler {
	s.watchdog = fn
	return s
}

// WithClock is for tests.
func (s *Scheduler) WithClock(now func() time.Time) *Scheduler {
	s.now = now
	return s
}

// tick performs one scheduler iteration: wait past any rate-limit pause, pick
// an athlete, fetch feed, process items. Returns an error only for truly
// unexpected conditions (not for 401/429/feed-level errors, which are
// handled in-place).
func (s *Scheduler) tick(ctx context.Context) error {
	if err := s.rl.Wait(ctx); err != nil {
		return err // ctx cancelled
	}
	token, err := s.auth.Ensure(ctx)
	if err != nil {
		return fmt.Errorf("auth.Ensure: %w", err)
	}

	athlete, ok, err := s.store.PickStaleAthlete(ctx)
	if err != nil {
		return fmt.Errorf("PickStaleAthlete: %w", err)
	}
	if !ok {
		s.log.Debug("no athletes to check")
		return nil
	}

	feed, err := s.strava.GetFeed(ctx, token, athlete.ID)
	s.recordAPICall(ctx, "feed", err)

	switch {
	case err == nil:
		if perr := s.processFeed(ctx, token, athlete, feed); perr != nil {
			return perr
		}
		return s.store.MarkFeedCheck(ctx, athlete.ID, statusOK)

	case errors.Is(err, strava.ErrUnauthorized):
		s.log.Info("401 on feed, re-authenticating", "athlete", athlete.ID)
		if _, aerr := s.auth.Reauth(ctx); aerr != nil {
			return fmt.Errorf("reauth: %w", aerr)
		}
		// Do NOT advance last_checked_at — we want to retry this athlete on next tick.
		return nil

	case errors.Is(err, strava.ErrRateLimited):
		var rl *strava.RateLimitedError
		reason := "feed 429"
		if errors.As(err, &rl) && rl.RetryAfter > 0 {
			reason = fmt.Sprintf("feed 429 retry-after=%s", rl.RetryAfter)
		}
		s.rl.Pause1Hour(reason)
		s.log.Warn("rate limited", "reason", reason, "athlete", athlete.ID)
		return s.store.MarkFeedCheck(ctx, athlete.ID, statusRateLimited)

	case errors.Is(err, strava.ErrForbidden):
		return s.handleAthleteError(ctx, athlete.ID, statusForbidden)
	case errors.Is(err, strava.ErrNotFound):
		return s.handleAthleteError(ctx, athlete.ID, statusNotFound)
	case errors.Is(err, strava.ErrTransient):
		s.log.Warn("transient error on feed", "athlete", athlete.ID, "err", err)
		_, berr := s.store.BumpFeedCheckError(ctx, athlete.ID, statusTransient)
		return berr

	default:
		s.log.Error("unknown feed error", "athlete", athlete.ID, "err", err)
		_, berr := s.store.BumpFeedCheckError(ctx, athlete.ID, statusTransient)
		return berr
	}
}

// handleAthleteError bumps the error counter, applies cooldown at
// ColdDemotionThreshold lives at the cold demotion point. Used for 403/404.
func (s *Scheduler) handleAthleteError(ctx context.Context, athleteID int64, status string) error {
	n, err := s.store.BumpFeedCheckError(ctx, athleteID, status)
	if err != nil {
		return err
	}
	switch {
	case n >= ColdDemotionThreshold:
		s.log.Info("demoting athlete to cold", "athlete", athleteID, "consecutive_errors", n, "status", status)
		return s.store.MarkColdAthlete(ctx, athleteID)
	case n >= CooldownErrorThreshold:
		s.log.Info("per-athlete cooldown", "athlete", athleteID, "consecutive_errors", n, "status", status)
		return s.store.SetFeedCheckCooldown(ctx, athleteID, s.now().Add(coolDownDuration))
	}
	return nil
}

// processFeed walks the feed items and applies bootstrap / kudos / budget rules.
// Separated from tick() for readability and to give tests a direct surface.
func (s *Scheduler) processFeed(ctx context.Context, token string, athlete store.Athlete, feed []strava.FeedItem) error {
	budget := s.cfg.MaxKudosPerPass
	for _, item := range feed {
		// Check ctx cancel before each item so graceful shutdown doesn't
		// take 10s of kudos-jitter to react.
		if err := ctx.Err(); err != nil {
			return err
		}

		if item.HasKudoed {
			if err := s.store.UpsertBootstrapKudos(ctx, item.ActivityID, athlete.ID); err != nil {
				return fmt.Errorf("UpsertBootstrapKudos: %w", err)
			}
			continue
		}

		// Secondary defence against re-POSTing.
		already, err := s.store.AlreadyKudoed(ctx, item.ActivityID)
		if err != nil {
			return fmt.Errorf("AlreadyKudoed: %w", err)
		}
		if already {
			continue
		}

		if budget == 0 {
			s.log.Info("kudos budget exhausted for this pass",
				"athlete", athlete.ID, "skipped_activity", item.ActivityID)
			break
		}

		// Jitter between kudos POSTs to avoid notification-lavine.
		if err := s.sleepJitter(ctx); err != nil {
			return err
		}

		status, perr := s.strava.PostKudos(ctx, token, item.ActivityID)
		s.recordAPICallStatus(ctx, "kudos", status, perr)
		if perr != nil && !isIdempotent(status) {
			// Log the error but try to record the status for audit and continue.
			s.log.Warn("PostKudos failed", "activity", item.ActivityID, "status", status, "err", perr)
			_ = s.store.LogKudos(ctx, item.ActivityID, athlete.ID, status)
			continue
		}
		if err := s.store.LogKudos(ctx, item.ActivityID, athlete.ID, status); err != nil {
			return fmt.Errorf("LogKudos: %w", err)
		}
		budget--
	}
	return nil
}

func isIdempotent(status int) bool {
	return status == 200 || status == 201 || status == 409
}

// sleepJitter sleeps a random duration in [KudosJitterMin, KudosJitterMax].
// Context-aware: returns early on cancel.
func (s *Scheduler) sleepJitter(ctx context.Context) error {
	min, max := s.cfg.KudosJitterMin, s.cfg.KudosJitterMax
	if max <= min {
		max = min
	}
	span := max - min
	var jitter time.Duration
	if span > 0 {
		jitter = time.Duration(rand.Int64N(int64(span)))
	}
	delay := min + jitter
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// recordAPICall is best-effort; errors from RecordAPICall are logged but not
// propagated, since a lost diagnostic row must never kill the scheduler.
func (s *Scheduler) recordAPICall(ctx context.Context, endpoint string, err error) {
	status := 200
	switch {
	case errors.Is(err, strava.ErrUnauthorized):
		status = 401
	case errors.Is(err, strava.ErrRateLimited):
		status = 429
	case errors.Is(err, strava.ErrForbidden):
		status = 403
	case errors.Is(err, strava.ErrNotFound):
		status = 404
	case errors.Is(err, strava.ErrTransient):
		status = 500
	case err != nil:
		status = 0
	}
	if rerr := s.store.RecordAPICall(ctx, endpoint, status); rerr != nil {
		s.log.Warn("RecordAPICall failed", "endpoint", endpoint, "err", rerr)
	}
}

func (s *Scheduler) recordAPICallStatus(ctx context.Context, endpoint string, status int, err error) {
	if status == 0 && err != nil {
		// Translate sentinel errors to a status — reuse recordAPICall's logic.
		s.recordAPICall(ctx, endpoint, err)
		return
	}
	if rerr := s.store.RecordAPICall(ctx, endpoint, status); rerr != nil {
		s.log.Warn("RecordAPICall failed", "endpoint", endpoint, "err", rerr)
	}
}

// Persisted feed_check statuses. Mirror the store.Status* constants but
// duplicated here to avoid importing store (which would cause a cycle).
const (
	statusOK           = "ok"
	statusRateLimited  = "rate_limited"
	statusForbidden    = "forbidden"
	statusNotFound     = "not_found"
	statusTransient    = "transient"
)

// ---- Rosters refresher (Task 9) ----

// refreshRosters fetches /athlete, /followers, /friends and upserts rows
// into the athletes table with the right relation. Routes through
// rl.Wait between requests so a concurrent 429 pause blocks us too —
// conservative, not strictly necessary (those endpoints live in a
// separate bucket per our investigation on rw), but removes any
// cold-start burst concern.
func (s *Scheduler) refreshRosters(ctx context.Context) error {
	if err := s.rl.Wait(ctx); err != nil {
		return err
	}
	token, err := s.auth.Ensure(ctx)
	if err != nil {
		return fmt.Errorf("auth.Ensure: %w", err)
	}

	athleteID, err := s.strava.GetProfile(ctx, token)
	s.recordAPICall(ctx, "profile", err)
	if err != nil {
		return fmt.Errorf("GetProfile: %w", err)
	}

	if err := s.rl.Wait(ctx); err != nil {
		return err
	}
	followers, err := s.strava.GetFollowers(ctx, token, athleteID)
	s.recordAPICall(ctx, "followers", err)
	if err != nil {
		s.log.Warn("GetFollowers failed", "err", err)
		// Don't abort roster refresh — try friends too
	} else {
		for _, a := range followers {
			if err := s.store.UpsertAthlete(ctx, a.ID, a.DisplayName(), store.RelationFollower); err != nil {
				s.log.Warn("UpsertAthlete follower", "id", a.ID, "err", err)
			}
		}
	}

	if err := s.rl.Wait(ctx); err != nil {
		return err
	}
	friends, err := s.strava.GetFriends(ctx, token, athleteID)
	s.recordAPICall(ctx, "friends", err)
	if err != nil {
		s.log.Warn("GetFriends failed", "err", err)
	} else {
		for _, a := range friends {
			if err := s.store.UpsertAthlete(ctx, a.ID, a.DisplayName(), store.RelationFollowing); err != nil {
				s.log.Warn("UpsertAthlete friend", "id", a.ID, "err", err)
			}
		}
	}

	s.log.Info("roster refresh ok",
		"followers", len(followers), "friends", len(friends))
	return nil
}

// ---- Run loop (tickers) ----

// Run is the entry point. Spins up three tickers (feed, rosters, GC) and
// returns on ctx.Done(). Errors in individual ticks are logged, not fatal.
func (s *Scheduler) Run(ctx context.Context) error {
	// First roster refresh right away so cold-start has data.
	if err := s.refreshRosters(ctx); err != nil {
		s.log.Warn("initial roster refresh failed", "err", err)
		// Continue — next ticker cycle will retry
	}
	// First GC immediately too (cheap and keeps api_calls small on restart).
	if deleted, err := s.store.GCOlderThan(ctx, 7*24*time.Hour); err != nil {
		s.log.Warn("initial GC failed", "err", err)
	} else if deleted > 0 {
		s.log.Info("gc api_calls", "deleted", deleted)
	}

	feedTicker := time.NewTicker(s.cfg.ScanInterval)
	defer feedTicker.Stop()
	rosterTicker := time.NewTicker(s.cfg.RosterRefreshInterval)
	defer rosterTicker.Stop()
	gcTicker := time.NewTicker(7 * 24 * time.Hour)
	defer gcTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-feedTicker.C:
			if err := s.tick(ctx); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				s.log.Warn("tick error", "err", err)
			}
			s.watchdog()

		case <-rosterTicker.C:
			if err := s.refreshRosters(ctx); err != nil {
				s.log.Warn("roster refresh error", "err", err)
			}

		case <-gcTicker.C:
			if deleted, err := s.store.GCOlderThan(ctx, 7*24*time.Hour); err != nil {
				s.log.Warn("gc error", "err", err)
			} else if deleted > 0 {
				s.log.Info("gc api_calls", "deleted", deleted)
			}
		}
	}
}
