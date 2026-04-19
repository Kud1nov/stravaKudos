package strava

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// mux-based test server: route table. Avoids regex for path matching.
type routeKey struct{ method, path string }
type routeVal struct {
	status int
	body   []byte
	header http.Header
}

func newRoutedServer(t *testing.T, routes map[routeKey]routeVal) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := routeKey{method: r.Method, path: r.URL.Path}
		val, ok := routes[key]
		if !ok {
			t.Logf("unmatched request: %s %s (query=%s)", r.Method, r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
			return
		}
		for k, v := range val.header {
			for _, vv := range v {
				w.Header().Add(k, vv)
			}
		}
		if val.status == 0 {
			val.status = 200
		}
		w.WriteHeader(val.status)
		_, _ = w.Write(val.body)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-ua", 5*time.Second)
}

func TestGetProfile(t *testing.T) {
	c := newRoutedServer(t, map[routeKey]routeVal{
		{"GET", "/api/v3/athlete"}: {body: loadFixture(t, "athlete.json")},
	})
	id, err := c.GetProfile(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if id != 42798201 {
		t.Errorf("id = %d", id)
	}
}

func TestGetFollowers(t *testing.T) {
	c := newRoutedServer(t, map[routeKey]routeVal{
		{"GET", "/api/v3/athletes/42/followers"}: {body: loadFixture(t, "followers.json")},
	})
	list, err := c.GetFollowers(context.Background(), "tok", 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d", len(list))
	}
	if list[0].ID != 1001 || list[0].DisplayName() != "Alice A" {
		t.Errorf("unexpected first: %+v", list[0])
	}
}

func TestGetFriends(t *testing.T) {
	c := newRoutedServer(t, map[routeKey]routeVal{
		{"GET", "/api/v3/athletes/42/friends"}: {body: loadFixture(t, "friends.json")},
	})
	list, err := c.GetFriends(context.Background(), "tok", 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[1].ID != 2001 {
		t.Errorf("unexpected: %+v", list)
	}
}

func TestGetFeedSkipsNonActivityCards(t *testing.T) {
	c := newRoutedServer(t, map[routeKey]routeVal{
		{"GET", "/api/v3/feed/athlete/42"}: {body: loadFixture(t, "feed_mixed.json")},
	})
	items, err := c.GetFeed(context.Background(), "tok", 42)
	if err != nil {
		t.Fatal(err)
	}
	// Expected: 3 real items (500001 has_kudoed=true, 500002 false, 500003 false).
	// entity_id=0 dropped, 500004 (no has_kudoed) dropped.
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3: %+v", len(items), items)
	}
	wantIDs := []int64{500001, 500002, 500003}
	for i, w := range wantIDs {
		if items[i].ActivityID != w {
			t.Errorf("items[%d].ActivityID = %d, want %d", i, items[i].ActivityID, w)
		}
	}
	if !items[0].HasKudoed || items[1].HasKudoed || items[2].HasKudoed {
		t.Errorf("unexpected has_kudoed flags: %+v", items)
	}
}

func TestPostKudosIdempotent(t *testing.T) {
	cases := []struct {
		name   string
		status int
		wantErr bool
	}{
		{"201 created", 201, false},
		{"200 already", 200, false},
		{"409 conflict", 409, false},
		{"500 transient", 500, true},
		{"401 unauthorized", 401, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newRoutedServer(t, map[routeKey]routeVal{
				{"POST", "/api/v3/activities/99/kudos"}: {status: tc.status},
			})
			status, err := c.PostKudos(context.Background(), "tok", 99)
			if status != tc.status {
				t.Errorf("status = %d, want %d", status, tc.status)
			}
			if tc.wantErr && err == nil {
				t.Errorf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected err: %v", err)
			}
		})
	}
}

func TestAuthSuccess(t *testing.T) {
	c := newRoutedServer(t, map[routeKey]routeVal{
		{"POST", "/api/v3/oauth/internal/token"}: {body: []byte(`{"access_token":"new-tok"}`)},
	})
	r, err := c.Auth(context.Background(), "secret", "u@x", "p")
	if err != nil {
		t.Fatal(err)
	}
	if r.AccessToken != "new-tok" {
		t.Errorf("token = %q", r.AccessToken)
	}
}

func TestAuthSendsCredentials(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"access_token":"t"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "ua", 5*time.Second)
	_, _ = c.Auth(context.Background(), "secret-value", "u@x", "p123")

	if captured["client_id"].(float64) != 2 {
		t.Errorf("client_id = %v, want 2", captured["client_id"])
	}
	if captured["client_secret"] != "secret-value" || captured["email"] != "u@x" || captured["password"] != "p123" {
		t.Errorf("body incorrect: %+v", captured)
	}
}

func TestAuthEmptyTokenError(t *testing.T) {
	c := newRoutedServer(t, map[routeKey]routeVal{
		{"POST", "/api/v3/oauth/internal/token"}: {body: []byte(`{"access_token":""}`)},
	})
	_, err := c.Auth(context.Background(), "s", "e", "p")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty-token error, got %v", err)
	}
}

func TestAuth401(t *testing.T) {
	c := newRoutedServer(t, map[routeKey]routeVal{
		{"POST", "/api/v3/oauth/internal/token"}: {status: 401, body: []byte(`{"error":"bad creds"}`)},
	})
	_, err := c.Auth(context.Background(), "s", "e", "p")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}

func TestEndpointsPassThrough429(t *testing.T) {
	c := newRoutedServer(t, map[routeKey]routeVal{
		{"GET", "/api/v3/feed/athlete/42"}: {status: 429, header: http.Header{"Retry-After": {"60"}}},
	})
	_, err := c.GetFeed(context.Background(), "tok", 42)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != 60*time.Second {
		t.Errorf("unexpected rate-limit: %v", err)
	}
}
