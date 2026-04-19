// Package strava talks to Strava's mobile API (m.strava.com/api/v3/...).
//
// Only the endpoints the bot actually needs are exposed. See endpoints.go.
// The taxonomy below is what the scheduler switches on — every HTTP call
// returns one of these sentinel errors (or a wrapper carrying details).
package strava

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors. Use errors.Is to check.
var (
	ErrUnauthorized = errors.New("401 Unauthorized")
	ErrRateLimited  = errors.New("429 Too Many Requests")
	ErrForbidden    = errors.New("403 Forbidden")
	ErrNotFound     = errors.New("404 Not Found")
	ErrTransient    = errors.New("transient network/server error")
)

// RateLimitedError wraps ErrRateLimited with the Retry-After duration
// (zero if the server did not provide one).
type RateLimitedError struct {
	RetryAfter time.Duration
	Body       string
}

// Error implements error.
func (e *RateLimitedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s (retry-after %s)", ErrRateLimited, e.RetryAfter)
	}
	return ErrRateLimited.Error()
}

// Unwrap satisfies errors.Is/As so callers can use errors.Is(err, ErrRateLimited).
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

// TransientError wraps ErrTransient with the original cause for debuggability.
type TransientError struct {
	Cause error
	Code  int // 0 if network-level
}

// Error implements error.
func (e *TransientError) Error() string {
	if e.Code > 0 {
		return fmt.Sprintf("%s (%d): %v", ErrTransient, e.Code, e.Cause)
	}
	return fmt.Sprintf("%s: %v", ErrTransient, e.Cause)
}

// Unwrap lets errors.Is(err, ErrTransient) match.
func (e *TransientError) Unwrap() error { return ErrTransient }
