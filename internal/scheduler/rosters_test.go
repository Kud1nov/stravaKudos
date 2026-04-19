package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/akudinov/stravakudos/internal/store"
	"github.com/akudinov/stravakudos/internal/strava"
)

func TestRefreshRosters_ColdStart(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)

	api := &fakeAPI{
		profileID: 42,
		followers: []strava.Athlete{
			{ID: 1, Firstname: "Alice", Lastname: "A"},
			{ID: 2, Firstname: "Bob", Lastname: "B"},
		},
		friends: []strava.Athlete{
			{ID: 2, Firstname: "Bob", Lastname: "B"}, // overlap → should become 'both'
			{ID: 3, Firstname: "Carol", Lastname: "C"},
		},
	}
	sch := newTestScheduler(t, api, &fakeAuth{token: "t"}, &fakeRL{}, st)

	if err := sch.refreshRosters(ctx); err != nil {
		t.Fatal(err)
	}

	// Expected: 3 athletes total; 1=follower, 2=both, 3=following
	cases := []struct {
		id  int64
		rel string
	}{
		{1, store.RelationFollower},
		{2, store.RelationBoth},
		{3, store.RelationFollowing},
	}
	for _, tc := range cases {
		var got string
		err := st.DB().QueryRow(`SELECT relation FROM athletes WHERE id = ?`, tc.id).Scan(&got)
		if err != nil {
			t.Errorf("athlete %d: %v", tc.id, err)
			continue
		}
		if got != tc.rel {
			t.Errorf("athlete %d relation = %q, want %q", tc.id, got, tc.rel)
		}
	}
}

func TestRefreshRosters_FollowersFailsButFriendsStillProcessed(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)

	api := &fakeAPI{
		profileID:    42,
		followersErr: strava.ErrForbidden,
		friends: []strava.Athlete{
			{ID: 10, Firstname: "X", Lastname: "Y"},
		},
	}
	sch := newTestScheduler(t, api, &fakeAuth{token: "t"}, &fakeRL{}, st)

	if err := sch.refreshRosters(ctx); err != nil {
		t.Fatal(err)
	}
	var rel string
	_ = st.DB().QueryRow(`SELECT relation FROM athletes WHERE id = 10`).Scan(&rel)
	if rel != store.RelationFollowing {
		t.Errorf("friend should be persisted even if followers failed; got %q", rel)
	}
}

func TestRefreshRosters_ProfileFailsAborts(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)

	api := &fakeAPI{profileErr: strava.ErrUnauthorized}
	sch := newTestScheduler(t, api, &fakeAuth{token: "t"}, &fakeRL{}, st)

	err := sch.refreshRosters(ctx)
	if err == nil {
		t.Fatal("expected error when GetProfile fails")
	}
	// followers/friends must NOT have been called
	if api.followersCalls != 0 || api.friendsCalls != 0 {
		t.Errorf("followers=%d, friends=%d — should be 0/0 after profile error",
			api.followersCalls, api.friendsCalls)
	}
}

func TestRefreshRosters_SecondPassDoesNotRemoveMissing(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)

	api := &fakeAPI{
		profileID: 42,
		followers: []strava.Athlete{{ID: 1, Firstname: "A", Lastname: ""}},
	}
	sch := newTestScheduler(t, api, &fakeAuth{token: "t"}, &fakeRL{}, st)

	if err := sch.refreshRosters(ctx); err != nil {
		t.Fatal(err)
	}
	// Now user 1 disappears — re-run with empty followers
	api.followers = nil
	if err := sch.refreshRosters(ctx); err != nil {
		t.Fatal(err)
	}
	// Athlete 1 must still be there (we don't auto-delete; demotion is
	// a future feature if needed).
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM athletes WHERE id = 1`).Scan(&n)
	if n != 1 {
		t.Errorf("athlete should remain after disappearing; got count %d", n)
	}
}

func TestRun_FirstRosterRefreshHappensBeforeTicker(t *testing.T) {
	st := newRealStore(t)

	api := &fakeAPI{
		profileID: 42,
		followers: []strava.Athlete{{ID: 1, Firstname: "A", Lastname: ""}},
	}
	sch := New(api, st, &fakeAuth{token: "t"}, &fakeRL{}, Config{
		ScanInterval:          1 * time.Hour, // won't fire in the test window
		RosterRefreshInterval: 24 * time.Hour,
		MaxKudosPerPass:       5,
	}, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()
	<-done // wait for shutdown

	// Even though no ticker fired, the initial refresh should have happened
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM athletes`).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 athlete from initial refresh; got %d", n)
	}
}

func TestRun_WeeklyGCRunsInitially(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)

	// Seed api_calls: 3 old, 2 fresh
	_ = st.DB().QueryRow(`SELECT 1`).Scan(new(int)) // noop; just exercise connection

	// Use store method with faked clock
	fc := &fakeClock{t: time.Now().Add(-10 * 24 * time.Hour)}
	st.WithClock(fc.now)
	for i := 0; i < 3; i++ {
		_ = st.RecordAPICall(ctx, "feed", 200)
	}
	fc.t = time.Now()
	for i := 0; i < 2; i++ {
		_ = st.RecordAPICall(ctx, "feed", 200)
	}
	st.WithClock(time.Now) // real clock for the scheduler

	api := &fakeAPI{profileID: 42}
	sch := New(api, st, &fakeAuth{token: "t"}, &fakeRL{}, Config{
		ScanInterval:          1 * time.Hour,
		RosterRefreshInterval: 24 * time.Hour,
		MaxKudosPerPass:       5,
	}, discardLogger())

	runCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = sch.Run(runCtx)

	var remaining int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM api_calls`).Scan(&remaining)
	// 3 old should have been purged on initial GC; 2 fresh + some recorded
	// by the refreshRosters call during Run — exact count fuzzy but <= 5
	if remaining > 5 || remaining < 2 {
		t.Errorf("remaining api_calls = %d, expected 2..5 (old purged, fresh kept + new recorded)", remaining)
	}
}

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }
