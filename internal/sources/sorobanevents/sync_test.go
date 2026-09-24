package sorobanevents

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Sync is the projector's soroban_events completeness barrier: it must not
// return while a row PushEvent already accepted is still unwritten, and must
// return once it lands.
func TestAsyncSink_SyncWaitsForAcceptedRowsToCommit(t *testing.T) {
	w := newBlockableWriter()
	s := NewAsyncSink(w, AsyncSinkOptions{BatchSize: 1, FlushInterval: 10 * time.Millisecond})
	s.Start()
	defer s.Stop()

	for l := uint32(1); l <= 3; l++ {
		s.PushEvent(captureableEvent(t, l))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := s.Sync(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sync with 3 unwritten rows = %v, want context.DeadlineExceeded", err)
	}

	close(w.release)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("Sync after the writer released = %v, want nil", err)
	}
	if got := w.WrittenRows(); got != 3 {
		t.Errorf("rows written when Sync returned = %d, want 3", got)
	}
}

// An idle sink has nothing to wait for.
func TestAsyncSink_SyncIdleReturnsImmediately(t *testing.T) {
	s := NewAsyncSink(newBlockableWriter(), AsyncSinkOptions{})
	s.Start()
	defer s.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("Sync on an idle sink = %v, want nil", err)
	}
}
