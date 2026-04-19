package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/akudinov/stravakudos/internal/store"
	"github.com/akudinov/stravakudos/internal/strava"
)

// ---- test doubles ----

type fakeAPI struct {
	mu              sync.Mutex
	profileID       int64
	profileErr      error
	followers       []strava.Athlete
	followersErr    error
	friends         []strava.Athlete
	friendsErr      error
	feedByAthlete   map[int64][]strava.FeedItem
	feedErrByAthlete map[int64]error
	kudosResults    map[int64]kudosResult
	// counts for assertions
	profileCalls   int
	followersCalls int
	friendsCalls   int
	feedCalls      int
	kudosCalls     int
}

type kudosResult struct {
	status int
	err    error
}

func (f *fakeAPI) GetProfile(ctx context.Context, token string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profileCalls++
	return f.profileID, f.profileErr
}
func (f *fakeAPI) GetFollowers(ctx context.Context, token string, _ int64) ([]strava.Athlete, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.followersCalls++
	return f.followers, f.followersErr
}
func (f *fakeAPI) GetFriends(ctx context.Context, token string, _ int64) ([]strava.Athlete, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.friendsCalls++
	return f.friends, f.friendsErr
}
func (f *fakeAPI) GetFeed(ctx context.Context, token string, athleteID int64) ([]strava.FeedItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feedCalls++
	if err, ok := f.feedErrByAthlete[athleteID]; ok {
		return nil, err
	}
	return f.feedByAthlete[athleteID], nil
}
func (f *fakeAPI) PostKudos(ctx context.Context, token string, activityID int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kudosCalls++
	if r, ok := f.kudosResults[activityID]; ok {
		return r.status, r.err
	}
	return 201, nil
}

type fakeAuth struct {
	token         string
	ensureErr     error
	reauthCalls   int
	reauthErr     error
}

func (a *fakeAuth) Ensure(ctx context.Context) (string, error) { return a.token, a.ensureErr }
func (a *fakeAuth) Reauth(ctx context.Context) (string, error) {
	a.reauthCalls++
	return a.token, a.reauthErr
}

// fakeRL always allows (not testing pause semantics here — that's in
// ratelimit_test). Only counts Pause1Hour invocations.
type fakeRL struct {
	pauseCalls int
	lastReason string
	paused     bool
}

func (r *fakeRL) Wait(ctx context.Context) error { return nil }
func (r *fakeRL) Pause1Hour(reason string) {
	r.pauseCalls++
	r.lastReason = reason
	r.paused = true
}
func (r *fakeRL) IsPaused() bool { return r.paused }

// ---- helpers ----

func newRealStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestScheduler(t *testing.T, api StravaAPI, auth Auth, rl RateLimiter, s Store) *Scheduler {
	t.Helper()
	return New(api, s, auth, rl, Config{
		ScanInterval:          time.Minute,
		RosterRefreshInterval: 24 * time.Hour,
		MaxKudosPerPass:       5,
		KudosJitterMin:        0, // no sleep in tests
		KudosJitterMax:        0,
	}, discardLogger())
}

// ---- Task 8a tests ----

func TestTick_NoAthletes(t *testing.T) {
	st := newRealStore(t)
	sch := newTestScheduler(t, &fakeAPI{}, &fakeAuth{token: "t"}, &fakeRL{}, st)

	if err := sch.tick(context.Background()); err != nil {
		t.Errorf("tick on empty store: %v", err)
	}
}

func TestTick_RateLimitedPausesAndMarksFeedCheck(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)
	_ = st.UpsertAthlete(ctx, 1, "Alice", store.RelationFollower)

	api := &fakeAPI{
		feedErrByAthlete: map[int64]error{
			1: &strava.RateLimitedError{RetryAfter: 30 * time.Minute},
		},
	}
	rl := &fakeRL{}
	sch := newTestScheduler(t, api, &fakeAuth{token: "t"}, rl, st)

	if err := sch.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if rl.pauseCalls != 1 {
		t.Errorf("Pause1Hour calls = %d, want 1", rl.pauseCalls)
	}
	var status string
	_ = st.DB().QueryRow(`SELECT last_status FROM feed_checks WHERE athlete_id = 1`).Scan(&status)
	if status != "rate_limited" {
		t.Errorf("feed_checks.last_status = %q, want rate_limited", status)
	}
}

// Critical regression: after a 429 sets a pause, subsequent ticks must NOT
// issue HTTP calls even though our fake RL.Wait always returns immediately.
// We simulate "paused" by observing rl.IsPaused() semantics: a real Limiter's
// Wait would block; here we use a blocking RL to prove the gate works.
func TestTick_PauseBlocksSubsequentTicks(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)
	_ = st.UpsertAthlete(ctx, 1, "A", store.RelationFollower)

	// First call triggers 429, subsequent calls would return empty feed (no kudos).
	api := &fakeAPI{
		feedErrByAthlete: map[int64]error{1: &strava.RateLimitedError{}},
	}
	rl := &blockingRL{}
	sch := newTestScheduler(t, api, &fakeAuth{token: "t"}, rl, st)

	// Tick #1: 429 → pause triggered
	if err := sch.tick(ctx); err != nil {
		t.Fatalf("tick#1: %v", err)
	}
	before := api.feedCalls

	// Switch the fake to return OK — if pause works, no new feed calls because
	// Wait would block forever in a real scenario. We simulate by making Wait
	// return ctx.DeadlineExceeded to stop the tick before any HTTP.
	rl.blockUntilCancel = true

	// Tick #2..#4: every tick should hit Wait and bail before GetFeed
	for i := 0; i < 3; i++ {
		tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		_ = sch.tick(tctx)
		cancel()
	}
	if api.feedCalls != before {
		t.Errorf("additional feed calls during pause: before=%d after=%d", before, api.feedCalls)
	}
}

// blockingRL stays paused permanently after Pause1Hour; Wait blocks until ctx
// cancelled.
type blockingRL struct {
	paused            bool
	blockUntilCancel  bool
	pauseCalls        int
	lastReason        string
}

func (r *blockingRL) Wait(ctx context.Context) error {
	if !r.blockUntilCancel {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}
func (r *blockingRL) Pause1Hour(reason string) {
	r.paused = true
	r.pauseCalls++
	r.lastReason = reason
}
func (r *blockingRL) IsPaused() bool { return r.paused }

func TestTick_401TriggersReauth(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)
	_ = st.UpsertAthlete(ctx, 1, "A", store.RelationFollower)

	api := &fakeAPI{
		feedErrByAthlete: map[int64]error{1: strava.ErrUnauthorized},
	}
	auth := &fakeAuth{token: "t"}
	sch := newTestScheduler(t, api, auth, &fakeRL{}, st)

	if err := sch.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if auth.reauthCalls != 1 {
		t.Errorf("Reauth calls = %d, want 1", auth.reauthCalls)
	}
	// last_checked_at must NOT be advanced — the same athlete should be
	// picked on the next tick.
	a, _, _ := st.PickStaleAthlete(ctx)
	if a.ID != 1 {
		t.Errorf("after 401, expected same athlete next tick, got %d", a.ID)
	}
}

func TestHandleAthleteError_CooldownAndCold(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)
	_ = st.UpsertAthlete(ctx, 1, "A", store.RelationFollower)

	api := &fakeAPI{
		feedErrByAthlete: map[int64]error{1: strava.ErrForbidden},
	}
	sch := newTestScheduler(t, api, &fakeAuth{token: "t"}, &fakeRL{}, st)

	// Drive 5 consecutive 403s → cooldown activates on the 5th
	for i := 0; i < 5; i++ {
		if err := sch.tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	// After cooldown, last_checked_at should be in the future
	var lca int64
	_ = st.DB().QueryRow(`SELECT last_checked_at FROM feed_checks WHERE athlete_id = 1`).Scan(&lca)
	if lca <= time.Now().Add(30*time.Minute).Unix() {
		t.Errorf("cooldown did not push last_checked_at forward; lca=%d, now=%d", lca, time.Now().Unix())
	}

	// 15 more 403s → demoted to cold (total 20). Reset cooldown first so
	// athlete is pickable again.
	_ = st.SetFeedCheckCooldown(ctx, 1, time.Now().Add(-time.Hour))
	for i := 0; i < 15; i++ {
		_ = st.SetFeedCheckCooldown(ctx, 1, time.Now().Add(-time.Hour)) // keep it pickable
		if err := sch.tick(ctx); err != nil {
			t.Fatalf("tick cold #%d: %v", i, err)
		}
	}
	var rel string
	_ = st.DB().QueryRow(`SELECT relation FROM athletes WHERE id = 1`).Scan(&rel)
	if rel != store.RelationCold {
		t.Errorf("after 20 errors, expected cold, got %q", rel)
	}
}

func TestTick_TransientDoesNotPauseScheduler(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)
	_ = st.UpsertAthlete(ctx, 1, "A", store.RelationFollower)

	api := &fakeAPI{
		feedErrByAthlete: map[int64]error{
			1: &strava.TransientError{Code: 500, Cause: errors.New("server boom")},
		},
	}
	rl := &fakeRL{}
	sch := newTestScheduler(t, api, &fakeAuth{token: "t"}, rl, st)

	if err := sch.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if rl.pauseCalls != 0 {
		t.Errorf("transient must NOT trigger global pause; got %d", rl.pauseCalls)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT consecutive_errors FROM feed_checks WHERE athlete_id = 1`).Scan(&n)
	if n != 1 {
		t.Errorf("consecutive_errors = %d, want 1", n)
	}
}
