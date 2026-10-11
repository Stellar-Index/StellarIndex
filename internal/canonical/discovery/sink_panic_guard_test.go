package discovery_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// panickingRecorder is the shape of a Recorder that has gone wrong in a way
// no error return describes — a nil map write, a slice bound, a driver bug.
type panickingRecorder struct{}

func (panickingRecorder) Record(context.Context, discovery.Hit) error {
	panic("recorder blew up")
}

func (panickingRecorder) IsKnown(context.Context, string) (bool, error) {
	return false, nil
}

// AsyncSink's drain worker is a detached goroutine, and an unrecovered panic in ANY goroutine
// terminates the WHOLE process (stellarindex-api and stellarindex-indexer alike). Without the
// guard this test CRASHES the test binary, which is the production harm.
//
// Containment alone is not enough: run() closes s.done and Stop() blocks on it, so a recover
// that did not keep close(s.done) as the outermost defer would trade a crash for a shutdown
// that never returns.
func TestAsyncSink_PanickingRecorderDoesNotKillTheProcess(t *testing.T) {
	const workerName = "discovery-async-sink-drain"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	sink := discovery.NewAsyncSink(panickingRecorder{}, discovery.AsyncSinkOptions{BufferSize: 4})
	sink.Start()
	sink.Push(discovery.Hit{
		ContractID:        "C-PANIC",
		EventType:         discovery.EventTransfer,
		Ledger:            1,
		ObservedAtRFC3339: "2026-09-19T12:00:00Z",
	})

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
