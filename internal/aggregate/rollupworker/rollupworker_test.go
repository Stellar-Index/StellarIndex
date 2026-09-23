package rollupworker

import (
	"context"
	"testing"
	"time"
)

// TestRun_RefreshesImmediately proves Run does one pass before the first
// tick, then returns ctx.Err() on cancellation.
func TestRun_RefreshesImmediately(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run does the immediate pass, then the select returns.

	err := Run(ctx, time.Hour, func(context.Context) { calls++ })
	if calls != 1 {
		t.Errorf("roll called %d times, want 1 (the immediate pass)", calls)
	}
	if err != context.Canceled {
		t.Errorf("Run err = %v, want context.Canceled", err)
	}
}

// TestRun_TicksRepeatedly proves subsequent ticks call roll again, not
// just the immediate pass.
func TestRun_TicksRepeatedly(t *testing.T) {
	calls := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, time.Millisecond, func(context.Context) {
			select {
			case calls <- struct{}{}:
			default:
			}
		})
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-calls:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for roll call %d", i+1)
		}
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Errorf("Run err = %v, want context.Canceled", err)
	}
}
