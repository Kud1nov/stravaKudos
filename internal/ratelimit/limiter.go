// Package ratelimit is a global pause gate. When tripped (by a 429), every
// subsequent Wait() blocks until the pause window passes. One knob: how long
// to pause. No token-bucket, no per-endpoint — the thin-drip scheduler at
// 1 request per 90s already keeps us an order of magnitude under any quota;
// this gate is the seatbelt for when Strava changes the rules under us.
package ratelimit

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Limiter is a single global pause window.
type Limiter struct {
	mu         sync.Mutex
	pauseUntil time.Time
	reason     string

	now func() time.Time
}

// NewLimiter returns an un-paused limiter using time.Now.
func NewLimiter() *Limiter { return &Limiter{now: time.Now} }

// WithClock replaces the time source (used by tests).
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
	return l
}

// PauseUntil sets an absolute pause deadline, annotated with reason for logs.
func (l *Limiter) PauseUntil(t time.Time, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pauseUntil = t
	l.reason = reason
}

// Pause1Hour is the default 429 response: 1h + random 0-5min jitter,
// always pushing out existing pause (never shortens).
func (l *Limiter) Pause1Hour(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	candidate := l.now().Add(time.Hour + time.Duration(rand.Int64N(int64(5*time.Minute))))
	if candidate.After(l.pauseUntil) {
		l.pauseUntil = candidate
		l.reason = reason
	}
}

// IsPaused reports whether Wait would block at this instant.
func (l *Limiter) IsPaused() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.now().Before(l.pauseUntil)
}

// Reason returns the annotation from the last Pause call.
func (l *Limiter) Reason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reason
}

// PauseUntilTime returns the deadline (for logs/tests).
func (l *Limiter) PauseUntilTime() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pauseUntil
}

// Wait blocks until the pause window expires OR ctx is cancelled. Returns
// ctx.Err() on cancel, nil otherwise. Returns immediately if not paused.
func (l *Limiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	remaining := l.pauseUntil.Sub(l.now())
	l.mu.Unlock()
	if remaining <= 0 {
		return nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
