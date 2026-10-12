package chops

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestFollowLoop_ErrorRetriesThenCancelExits pins two contracts: a transient catch-up error
// must NOT kill the loop (it retries next tick; the watermark holds, so no ledger is skipped),
// and a ctx-cancel between ticks ends the loop cleanly (return nil).
func TestFollowLoop_ErrorRetriesThenCancelExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	err := followLoop(ctx, time.Millisecond, func(context.Context, int) (bool, error) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			return false, errors.New("transient CH blip") // must be retried, not fatal
		case 4:
			cancel() // after surviving the error + a few good ticks, shut down
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("followLoop returned %v, want nil (graceful shutdown)", err)
	}
	if n := atomic.LoadInt32(&calls); n < 4 {
		t.Fatalf("catchUp called %d times, want >=4 — a transient error killed the loop", n)
	}
}

// TestFollowLoop_MidDeriveCancelIsClean: SIGTERM during a catch-up must be a clean shutdown (return nil), not a logged
// derive failure. It captures stderr so deleting the ctx.Err() shortcut in followLoop turns the test red.
func TestFollowLoop_MidDeriveCancelIsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	loopErr := followLoop(ctx, time.Hour, func(c context.Context, _ int) (bool, error) {
		cancel()              // shutdown requested mid-derive
		return false, c.Err() // catchUp returns the ctx error, as runCap67CatchUp would
	})

	_ = w.Close()
	os.Stderr = orig
	out, _ := io.ReadAll(r)

	if loopErr != nil {
		t.Fatalf("mid-derive cancel returned %v, want nil", loopErr)
	}
	if strings.Contains(string(out), "catch-up error") {
		t.Fatalf("mid-derive shutdown logged a spurious error line: %q", out)
	}
}

// TestFollowLoop_RunsCatchUpImmediately: the first catch-up fires before the first tick, so a user opening the page does
// not wait an interval. A long interval, one immediate call, then cancel proves the loop does not block on the ticker
// first.
func TestFollowLoop_RunsCatchUpImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var called int32
	err := followLoop(ctx, time.Hour, func(context.Context, int) (bool, error) {
		atomic.AddInt32(&called, 1)
		cancel()
		return true, nil
	})
	if err != nil {
		t.Fatalf("followLoop returned %v, want nil", err)
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Fatalf("catchUp called %d times, want exactly 1 (immediate, pre-tick)", called)
	}
}

// Pins the idle count handed to each tick: consecutive idle ticks, reset by a tick that worked, kept by one that errored.
// A miscount would hide a stall or back off a live feed. No timing asserted; policy is TestFollowTick.
func TestFollowLoop_IdleAccounting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// What each tick reports back, in order; the loop's idle count on
	// entry to each tick is recorded and compared afterwards.
	script := []struct {
		worked bool
		err    error
	}{
		{worked: false},                // idle 0 → 1
		{worked: false},                // idle 1 → 2
		{err: errors.New("transient")}, // idle 2 → 2 (an error is neither)
		{worked: false},                // idle 2 → 3
		{worked: true},                 // idle 3 → 0
		{worked: false},                // idle 0 → 1
		{worked: true},                 // idle 1 → 0
		{worked: false},                // last tick: cancels
	}
	wantIdle := []int{0, 1, 2, 2, 3, 0, 1, 0}

	var got []int
	err := followLoop(ctx, time.Millisecond, func(_ context.Context, idle int) (bool, error) {
		got = append(got, idle)
		step := script[len(got)-1]
		if len(got) == len(script) {
			cancel()
		}
		return step.worked, step.err
	})
	if err != nil {
		t.Fatalf("followLoop returned %v, want nil", err)
	}
	if len(got) != len(wantIdle) {
		t.Fatalf("catchUp called %d times, want %d", len(got), len(wantIdle))
	}
	for i := range wantIdle {
		if got[i] != wantIdle[i] {
			t.Errorf("tick %d saw idle=%d, want %d (sequence %v)", i+1, got[i], wantIdle[i], got)
		}
	}
}
