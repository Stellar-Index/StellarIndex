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

// CA2-A30-harden-2 supersedes K012's "contain and keep running" contract for
// this one sink. K012 treated the drain worker as an ordinary detached
// background worker whose halt only stops its own work — right for most
// callers of worker.Recover, wrong here: the dispatcher calls PushEvent
// SYNCHRONOUSLY on its hot path (internal/dispatcher/dispatcher.go), so a
// contained-and-dead drain leaves every source's PushEvent blocked forever
// on the full channel, with no restart and no operator recourse beyond a
// manual unit restart per the runbook. run() must instead let the panic
// propagate so the process crashes and its systemd unit (Restart=always)
// restarts it from the durable ledger cursor.
//
// This still can't call sink.Start()+Stop(): that would crash the whole
// test binary, taking every other test down with it. Calling run()
// directly, inside a goroutine that recovers it itself, observes the same
// unwind a real supervisor would see without losing the test process.
func TestAsyncSink_PanickingWriterCrashesRatherThanWedges(t *testing.T) {
	const workerName = "soroban-events-sink-drain"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	sink := NewAsyncSink(panickingWriter{}, AsyncSinkOptions{
		BufferSize:    4,
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})
	sink.PushEvent(captureableEvent(t, 1)) // buffered; BatchSize=1 flushes it as soon as run() reads it

	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		sink.run()
	}()

	select {
	case r := <-recovered:
		if r == nil {
			t.Fatal("run() returned without panicking: the drain panic was contained instead of " +
				"propagating, so PushEvent's channel is left undrained and every producer wedges forever")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() neither panicked nor returned: the drain deadlocked")
	}

	select {
	case <-sink.done:
	default:
		t.Fatal("s.done was not closed even though run() unwound via panic — Stop() would block forever on it")
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v — the panic must still "+
			"be reported before it propagates, so the existing page fires either way",
			workerName, after, before+1)
	}
}
