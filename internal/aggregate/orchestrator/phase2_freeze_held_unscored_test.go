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

// TestLogFreezeTransition_HeldUnscored_IsCounted: a hold expiry on an unscored bucket ([freeze.TransitionHeldUnscored]) must increment a dedicated counter, so a sustained run (stuck scorer) is not hidden in the Debug-only default arm.
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
