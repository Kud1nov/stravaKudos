// Package store is the SQLite persistence layer.
//
// State that matters across restarts lives here: the access token,
// the roster of athletes, per-athlete feed-check bookkeeping, the
// kudos audit log, and a rolling 7-day window of api_calls for
// ad-hoc diagnostics.
//
// Uses modernc.org/sqlite (pure-Go driver) so the binary stays CGO-free.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is the target version Store.Open migrates to.
const schemaVersion = 1

// Relation values for athletes.relation. Keep in sync with CHECK constraint.
const (
	RelationFollower  = "follower"
	RelationFollowing = "following"
	RelationBoth      = "both"
	RelationCold      = "cold"
)

// FeedStatus values persisted in feed_checks.last_status.
const (
	StatusOK           = "ok"
	StatusRateLimited  = "rate_limited"
	StatusUnauthorized = "unauthorized"
	StatusForbidden    = "forbidden"
	StatusNotFound     = "not_found"
	StatusTransient    = "transient"
)

// Store wraps a *sql.DB with the domain methods used by scheduler and main.
type Store struct {
	db *sql.DB
	// now is injected for tests; production uses time.Now.
	now func() time.Time
}

// Athlete is a row from the athletes table — minimal shape the scheduler needs.
type Athlete struct {
	ID       int64
	Name     string
	Relation string
}

// TokenRecord is what GetToken returns. ExpiresAt may be zero if unknown.
type TokenRecord struct {
	AccessToken string
	ObtainedAt  time.Time
	ExpiresAt   time.Time
}

// Open opens (or creates) a SQLite database at dbPath, applies pragmas,
// runs migrations. dbPath ":memory:" is supported for tests.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	// Single-writer bot, but WAL still buys us durability + concurrent reads
	// (for ad-hoc `sqlite3` inspection while the bot is running).
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}

	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the DB handle.
func (s *Store) Close() error { return s.db.Close() }

// DB returns the underlying *sql.DB for ad-hoc queries (CLI subcommands).
// Avoid using this from scheduler code.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	// Apply the embedded schema.sql (idempotent — uses IF NOT EXISTS).
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(v), 0) FROM schema_version`).Scan(&v); err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	if v < schemaVersion {
		if _, err := s.db.Exec(`INSERT INTO schema_version(v) VALUES (?)`, schemaVersion); err != nil {
			return fmt.Errorf("record schema_version: %w", err)
		}
	}
	return nil
}

// ---- auth ----

// GetToken returns the stored token or (nil, nil) if absent.
func (s *Store) GetToken(ctx context.Context) (*TokenRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT access_token, obtained_at, expires_at FROM auth WHERE id = 1`)
	var (
		t            TokenRecord
		obtained     int64
		expiresSQL   sql.NullInt64
	)
	err := row.Scan(&t.AccessToken, &obtained, &expiresSQL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.ObtainedAt = time.Unix(obtained, 0)
	if expiresSQL.Valid {
		t.ExpiresAt = time.Unix(expiresSQL.Int64, 0)
	}
	return &t, nil
}

// SetToken stores or replaces the access token. expiresAt.IsZero() means
// "unknown" (NULL in DB).
func (s *Store) SetToken(ctx context.Context, accessToken string, expiresAt time.Time) error {
	var expirePtr *int64
	if !expiresAt.IsZero() {
		e := expiresAt.Unix()
		expirePtr = &e
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auth(id, access_token, obtained_at, expires_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  access_token = excluded.access_token,
		  obtained_at  = excluded.obtained_at,
		  expires_at   = excluded.expires_at`,
		accessToken, s.now().Unix(), expirePtr)
	return err
}

// ClearToken wipes the stored token (e.g. on 401).
func (s *Store) ClearToken(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth WHERE id = 1`)
	return err
}

// ---- athletes ----

// UpsertAthlete inserts or updates an athlete, preserving the 'both' relation
// if we've already seen them in the other list. The caller passes the
// relation discovered in the current refresh ('follower' or 'following');
// this method promotes to 'both' if the other was already present.
func (s *Store) UpsertAthlete(ctx context.Context, id int64, name, rel string) error {
	if rel != RelationFollower && rel != RelationFollowing {
		return fmt.Errorf("UpsertAthlete: unexpected relation %q", rel)
	}
	now := s.now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	err = tx.QueryRowContext(ctx, `SELECT relation FROM athletes WHERE id = ?`, id).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO athletes(id, display_name, relation, added_at, last_seen_in_list)
			VALUES (?, ?, ?, ?, ?)`,
			id, name, rel, now, now); err != nil {
			return err
		}
		// initialise feed_checks row (needed for PickStaleAthlete to consider them)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO feed_checks(athlete_id, last_checked_at) VALUES (?, 0)`, id); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		// promote to 'both' if the new relation differs from existing
		// (but 'cold' and 'both' stick — cold means we demoted them, both is terminal merge)
		newRel := existing
		if existing != RelationCold && existing != RelationBoth && existing != rel {
			newRel = RelationBoth
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE athletes SET display_name = ?, relation = ?, last_seen_in_list = ?
			WHERE id = ?`, name, newRel, now, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PickStaleAthlete returns the athlete with the oldest last_checked_at that
// is not 'cold'. Returns (Athlete{}, false) if the table is empty.
func (s *Store) PickStaleAthlete(ctx context.Context) (Athlete, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT a.id, a.display_name, a.relation
		FROM athletes a
		JOIN feed_checks f ON f.athlete_id = a.id
		WHERE a.relation != ?
		ORDER BY f.last_checked_at ASC
		LIMIT 1`, RelationCold)
	var a Athlete
	err := row.Scan(&a.ID, &a.Name, &a.Relation)
	if errors.Is(err, sql.ErrNoRows) {
		return Athlete{}, false, nil
	}
	if err != nil {
		return Athlete{}, false, err
	}
	return a, true, nil
}

// MarkColdAthlete demotes an athlete to 'cold' — they'll still be in the
// table (so relation history isn't lost) but PickStaleAthlete skips them.
func (s *Store) MarkColdAthlete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE athletes SET relation = ? WHERE id = ?`, RelationCold, id)
	return err
}

// ---- feed_checks ----

// MarkFeedCheck records a successful (or specific-status) feed check: sets
// last_checked_at to now, sets last_status, resets consecutive_errors.
func (s *Store) MarkFeedCheck(ctx context.Context, athleteID int64, status string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE feed_checks SET
		  last_checked_at = ?,
		  last_status = ?,
		  consecutive_errors = 0
		WHERE athlete_id = ?`, s.now().Unix(), status, athleteID)
	return err
}

// BumpFeedCheckError records an error: increments consecutive_errors and
// stamps last_checked_at (so scheduler rotates to the next athlete)
// and last_status.
func (s *Store) BumpFeedCheckError(ctx context.Context, athleteID int64, status string) (int, error) {
	_, err := s.db.ExecContext(ctx, `
		UPDATE feed_checks SET
		  last_checked_at = ?,
		  last_status = ?,
		  consecutive_errors = consecutive_errors + 1
		WHERE athlete_id = ?`, s.now().Unix(), status, athleteID)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.db.QueryRowContext(ctx,
		`SELECT consecutive_errors FROM feed_checks WHERE athlete_id = ?`,
		athleteID).Scan(&n)
	return n, err
}

// SetFeedCheckCooldown pushes last_checked_at forward so the athlete
// won't be picked again until `after`.
func (s *Store) SetFeedCheckCooldown(ctx context.Context, athleteID int64, after time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE feed_checks SET last_checked_at = ? WHERE athlete_id = ?`,
		after.Unix(), athleteID)
	return err
}

// ---- kudos_log ----

// AlreadyKudoed returns true iff activity_id has a row in kudos_log.
func (s *Store) AlreadyKudoed(ctx context.Context, activityID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM kudos_log WHERE activity_id = ?`, activityID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// UpsertBootstrapKudos records that we observed has_kudoed=true from Strava
// (we did NOT POST). No-op if already present.
func (s *Store) UpsertBootstrapKudos(ctx context.Context, activityID, athleteID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO kudos_log(activity_id, athlete_id, kudoed_at, api_status)
		VALUES (?, ?, ?, 0)
		ON CONFLICT(activity_id) DO NOTHING`,
		activityID, athleteID, s.now().Unix())
	return err
}

// LogKudos records the result of our own POST (success or error).
func (s *Store) LogKudos(ctx context.Context, activityID, athleteID int64, apiStatus int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO kudos_log(activity_id, athlete_id, kudoed_at, api_status)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(activity_id) DO UPDATE SET
		  kudoed_at = excluded.kudoed_at,
		  api_status = excluded.api_status`,
		activityID, athleteID, s.now().Unix(), apiStatus)
	return err
}

// ---- api_calls ----

// RecordAPICall appends a diagnostics row. Non-fatal on error (caller logs).
func (s *Store) RecordAPICall(ctx context.Context, endpoint string, status int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_calls(ts, endpoint, status) VALUES (?, ?, ?)`,
		s.now().Unix(), endpoint, status)
	return err
}

// GCOlderThan deletes api_calls rows older than `retain`. Returns rows deleted.
func (s *Store) GCOlderThan(ctx context.Context, retain time.Duration) (int64, error) {
	cutoff := s.now().Add(-retain).Unix()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM api_calls WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// WithClock lets tests inject a deterministic clock.
func (s *Store) WithClock(now func() time.Time) *Store { s.now = now; return s }
