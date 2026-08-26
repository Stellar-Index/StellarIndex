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

// TestFollowLoop_ErrorRetriesThenCancelExits pins the daemon's two load-bearing
// contracts: a transient catch-up error must NOT kill the loop (it retries on
// the next tick — the watermark holds, so no ledger is skipped), and a
// ctx-cancel between ticks must end the loop cleanly (return nil). This is the
// real-time movement feed's resilience guarantee, testable without ClickHouse.
func TestFollowLoop_ErrorRetriesThenCancelExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	err := followLoop(ctx, time.Millisecond, func(context.Context) error {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			return errors.New("transient CH blip") // must be retried, not fatal
		case 4:
			cancel() // after surviving the error + a few good ticks, shut down
		}
		return nil
	})
	if err != nil {
		t.Fatalf("followLoop returned %v, want nil (graceful shutdown)", err)
	}
	if n := atomic.LoadInt32(&calls); n < 4 {
		t.Fatalf("catchUp called %d times, want >=4 — a transient error killed the loop", n)
	}
}

// TestFollowLoop_MidDeriveCancelIsClean: if SIGTERM lands WHILE a catch-up is
// running, catchUp surfaces the ctx error; the loop must treat that as a clean
// shutdown (return nil) AND NOT log it as a derive failure. The `ctx.Err()`
// shortcut in followLoop exists precisely to keep a shutdown out of the error
// log — without it the loop logs a misleading "catch-up error ... retrying"
// line on every SIGTERM. This captures stderr to make that guard non-vacuous:
// delete the shortcut and this test goes red on the spurious log.
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

	loopErr := followLoop(ctx, time.Hour, func(c context.Context) error {
		cancel()       // shutdown requested mid-derive
		return c.Err() // catchUp returns the ctx error, as runCap67CatchUp would
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

// TestFollowLoop_RunsCatchUpImmediately: the first catch-up must fire before the
// first tick elapses (a user opening the page shouldn't wait a full interval for
// the feed to advance). With a long interval, a single immediate call then a
// cancel proves the loop doesn't block on the ticker first.
func TestFollowLoop_RunsCatchUpImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var called int32
	err := followLoop(ctx, time.Hour, func(context.Context) error {
		atomic.AddInt32(&called, 1)
		cancel()
		return nil
	})
	if err != nil {
		t.Fatalf("followLoop returned %v, want nil", err)
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Fatalf("catchUp called %d times, want exactly 1 (immediate, pre-tick)", called)
	}
}
