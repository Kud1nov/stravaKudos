package strava

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client is a stateless HTTP client for Strava's mobile API.
//
// No per-request state lives on the struct — safe for concurrent use.
// In practice the scheduler is sequential; this just protects future-us.
type Client struct {
	baseURL   string
	userAgent string
	http      *http.Client
}

// NewClient constructs a Client. timeout applies per request; 30s is a sane
// default given Strava responses are typically <1s.
func NewClient(baseURL, userAgent string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		userAgent: userAgent,
		http: &http.Client{
			Timeout: timeout,
			// default transport fine; connections pool automatically
		},
	}
}

// Response is the normalised shape returned by do: body already read,
// along with status code and a subset of headers the caller may need.
type Response struct {
	StatusCode int
	Body       []byte
	Header     http.Header
}

// do performs the request and translates errors according to our taxonomy.
// On success (2xx) returns (*Response, nil). On error, the *Response may be
// nil or partial — callers who need the body for error diagnostics should
// always check before dereferencing.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, token string) (*Response, error) {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	if token != "" {
		// NOTE: mobile API uses "access_token <t>", NOT "Bearer <t>".
		req.Header.Set("Authorization", "access_token "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// network-level failure — always transient
		return nil, &TransientError{Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()

	// Always read body — we need it for error diagnostics and successful parsing
	payload, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, &TransientError{Cause: readErr, Code: resp.StatusCode}
	}

	r := &Response{
		StatusCode: resp.StatusCode,
		Body:       payload,
		Header:     resp.Header,
	}

	// 2xx: success
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return r, nil
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return r, ErrUnauthorized
	case http.StatusForbidden:
		return r, ErrForbidden
	case http.StatusNotFound:
		return r, ErrNotFound
	case http.StatusTooManyRequests:
		return r, &RateLimitedError{
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       string(payload),
		}
	}
	// 409 Conflict is a quirk of POST kudos (already kudoed); leave to endpoints to decide
	// 400s other than the above we treat as transient — Strava occasionally sends 502/504 too
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusConflict {
		return r, &TransientError{Code: resp.StatusCode, Cause: errors.New(string(payload))}
	}
	// Unknown 4xx — don't retry, but don't classify as fatal either. Treat as transient
	// so a one-off weird response doesn't kill the scheduler's view of a healthy athlete.
	return r, &TransientError{Code: resp.StatusCode, Cause: errors.New(string(payload))}
}

func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	// Seconds form
	if n, err := strconv.Atoi(h); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	// HTTP-date form
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}
