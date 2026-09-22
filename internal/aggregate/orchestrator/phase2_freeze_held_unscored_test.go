package orchestrator

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestLogFreezeTransition_HeldUnscored_IsCounted — a hold expiry that
// lands on an unscored bucket ([freeze.TransitionHeldUnscored]) must
// be visible: a dedicated counter increments so a sustained run (a
// stuck scorer) is distinguishable from the silent, Debug-only
// default arm it used to fall into alongside every unrecognised
// transition.
func TestLogFreezeTransition_HeldUnscored_IsCounted(t *testing.T) {
	o := &Orchestrator{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	pair := xlmUsdtPair(t)

	before := testutil.ToFloat64(obs.AnomalyFreezeHeldUnscoredTotal)

	o.logFreezeTransition(pair, time.Hour, anomaly.Decision{}, freeze.Outcome{
		Transition: freeze.TransitionHeldUnscored,
		State:      freeze.State{ExtensionsUsed: 1},
	})

	after := testutil.ToFloat64(obs.AnomalyFreezeHeldUnscoredTotal)
	if got := after - before; got != 1 {
		t.Errorf("AnomalyFreezeHeldUnscoredTotal increment = %v, want 1", got)
	}
}
