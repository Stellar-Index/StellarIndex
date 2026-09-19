package divergence_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// K012 — Compare's per-reference fan-out already recovered (a broken
// reference must not take the comparison down; see
// TestCompare_PanicInOneReferenceIsolated), but it recovered PRIVATELY: the
// panic landed in one Result's Failures map and nowhere else, so the alert
// that exists to say "a detached goroutine died" — which reads
// stellarindex_worker_panics_total via stellarindex_worker_panicked — never
// fired. A reference panicking on every tick looked, to the page rule,
// exactly like a reference that was simply unreachable.
//
// This pins the recovered value going through worker.Report as well, and
// deliberately re-asserts the Failures label so routing the panic to the
// metric cannot quietly cost the operator-facing one.
func TestCompare_PanicMovesTheWorkerPanicCounter(t *testing.T) {
	const workerName = "divergence-reference-lookup"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	refs := []divergence.Reference{
		&stubReference{name: "good", price: 0.10},
		&panickingReference{name: "bad", panicValue: "kapow"},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 0.10, time.Now(),
		divergence.CompareOptions{})

	if got := res.Failures["bad"]; got != "panicked: kapow" {
		t.Errorf("Failures[bad] = %q, want %q — the operator-facing label must survive", got, "panicked: kapow")
	}
	if res.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", res.SuccessCount)
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v — a panic "+
			"recovered only into one comparison's Failures map is invisible to the page rule",
			workerName, after, before+1)
	}
}
