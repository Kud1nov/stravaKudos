package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/akudinov/stravakudos/internal/store"
	"github.com/akudinov/stravakudos/internal/strava"
)

// Task 8b: processFeed (bootstrap + max-5-kudos + graceful shutdown).

func newSchedulerForProcessFeed(t *testing.T, api *fakeAPI, st *store.Store) *Scheduler {
	t.Helper()
	return New(api, st, &fakeAuth{token: "t"}, &fakeRL{}, Config{
		MaxKudosPerPass: 5,
		KudosJitterMin:  0,
		KudosJitterMax:  0,
	}, discardLogger())
}

func TestProcessFeed_MaxFiveKudosPerPass(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)
	_ = st.UpsertAthlete(ctx, 1, "A", store.RelationFollower)

	api := &fakeAPI{}
	sch := newSchedulerForProcessFeed(t, api, st)

	feed := make([]strava.FeedItem, 6)
	for i := range feed {
		feed[i] = strava.FeedItem{ActivityID: int64(100 + i), HasKudoed: false}
	}
	athlete, _, _ := st.PickStaleAthlete(ctx)
	if err := sch.processFeed(ctx, "t", athlete, feed); err != nil {
		t.Fatal(err)
	}
	if api.kudosCalls != 5 {
		t.Errorf("kudos calls = %d, want 5", api.kudosCalls)
	}
	// 5 rows in kudos_log (all api_status=201 from default fakeAPI PostKudos)
	var n, posted int
	_ = st.DB().QueryRow(`SELECT COUNT(*), COUNT(CASE WHEN api_status=201 THEN 1 END) FROM kudos_log`).Scan(&n, &posted)
	if n != 5 || posted != 5 {
		t.Errorf("kudos_log counts: total=%d posted=%d, want 5/5", n, posted)
	}
}

func TestProcessFeed_MixedBootstrapAndPost(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)
	_ = st.UpsertAthlete(ctx, 1, "A", store.RelationFollower)
	api := &fakeAPI{}
	sch := newSchedulerForProcessFeed(t, api, st)

	feed := []strava.FeedItem{
		{ActivityID: 1001, HasKudoed: true},
		{ActivityID: 1002, HasKudoed: true},
		{ActivityID: 1003, HasKudoed: true},
		{ActivityID: 1004, HasKudoed: false},
		{ActivityID: 1005, HasKudoed: false},
	}
	athlete, _, _ := st.PickStaleAthlete(ctx)
	if err := sch.processFeed(ctx, "t", athlete, feed); err != nil {
		t.Fatal(err)
	}
	// 3 bootstrap rows (api_status=0), 2 posted (api_status=201), 0 extra calls
	if api.kudosCalls != 2 {
		t.Errorf("kudos calls = %d, want 2", api.kudosCalls)
	}
	var bootstrap, posted int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM kudos_log WHERE api_status=0`).Scan(&bootstrap)
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM kudos_log WHERE api_status=201`).Scan(&posted)
	if bootstrap != 3 || posted != 2 {
		t.Errorf("bootstrap=%d posted=%d, want 3/2", bootstrap, posted)
	}
}

// Critical: the bootstrap-of-5-years scenario. Fresh DB, feed of 30 items,
// all has_kudoed:true — must emit ZERO POSTs and 30 bootstrap rows.
func TestProcessFeed_BootstrapAllTrue_ZeroPosts(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)
	_ = st.UpsertAthlete(ctx, 1, "A", store.RelationFollower)
	api := &fakeAPI{}
	sch := newSchedulerForProcessFeed(t, api, st)

	feed := make([]strava.FeedItem, 30)
	for i := range feed {
		feed[i] = strava.FeedItem{ActivityID: int64(500_000 + i), HasKudoed: true}
	}
	athlete, _, _ := st.PickStaleAthlete(ctx)
	if err := sch.processFeed(ctx, "t", athlete, feed); err != nil {
		t.Fatal(err)
	}
	if api.kudosCalls != 0 {
		t.Errorf("cold-start bootstrap MUST NOT POST; got %d kudos calls", api.kudosCalls)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM kudos_log WHERE api_status=0`).Scan(&n)
	if n != 30 {
		t.Errorf("bootstrap rows = %d, want 30", n)
	}
}

func TestProcessFeed_SkipsAlreadyKudoed(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)
	_ = st.UpsertAthlete(ctx, 1, "A", store.RelationFollower)
	// Pre-seed: activity 7777 is in our log (e.g. we posted last run).
	_ = st.LogKudos(ctx, 7777, 1, 201)

	api := &fakeAPI{}
	sch := newSchedulerForProcessFeed(t, api, st)

	// Strava says has_kudoed=false (maybe stale Strava cache). Our local log
	// wins for the skip decision — we don't POST again.
	feed := []strava.FeedItem{{ActivityID: 7777, HasKudoed: false}}
	athlete, _, _ := st.PickStaleAthlete(ctx)
	if err := sch.processFeed(ctx, "t", athlete, feed); err != nil {
		t.Fatal(err)
	}
	if api.kudosCalls != 0 {
		t.Errorf("must skip AlreadyKudoed activity; got %d POSTs", api.kudosCalls)
	}
}

// Graceful shutdown: ctx cancelled between kudos POSTs (in sleepJitter).
// processFeed must return promptly without starting additional POSTs.
func TestProcessFeed_GracefulShutdown(t *testing.T) {
	st := newRealStore(t)
	ctxSeed := context.Background()
	_ = st.UpsertAthlete(ctxSeed, 1, "A", store.RelationFollower)

	api := &fakeAPI{}
	sch := New(api, st, &fakeAuth{token: "t"}, &fakeRL{}, Config{
		MaxKudosPerPass: 5,
		// Non-zero jitter so ctx has a window to cancel in sleepJitter
		KudosJitterMin: 50 * time.Millisecond,
		KudosJitterMax: 100 * time.Millisecond,
	}, discardLogger())

	feed := make([]strava.FeedItem, 10)
	for i := range feed {
		feed[i] = strava.FeedItem{ActivityID: int64(200 + i), HasKudoed: false}
	}
	ctx, cancel := context.WithCancel(ctxSeed)
	// Cancel after ~75ms — enough for 1 POST + partial jitter wait
	go func() {
		time.Sleep(75 * time.Millisecond)
		cancel()
	}()

	athlete, _, _ := st.PickStaleAthlete(ctxSeed)
	err := sch.processFeed(ctx, "t", athlete, feed)
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if api.kudosCalls >= 5 {
		t.Errorf("shutdown should abort budget early; got %d kudos calls out of 5-budget", api.kudosCalls)
	}
}

// Idempotent kudos: when server returns 409 (already kudoed), it's still a
// success — counts against the budget, logged with status=409.
func TestProcessFeed_409IsIdempotentSuccess(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)
	_ = st.UpsertAthlete(ctx, 1, "A", store.RelationFollower)

	api := &fakeAPI{
		kudosResults: map[int64]kudosResult{
			999: {status: 409, err: nil},
		},
	}
	sch := newSchedulerForProcessFeed(t, api, st)

	feed := []strava.FeedItem{{ActivityID: 999, HasKudoed: false}}
	athlete, _, _ := st.PickStaleAthlete(ctx)
	if err := sch.processFeed(ctx, "t", athlete, feed); err != nil {
		t.Fatal(err)
	}
	var st409 int
	_ = st.DB().QueryRow(`SELECT api_status FROM kudos_log WHERE activity_id = 999`).Scan(&st409)
	if st409 != 409 {
		t.Errorf("api_status = %d, want 409", st409)
	}
}
