package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeRoutedViaTagger records every TagTradesRoutedVia call.
type fakeRoutedViaTagger struct {
	mu    sync.Mutex
	calls []taggerCall
	ret   int64
	err   error
}

type taggerCall struct {
	router, source string
	from, to       time.Time
}

func (f *fakeRoutedViaTagger) TagTradesRoutedVia(_ context.Context, routerName, tradeSource string, from, to time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, taggerCall{router: routerName, source: tradeSource, from: from, to: to})
	return f.ret, f.err
}

func (f *fakeRoutedViaTagger) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRoutedViaTagger) call(i int) taggerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// TestRunRoutedViaTagger_SweepArgs pins the sweeper's tagging
// policy: routed_via value is the soroswap-router source name, the
// trade scope is source='soroswap', and the window is
// [now-lookback, now]. A drift in any of these silently breaks
// attribution (wrong tag, wrong protocol scoped, or a window that
// misses the projector lag it exists to cover).
func TestRunRoutedViaTagger_SweepArgs(t *testing.T) {
	fake := &fakeRoutedViaTagger{ret: 3}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Long interval: only the immediate boot sweep + the
		// shutdown flush sweep fire during the test.
		RunRoutedViaTagger(ctx, nil, fake, time.Hour, 30*time.Minute)
	}()

	// The boot sweep is synchronous before the ticker loop; poll
	// briefly for it.
	deadline := time.Now().Add(2 * time.Second)
	for fake.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fake.callCount() == 0 {
		t.Fatal("boot sweep never fired")
	}

	got := fake.call(0)
	if got.router != "soroswap-router" {
		t.Errorf("router tag = %q, want %q", got.router, "soroswap-router")
	}
	if got.source != "soroswap" {
		t.Errorf("trade source scope = %q, want %q", got.source, "soroswap")
	}
	window := got.to.Sub(got.from)
	if window != 30*time.Minute {
		t.Errorf("sweep window = %v, want 30m", window)
	}
	if got.to.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("sweep window end %v is in the future", got.to)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper did not stop on ctx cancel")
	}

	// Shutdown path performs one final flush sweep.
	if n := fake.callCount(); n < 2 {
		t.Errorf("expected boot + shutdown sweeps, got %d call(s)", n)
	}
}

// TestRunRoutedViaTagger_DefaultsAndErrorTolerance verifies that
// non-positive interval/lookback fall back to package defaults and
// that a failing store does not wedge or crash the loop.
func TestRunRoutedViaTagger_DefaultsAndErrorTolerance(t *testing.T) {
	fake := &fakeRoutedViaTagger{err: context.DeadlineExceeded}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunRoutedViaTagger(ctx, nil, fake, -1, -1)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for fake.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fake.callCount() == 0 {
		t.Fatal("boot sweep never fired")
	}
	got := fake.call(0)
	if window := got.to.Sub(got.from); window != routedViaSweepLookback {
		t.Errorf("default lookback = %v, want %v", window, routedViaSweepLookback)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper did not stop on ctx cancel after store errors")
	}
}
