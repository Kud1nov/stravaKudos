package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akudinov/stravakudos/internal/ratelimit"
	"github.com/akudinov/stravakudos/internal/scheduler"
	"github.com/akudinov/stravakudos/internal/store"
	"github.com/akudinov/stravakudos/internal/strava"
)

// stravaMock is a single httptest.Server covering every endpoint the bot
// uses. Tests tweak its behaviour by mutating fields before calling Run.
type stravaMock struct {
	mu sync.Mutex

	athleteID   int64
	followers   []strava.Athlete
	friends     []strava.Athlete
	feedByID    map[int64][]strava.FeedItem
	kudosFor429 map[int64]bool

	kudosCalls atomic.Int64
	feedCalls  atomic.Int64
}

func newStravaMock() *stravaMock {
	return &stravaMock{
		athleteID: 42,
		followers: []strava.Athlete{{ID: 1, Firstname: "Alice", Lastname: "A"}},
		friends:   []strava.Athlete{{ID: 2, Firstname: "Bob", Lastname: "B"}},
		feedByID: map[int64][]strava.FeedItem{
			1: {
				{ActivityID: 1001, HasKudoed: true},
				{ActivityID: 1002, HasKudoed: false},
			},
			2: {
				{ActivityID: 2001, HasKudoed: false},
			},
		},
	}
}

func (m *stravaMock) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()

		switch {
		case r.URL.Path == "/api/v3/oauth/internal/token":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"access_token":"srv-tok"}`))

		case r.URL.Path == "/api/v3/athlete":
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(strava.Athlete{ID: m.athleteID})

		case r.URL.Path == "/api/v3/athletes/42/followers":
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(m.followers)

		case r.URL.Path == "/api/v3/athletes/42/friends":
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(m.friends)

		case r.URL.Path == "/api/v3/feed/athlete/1" || r.URL.Path == "/api/v3/feed/athlete/2":
			m.feedCalls.Add(1)
			var id int64
			if r.URL.Path == "/api/v3/feed/athlete/1" {
				id = 1
			} else {
				id = 2
			}
			out := make([]map[string]any, 0, len(m.feedByID[id]))
			for _, it := range m.feedByID[id] {
				out = append(out, map[string]any{
					"entity_id": it.ActivityID,
					"item":      map[string]any{"has_kudoed": it.HasKudoed},
				})
			}
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(out)

		case len(r.URL.Path) > len("/api/v3/activities/") && r.URL.Path[:19] == "/api/v3/activities/":
			m.kudosCalls.Add(1)
			w.WriteHeader(201)

		default:
			http.NotFound(w, r)
		}
	}))
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestEndToEnd drives the full wiring: store + strava client + scheduler +
// auth manager against a mock server. Runs for enough time to observe
// roster refresh + a couple of feed ticks + at least one kudos POST.
func TestEndToEnd(t *testing.T) {
	mock := newStravaMock()
	srv := mock.serve()
	defer srv.Close()

	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	client := strava.NewClient(srv.URL, "test-ua", 3*time.Second)
	adapter := &tokenStoreAdapter{s: s}
	auth := strava.NewAuthManager(client, adapter, "secret", "u@x", "p", discardLogger())

	rl := ratelimit.NewLimiter()
	sch := scheduler.New(client, s, auth, rl, scheduler.Config{
		ScanInterval:          100 * time.Millisecond,
		RosterRefreshInterval: 10 * time.Second,
		MaxKudosPerPass:       5,
		KudosJitterMin:        0,
		KudosJitterMax:        0,
	}, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()
	<-done

	// Assertions:
	// - Both athletes (1 follower, 2 friend) are in athletes table
	var athleteCount int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM athletes`).Scan(&athleteCount)
	if athleteCount != 2 {
		t.Errorf("athletes = %d, want 2", athleteCount)
	}

	// - At least one kudos was posted (activity 1002 or 2001)
	var posted int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM kudos_log WHERE api_status = 201`).Scan(&posted)
	if posted == 0 {
		t.Errorf("no POSTed kudos; kudosCalls=%d feedCalls=%d",
			mock.kudosCalls.Load(), mock.feedCalls.Load())
	}
	// - activity 1001 should be bootstrapped (has_kudoed=true) — never POSTed
	var bootstrap int
	_ = s.DB().QueryRow(`SELECT api_status FROM kudos_log WHERE activity_id = 1001`).Scan(&bootstrap)
	if bootstrap != 0 {
		t.Errorf("activity 1001 api_status = %d, want 0 (bootstrap)", bootstrap)
	}
	// - api_calls has multiple rows (feed + roster-related)
	var apiCalls int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM api_calls`).Scan(&apiCalls)
	if apiCalls < 3 {
		t.Errorf("api_calls = %d, want ≥ 3 (profile+followers+friends at minimum)", apiCalls)
	}
}

// TestGracefulShutdown: Run returns within a short window after ctx cancel,
// store.Close() succeeds, no deadlock.
func TestGracefulShutdown(t *testing.T) {
	mock := newStravaMock()
	srv := mock.serve()
	defer srv.Close()

	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	client := strava.NewClient(srv.URL, "ua", 3*time.Second)
	adapter := &tokenStoreAdapter{s: s}
	auth := strava.NewAuthManager(client, adapter, "secret", "u", "p", discardLogger())
	rl := ratelimit.NewLimiter()
	sch := scheduler.New(client, s, auth, rl, scheduler.Config{
		ScanInterval:          5 * time.Second, // large so we don't burst during shutdown
		RosterRefreshInterval: 24 * time.Hour,
		MaxKudosPerPass:       5,
	}, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()

	time.Sleep(100 * time.Millisecond) // let initial refresh run
	shutdownStart := time.Now()
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of cancel — deadlock?")
	}
	if d := time.Since(shutdownStart); d > 2*time.Second {
		t.Errorf("shutdown took %s, expected < 2s", d)
	}
	if err := s.Close(); err != nil {
		t.Errorf("store.Close after shutdown: %v", err)
	}
}

// TestStatusSubcommand — smoke test the -status path.
func TestStatusSubcommand(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// Empty DB → status should exit 1 (no recent kudos)
	// We can't call runStatus and os.Exit, but we can check the return value.
	code := runStatus(s)
	if code != 1 {
		t.Errorf("status on empty DB = %d, want 1", code)
	}

	// After a recent POST, code should be 0
	ctx := context.Background()
	_ = s.LogKudos(ctx, 500, 1, 201)
	code = runStatus(s)
	if code != 0 {
		t.Errorf("status with fresh kudos = %d, want 0", code)
	}
}

// Use of os.Stdout/Stderr in runStatus is fine for tests — we just don't
// assert on its content. If needed later, swap to a configurable writer.
var _ = os.Stdout
