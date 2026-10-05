package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRunRefreshLoop_BoundsEachAttemptAndStopsOnCancel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const timeout = time.Minute
	calls := 0
	refresh := func(c context.Context) error {
		calls++
		dl, ok := c.Deadline()
		if !ok || time.Until(dl) > timeout {
			t.Errorf("refresh %d: ctx deadline %v (set=%v), want within %v", calls, dl, ok, timeout)
		}
		if calls == 3 {
			cancel()
			// Let the next tick fire too, so the loop sees ctx.Done and tick.C
			// ready at once; select picks between them at random.
			time.Sleep(5 * time.Millisecond)
		}
		return errors.New("boom")
	}

	done := make(chan struct{})
	go func() {
		runRefreshLoop(ctx, refresh, time.Millisecond, timeout, logger, "test periodic refresh")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runRefreshLoop did not return after ctx was cancelled")
	}
	if calls != 3 {
		t.Fatalf("refresh called %d times, want 3", calls)
	}
	if got := strings.Count(buf.String(), "test periodic refresh"); got != 3 {
		t.Fatalf("logged %d failures, want 3:\n%s", got, buf.String())
	}
}

func TestRefreshWithTimeout_SuccessLogsNothing(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	refreshWithTimeout(context.Background(), func(context.Context) error { return nil }, time.Second, logger, "initial refresh")
	if buf.Len() != 0 {
		t.Fatalf("successful refresh logged: %s", buf.String())
	}
}
