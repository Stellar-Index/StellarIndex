package sorobanevents

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// panickingWriter is a BatchWriter that has gone wrong in a way no error
// return describes — a nil pool, a driver bug, a bad type assertion.
type panickingWriter struct{}

func (panickingWriter) InsertSorobanEventsBatch(context.Context, []Row) error {
	panic("batch writer blew up")
}

// K012 — the landing-zone sink's drain worker is a detached goroutine, and
// an unrecovered panic in ANY goroutine terminates the WHOLE process, not
// just the worker. Without the guard this test does not fail, it CRASHES
// the test binary; that crash is the production harm.
//
// Containment alone is not enough: Stop() blocks on s.done, so a recover
// that did not keep close(s.done) as the outermost defer would trade a
// crash for a shutdown that never returns.
func TestAsyncSink_PanickingWriterDoesNotKillTheProcess(t *testing.T) {
	const workerName = "soroban-events-sink-drain"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	sink := NewAsyncSink(panickingWriter{}, AsyncSinkOptions{
		BufferSize:    4,
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})
	sink.Start()
	sink.PushEvent(captureableEvent(t, 1))

	stopped := make(chan struct{})
	go func() {
		sink.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() never returned: the drain worker contained its panic without " +
			"closing done, so every shutdown blocks on a worker that is already gone")
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v — a recover "+
			"that does not move the counter turns a loud crash into a silent dead worker",
			workerName, after, before+1)
	}
}
