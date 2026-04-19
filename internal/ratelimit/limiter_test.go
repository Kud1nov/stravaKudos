package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func TestNewLimiterNotPaused(t *testing.T) {
	l := NewLimiter().WithClock((&fakeClock{t: time.Unix(1000, 0)}).now)
	if l.IsPaused() {
		t.Error("fresh limiter should not be paused")
	}
}

func TestPause1HourRange(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	l := NewLimiter().WithClock(fc.now)

	l.Pause1Hour("429 on feed")
	if !l.IsPaused() {
		t.Fatal("must be paused right after Pause1Hour")
	}
	if l.Reason() != "429 on feed" {
		t.Errorf("reason = %q", l.Reason())
	}

	d := l.PauseUntilTime().Sub(fc.now())
	if d < time.Hour || d > time.Hour+5*time.Minute+time.Second {
		t.Errorf("pause duration = %s, want [1h, 1h5m]", d)
	}

	// After 2h, no longer paused
	fc.advance(2 * time.Hour)
	if l.IsPaused() {
		t.Error("should not be paused after advancing 2h")
	}
}

func TestRepeatedPauseDoesNotShorten(t *testing.T) {
	fc := &fakeClock{t: time.Unix(1_000_000, 0)}
	l := NewLimiter().WithClock(fc.now)

	l.PauseUntil(fc.now().Add(2*time.Hour), "long")
	l.Pause1Hour("short") // should NOT shorten
	if l.PauseUntilTime().Sub(fc.now()) < 2*time.Hour {
		t.Error("Pause1Hour must not shorten an existing longer pause")
	}
}

func TestWaitBlocksAndReleases(t *testing.T) {
	// Use real clock for this one — we need Wait's timer to fire.
	l := NewLimiter()
	l.PauseUntil(time.Now().Add(100*time.Millisecond), "test")

	start := time.Now()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 80*time.Millisecond {
		t.Errorf("Wait returned too fast: %s", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Wait took too long: %s", elapsed)
	}
}

func TestWaitNotPausedReturnsImmediately(t *testing.T) {
	l := NewLimiter()
	start := time.Now()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Error("Wait on un-paused limiter should be immediate")
	}
}

func TestWaitCancelled(t *testing.T) {
	l := NewLimiter()
	l.PauseUntil(time.Now().Add(10*time.Second), "test")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Wait(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

func TestConcurrentPauseChecks(t *testing.T) {
	// Smoke-test for the mutex — nothing more.
	l := NewLimiter()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Pause1Hour("x")
			_ = l.IsPaused()
			_ = l.Reason()
		}()
	}
	wg.Wait()
}
