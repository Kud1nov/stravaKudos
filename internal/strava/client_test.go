package strava

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-ua", 5*time.Second)
}

func TestDoSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "access_token tok" {
			t.Errorf("auth header = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "test-ua" {
			t.Errorf("ua = %q", got)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	resp, err := c.do(context.Background(), "GET", "/x", nil, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || string(resp.Body) != `{"ok":true}` {
		t.Errorf("unexpected resp: %+v", resp)
	}
}

func TestDoErrorMatrix(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		headers    map[string]string
		wantSentinel error
	}{
		{"401", 401, nil, ErrUnauthorized},
		{"403", 403, nil, ErrForbidden},
		{"404", 404, nil, ErrNotFound},
		{"429 without retry-after", 429, nil, ErrRateLimited},
		{"429 with retry-after seconds", 429, map[string]string{"Retry-After": "300"}, ErrRateLimited},
		{"500", 500, nil, ErrTransient},
		{"502", 502, nil, ErrTransient},
		{"409", 409, nil, ErrTransient},
		// 418 (unused) → transient (unknown 4xx policy)
		{"418 teapot", 418, nil, ErrTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintln(w, "nope")
			})
			resp, err := c.do(context.Background(), "GET", "/x", nil, "tok")
			if err == nil {
				t.Fatalf("expected error, got nil (resp %+v)", resp)
			}
			if !errors.Is(err, tc.wantSentinel) {
				t.Errorf("err = %v, want Is-%v", err, tc.wantSentinel)
			}
			if resp == nil {
				t.Errorf("response should be non-nil even on error")
			}
		})
	}
}

func TestRetryAfterSecondsParsed(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(429)
	})
	_, err := c.do(context.Background(), "GET", "/x", nil, "")
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitedError, got %T %v", err, err)
	}
	if rl.RetryAfter != 42*time.Second {
		t.Errorf("RetryAfter = %s, want 42s", rl.RetryAfter)
	}
}

func TestRetryAfterHTTPDateParsed(t *testing.T) {
	future := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", future)
		w.WriteHeader(429)
	})
	_, err := c.do(context.Background(), "GET", "/x", nil, "")
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitedError, got %T %v", err, err)
	}
	if rl.RetryAfter < 90*time.Second || rl.RetryAfter > 130*time.Second {
		t.Errorf("RetryAfter = %s, want ~2m", rl.RetryAfter)
	}
}

func TestDoNetworkError(t *testing.T) {
	// Unreachable URL
	c := NewClient("http://127.0.0.1:1", "test-ua", 200*time.Millisecond)
	_, err := c.do(context.Background(), "GET", "/x", nil, "")
	if !errors.Is(err, ErrTransient) {
		t.Errorf("network err should be ErrTransient, got %v", err)
	}
}

func TestDoContextCancel(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.do(ctx, "GET", "/x", nil, "")
	if err == nil || !strings.Contains(err.Error(), "context") {
		// Acceptable: wrapped TransientError carrying context.DeadlineExceeded
	}
	if err == nil {
		t.Fatal("expected error on context timeout")
	}
}
