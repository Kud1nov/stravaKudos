package strava

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// memTokenStore is a minimal in-memory TokenStore for tests.
type memTokenStore struct {
	mu       sync.Mutex
	token    *StoredToken
	setCalls int
	clearCalls int
	readCalls int
}

func (m *memTokenStore) GetToken(ctx context.Context) (*StoredToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readCalls++
	if m.token == nil {
		return nil, nil
	}
	cp := *m.token
	return &cp, nil
}

func (m *memTokenStore) SetToken(ctx context.Context, t string, exp time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setCalls++
	m.token = &StoredToken{AccessToken: t, ObtainedAt: time.Now(), ExpiresAt: exp}
	return nil
}

func (m *memTokenStore) ClearToken(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCalls++
	m.token = nil
	return nil
}

// authServer responds to /api/v3/oauth/internal/token with the given token
// (or 401 if empty).
func authServer(t *testing.T, newToken string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/oauth/internal/token" {
			http.NotFound(w, r)
			return
		}
		if newToken == "" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"access_token":"` + newToken + `"}`))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "ua", 3*time.Second)
}

func TestEnsureNoStoredTokenDoesPasswordGrant(t *testing.T) {
	ctx := context.Background()
	st := &memTokenStore{}
	c := authServer(t, "fresh-tok")
	am := NewAuthManager(c, st, "secret", "u@x", "p", nil)

	tok, err := am.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "fresh-tok" {
		t.Errorf("got %q", tok)
	}
	if st.setCalls != 1 {
		t.Errorf("SetToken calls = %d, want 1", st.setCalls)
	}
}

func TestEnsureWithStoredTokenSkipsNetwork(t *testing.T) {
	ctx := context.Background()
	st := &memTokenStore{}
	_ = st.SetToken(ctx, "cached", time.Time{})
	st.setCalls = 0 // reset after seeding

	// Server that would 500 if asked — proves Ensure didn't call it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected network call: %s", r.URL.Path)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "ua", time.Second)
	am := NewAuthManager(c, st, "s", "e", "p", nil)

	tok, err := am.Ensure(ctx)
	if err != nil || tok != "cached" {
		t.Fatalf("got %q/%v, want 'cached'", tok, err)
	}
	if st.setCalls != 0 {
		t.Error("Ensure should not call SetToken when token is fresh")
	}
}

func TestReauthClearsAndRefreshes(t *testing.T) {
	ctx := context.Background()
	st := &memTokenStore{}
	_ = st.SetToken(ctx, "old", time.Time{})
	st.setCalls, st.clearCalls = 0, 0

	c := authServer(t, "reauth-tok")
	am := NewAuthManager(c, st, "s", "e", "p", nil)

	tok, err := am.Reauth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "reauth-tok" {
		t.Errorf("token = %q", tok)
	}
	if st.clearCalls != 1 || st.setCalls != 1 {
		t.Errorf("clear=%d set=%d; want 1/1", st.clearCalls, st.setCalls)
	}
}

func TestEnsureReturnsErrorOn401(t *testing.T) {
	ctx := context.Background()
	st := &memTokenStore{}
	c := authServer(t, "") // 401 on auth
	am := NewAuthManager(c, st, "s", "e", "p", nil)

	_, err := am.Ensure(ctx)
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized chain, got %v", err)
	}
	if st.setCalls != 0 {
		t.Error("failed login must not set token")
	}
}

func TestImportInitialTokenPresent(t *testing.T) {
	ctx := context.Background()
	st := &memTokenStore{}
	am := NewAuthManager(nil, st, "s", "e", "p", nil) // client not needed for import

	dir := t.TempDir()
	path := filepath.Join(dir, "initial-token")
	if err := os.WriteFile(path, []byte("imported-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := am.ImportInitialToken(ctx, path); err != nil {
		t.Fatal(err)
	}
	if st.token == nil || st.token.AccessToken != "imported-tok" {
		t.Errorf("stored token = %+v", st.token)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("initial token file must be deleted after import")
	}
}

func TestImportInitialTokenAbsent(t *testing.T) {
	ctx := context.Background()
	st := &memTokenStore{}
	am := NewAuthManager(nil, st, "s", "e", "p", nil)

	// Non-existent path — must be no-op, no error.
	if err := am.ImportInitialToken(ctx, "/nonexistent/initial-token"); err != nil {
		t.Error(err)
	}
	if st.token != nil {
		t.Error("store should remain untouched")
	}
}

func TestImportInitialTokenEmptyFile(t *testing.T) {
	ctx := context.Background()
	st := &memTokenStore{}
	am := NewAuthManager(nil, st, "s", "e", "p", nil)

	dir := t.TempDir()
	path := filepath.Join(dir, "t")
	_ = os.WriteFile(path, []byte("   \n"), 0o600)

	if err := am.ImportInitialToken(ctx, path); err != nil {
		t.Fatal(err)
	}
	if st.token != nil {
		t.Error("empty file should not populate token")
	}
}
