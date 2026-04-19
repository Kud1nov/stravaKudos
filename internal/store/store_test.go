package store

import (
	"context"
	"testing"
	"time"
)

// fakeClock returns a controllable time source for deterministic tests.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time     { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func newStore(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	fc := &fakeClock{t: time.Unix(1_700_000_000, 0)} // stable 2023-11-14-ish
	s.WithClock(fc.now)
	return s, fc
}

func TestMigrateOnEmptyDB(t *testing.T) {
	s, _ := newStore(t)
	var v int
	if err := s.DB().QueryRow(`SELECT MAX(v) FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("schema_version = %d, want %d", v, schemaVersion)
	}
}

func TestAuth(t *testing.T) {
	ctx := context.Background()
	s, fc := newStore(t)

	if tok, err := s.GetToken(ctx); err != nil || tok != nil {
		t.Fatalf("empty store must return (nil,nil), got (%v,%v)", tok, err)
	}

	expires := fc.now().Add(6 * time.Hour)
	if err := s.SetToken(ctx, "abc", expires); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetToken(ctx)
	if err != nil || got == nil {
		t.Fatalf("expected token, got %v/%v", got, err)
	}
	if got.AccessToken != "abc" {
		t.Errorf("AccessToken = %q", got.AccessToken)
	}
	if !got.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, expires)
	}

	// No-expiry variant.
	if err := s.SetToken(ctx, "xyz", time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetToken(ctx)
	if got.AccessToken != "xyz" || !got.ExpiresAt.IsZero() {
		t.Errorf("expected no expiry, got %+v", got)
	}

	if err := s.ClearToken(ctx); err != nil {
		t.Fatal(err)
	}
	if tok, _ := s.GetToken(ctx); tok != nil {
		t.Error("expected nil after ClearToken")
	}
}

func TestUpsertAthleteRelationPromotion(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	if err := s.UpsertAthlete(ctx, 1, "Alice", RelationFollower); err != nil {
		t.Fatal(err)
	}
	// same relation again — stays follower
	if err := s.UpsertAthlete(ctx, 1, "Alice", RelationFollower); err != nil {
		t.Fatal(err)
	}
	// other relation — promotes to both
	if err := s.UpsertAthlete(ctx, 1, "Alice", RelationFollowing); err != nil {
		t.Fatal(err)
	}
	var rel string
	if err := s.DB().QueryRow(`SELECT relation FROM athletes WHERE id = 1`).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	if rel != RelationBoth {
		t.Errorf("relation = %q, want %q", rel, RelationBoth)
	}

	// 'cold' should not be clobbered by another upsert
	if err := s.MarkColdAthlete(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAthlete(ctx, 1, "Alice", RelationFollower); err != nil {
		t.Fatal(err)
	}
	_ = s.DB().QueryRow(`SELECT relation FROM athletes WHERE id = 1`).Scan(&rel)
	if rel != RelationCold {
		t.Errorf("cold athlete got re-promoted to %q", rel)
	}
}

func TestPickStaleAthlete(t *testing.T) {
	ctx := context.Background()
	s, fc := newStore(t)

	// empty -> not found
	if _, ok, _ := s.PickStaleAthlete(ctx); ok {
		t.Error("empty store must return ok=false")
	}

	for _, id := range []int64{10, 20, 30} {
		if err := s.UpsertAthlete(ctx, id, "a", RelationFollower); err != nil {
			t.Fatal(err)
		}
	}
	// mark 20 as recently checked (newest); 10 and 30 are still at 0
	fc.advance(time.Minute)
	if err := s.MarkFeedCheck(ctx, 20, StatusOK); err != nil {
		t.Fatal(err)
	}
	// mark 30 even later
	fc.advance(time.Minute)
	if err := s.MarkFeedCheck(ctx, 30, StatusOK); err != nil {
		t.Fatal(err)
	}

	a, ok, err := s.PickStaleAthlete(ctx)
	if err != nil || !ok {
		t.Fatalf("pick: %v / %v", ok, err)
	}
	if a.ID != 10 {
		t.Errorf("picked %d, want 10 (never-checked)", a.ID)
	}

	// skip cold athletes
	if err := s.MarkColdAthlete(ctx, 10); err != nil {
		t.Fatal(err)
	}
	a, _, _ = s.PickStaleAthlete(ctx)
	if a.ID != 20 { // 20 was checked before 30
		t.Errorf("picked %d, want 20", a.ID)
	}
}

func TestFeedCheckErrorBumping(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_ = s.UpsertAthlete(ctx, 1, "a", RelationFollower)

	n, err := s.BumpFeedCheckError(ctx, 1, StatusTransient)
	if err != nil || n != 1 {
		t.Fatalf("bump#1 → %d, %v", n, err)
	}
	n, _ = s.BumpFeedCheckError(ctx, 1, StatusTransient)
	if n != 2 {
		t.Errorf("bump#2 → %d, want 2", n)
	}

	// MarkFeedCheck(ok) resets the counter
	_ = s.MarkFeedCheck(ctx, 1, StatusOK)
	var n2 int
	_ = s.DB().QueryRow(`SELECT consecutive_errors FROM feed_checks WHERE athlete_id = 1`).Scan(&n2)
	if n2 != 0 {
		t.Errorf("MarkFeedCheck(ok) should reset; got %d", n2)
	}
}

func TestKudosLog(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	// AlreadyKudoed on empty
	if ok, _ := s.AlreadyKudoed(ctx, 42); ok {
		t.Error("AlreadyKudoed on empty must be false")
	}

	// Bootstrap
	if err := s.UpsertBootstrapKudos(ctx, 42, 1); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AlreadyKudoed(ctx, 42); !ok {
		t.Error("expected true after bootstrap")
	}
	// second bootstrap is no-op
	if err := s.UpsertBootstrapKudos(ctx, 42, 1); err != nil {
		t.Fatal(err)
	}
	var status int
	_ = s.DB().QueryRow(`SELECT api_status FROM kudos_log WHERE activity_id = 42`).Scan(&status)
	if status != 0 {
		t.Errorf("bootstrap api_status = %d, want 0", status)
	}

	// Real POST overwrites
	if err := s.LogKudos(ctx, 42, 1, 201); err != nil {
		t.Fatal(err)
	}
	_ = s.DB().QueryRow(`SELECT api_status FROM kudos_log WHERE activity_id = 42`).Scan(&status)
	if status != 201 {
		t.Errorf("post api_status = %d, want 201", status)
	}
}

func TestAPICallsGC(t *testing.T) {
	ctx := context.Background()
	s, fc := newStore(t)

	// 5 calls "now"
	for i := 0; i < 5; i++ {
		if err := s.RecordAPICall(ctx, "feed", 200); err != nil {
			t.Fatal(err)
		}
	}
	// advance 10 days, record 3 more
	fc.advance(10 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		_ = s.RecordAPICall(ctx, "feed", 200)
	}

	// GC older than 7 days → the 5 originals should go, the 3 fresh stay
	deleted, err := s.GCOlderThan(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5", deleted)
	}
	var remaining int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM api_calls`).Scan(&remaining)
	if remaining != 3 {
		t.Errorf("remaining = %d, want 3", remaining)
	}
}

func TestSetFeedCheckCooldown(t *testing.T) {
	ctx := context.Background()
	s, fc := newStore(t)
	_ = s.UpsertAthlete(ctx, 1, "a", RelationFollower)
	_ = s.UpsertAthlete(ctx, 2, "b", RelationFollower)

	// Push athlete 1 forward by 1h — should skip it for athlete 2
	if err := s.SetFeedCheckCooldown(ctx, 1, fc.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	a, _, _ := s.PickStaleAthlete(ctx)
	if a.ID != 2 {
		t.Errorf("picked %d, expected 2 (1 is on cooldown)", a.ID)
	}
}
