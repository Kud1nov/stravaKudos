package strava

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

// TokenStore is the subset of store.Store that AuthManager needs.
// Narrow interface → easy to mock for tests, no package cycle.
type TokenStore interface {
	GetToken(ctx context.Context) (*StoredToken, error)
	SetToken(ctx context.Context, accessToken string, expiresAt time.Time) error
	ClearToken(ctx context.Context) error
}

// StoredToken mirrors store.TokenRecord to avoid an import cycle. The store
// package depends on nothing; this package depends on store only implicitly
// through the interface.
type StoredToken struct {
	AccessToken string
	ObtainedAt  time.Time
	ExpiresAt   time.Time
}

// AuthManager owns the token lifecycle.
type AuthManager struct {
	store        TokenStore
	client       *Client
	clientSecret string
	email        string
	password     string
	log          *slog.Logger
}

// NewAuthManager constructs one. Pass a real *slog.Logger or slog.Default().
func NewAuthManager(client *Client, tokenStore TokenStore, clientSecret, email, password string, logger *slog.Logger) *AuthManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &AuthManager{
		store:        tokenStore,
		client:       client,
		clientSecret: clientSecret,
		email:        email,
		password:     password,
		log:          logger,
	}
}

// Ensure returns a valid access_token. If none is stored, logs in via
// password-grant and persists the new token. Safe to call multiple times.
func (a *AuthManager) Ensure(ctx context.Context) (string, error) {
	tok, err := a.store.GetToken(ctx)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	if tok != nil && tok.AccessToken != "" {
		// Token TTL: the mobile API has historically been effectively infinite
		// (last re-auth on production was 2020-02). We trust stored tokens
		// unless expires_at is set AND already past.
		if tok.ExpiresAt.IsZero() || tok.ExpiresAt.After(time.Now()) {
			return tok.AccessToken, nil
		}
		a.log.Info("stored token expired, re-authenticating", "expired_at", tok.ExpiresAt)
	}
	return a.login(ctx)
}

// Reauth invalidates the stored token and forces a fresh password-grant login.
// Called by the scheduler when it sees a 401.
func (a *AuthManager) Reauth(ctx context.Context) (string, error) {
	if err := a.store.ClearToken(ctx); err != nil {
		return "", fmt.Errorf("clear token: %w", err)
	}
	return a.login(ctx)
}

func (a *AuthManager) login(ctx context.Context) (string, error) {
	a.log.Info("password-grant login")
	resp, err := a.client.Auth(ctx, a.clientSecret, a.email, a.password)
	if err != nil {
		return "", fmt.Errorf("password-grant: %w", err)
	}
	// Mobile API does not return expires_at. Pass zero → store NULL.
	if err := a.store.SetToken(ctx, resp.AccessToken, time.Time{}); err != nil {
		return "", fmt.Errorf("save token: %w", err)
	}
	a.log.Info("login ok, token stored")
	return resp.AccessToken, nil
}

// ImportInitialToken reads a raw token from path (the legacy
// .strava-auth-token file copied from the v1 deployment), stores it, and
// deletes the file. No-op if the file is absent.
func (a *AuthManager) ImportInitialToken(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open initial token: %w", err)
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read initial token: %w", err)
	}
	token := string(raw)
	// Trim surrounding whitespace/newlines that may survive the file copy.
	for len(token) > 0 && (token[len(token)-1] == '\n' || token[len(token)-1] == ' ' || token[len(token)-1] == '\r') {
		token = token[:len(token)-1]
	}
	if token == "" {
		a.log.Warn("initial token file is empty, ignoring", "path", path)
		return nil
	}
	if err := a.store.SetToken(ctx, token, time.Time{}); err != nil {
		return fmt.Errorf("save imported token: %w", err)
	}
	// Remove the file so we don't re-import on the next boot.
	if err := os.Remove(path); err != nil {
		a.log.Warn("could not remove initial token file", "path", path, "err", err)
	}
	a.log.Info("imported initial token from file", "path", path)
	return nil
}
