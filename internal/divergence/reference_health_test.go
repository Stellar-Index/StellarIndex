package divergence_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// GH-679 (1): a pair with exactly two references loses one to a
// fail-closed freshness gate (price_unavailable). SuccessCount drops below
// the quorum, the verdict is carried forward instead of evaluated, and the
// pass still returns nil — so the pass-level outcome counter reads "ok".
// The per-reference counter and the pair quorum gauge must say otherwise.
func TestRefreshPair_ExportsReferenceOutcomesAndQuorumLoss(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "gh679-healthy", price: 1.40},
		&stubReference{name: "gh679-degraded", err: divergence.ErrPriceUnavailable},
	}
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{MinSourcesForWarning: 2})
	pair := xlmUSD(t)

	okBefore := testutil.ToFloat64(obs.DivergenceReferenceTotal.WithLabelValues("gh679-healthy", "ok"))
	failBefore := testutil.ToFloat64(obs.DivergenceReferenceTotal.WithLabelValues("gh679-degraded", "price_unavailable"))

	if err := svc.RefreshPair(context.Background(), pair, 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v (a below-quorum pass is not a refresh error)", err)
	}

	if got := testutil.ToFloat64(obs.DivergenceReferenceTotal.WithLabelValues("gh679-healthy", "ok")) - okBefore; got != 1 {
		t.Errorf("reference_total{gh679-healthy,ok} moved by %v, want 1", got)
	}
	if got := testutil.ToFloat64(obs.DivergenceReferenceTotal.WithLabelValues("gh679-degraded", "price_unavailable")) - failBefore; got != 1 {
		t.Errorf("reference_total{gh679-degraded,price_unavailable} moved by %v, want 1", got)
	}
	if got := testutil.ToFloat64(obs.DivergencePairQuorumMet.WithLabelValues(pair.String())); got != 0 {
		t.Errorf("pair_quorum_met = %v, want 0: one of two references responded, below quorum 2", got)
	}

	// Recovery re-arms the gauge.
	refs[1].(*stubReference).err = nil
	refs[1].(*stubReference).price = 1.00
	if err := svc.RefreshPair(context.Background(), pair, 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	if got := testutil.ToFloat64(obs.DivergencePairQuorumMet.WithLabelValues(pair.String())); got != 1 {
		t.Errorf("pair_quorum_met = %v after recovery, want 1", got)
	}
}

// The per-reference outcome is a metric label, so it must be a bounded
// class even where Failures carries verbatim error text.
func TestCompare_OutcomesAreBoundedClasses(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "ok", price: 1.00},
		&stubReference{name: "unsupported", err: divergence.ErrAssetUnsupported},
		&stubReference{name: "stale", err: divergence.ErrTooStaleToCompare},
		&stubReference{name: "slow", price: 1.00, delay: time.Second},
		&stubReference{name: "zero", price: 0},
		&stubReference{name: "opaque", err: errors.New("dial tcp 10.0.0.1:443: connection refused")},
		&panickingReference{name: "broken", panicValue: "kapow"},
	}
	res := divergence.Compare(context.Background(), refs, xlmUSD(t), 1.00, time.Now(),
		divergence.CompareOptions{PerReferenceTimeout: 50 * time.Millisecond})

	want := map[string]string{
		"ok":          divergence.OutcomeOK,
		"unsupported": divergence.OutcomeAssetUnsupported,
		"stale":       divergence.OutcomeTooStale,
		"slow":        divergence.OutcomeTimeout,
		"zero":        divergence.OutcomeInvalidPrice,
		"opaque":      divergence.OutcomeError,
		"broken":      divergence.OutcomePanicked,
	}
	if len(res.Outcomes) != len(want) {
		t.Errorf("Outcomes = %v, want one entry per reference (%d)", res.Outcomes, len(want))
	}
	for name, outcome := range want {
		if got := res.Outcomes[name]; got != outcome {
			t.Errorf("Outcomes[%s] = %q, want %q", name, got, outcome)
		}
	}
	if got := res.Failures["broken"]; got != "panicked: kapow" {
		t.Errorf("Failures[broken] = %q, want the operator-facing label unchanged", got)
	}
	vocab := map[string]bool{}
	for _, o := range divergence.ReferenceOutcomes {
		vocab[o] = true
	}
	for name, o := range res.Outcomes {
		if !vocab[o] {
			t.Errorf("Outcomes[%s] = %q is outside ReferenceOutcomes", name, o)
		}
	}
}

// GH-679 (2): the div: key must outlive the gap between two writes of the
// same pair. With the default 300s min interval on a 30s tick the pass runs
// every 330s, so a key written with the bare 300s TTL expired ~30s before
// its rewrite on every cycle — longer when the fan-out is slow — and the
// freeze path read "no cross-oracle data" throughout.
func TestRefreshPair_TTLBridgesRefreshCadence(t *testing.T) {
	const cadence = 330 * time.Second
	refs := []divergence.Reference{&stubReference{name: "a", price: 1.00}}
	svc, _, mr := newTestService(t, refs, divergence.ServiceOptions{RefreshInterval: cadence})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	if ttl := mr.TTL(cachekeys.Divergence(xlmUSD(t)).String()); ttl <= cadence {
		t.Errorf("div: TTL = %v, want > the %v refresh cadence", ttl, cadence)
	}
	if ttl := mr.TTL(cachekeys.DivergenceBaseIndex(xlmUSD(t).Base).String()); ttl <= cadence {
		t.Errorf("div:idx: TTL = %v, want > the %v refresh cadence", ttl, cadence)
	}
}

// The last pair of a sequential pass is rewritten a whole pass later than
// the first, and each pair can spend Compare's full overall budget (2x the
// per-reference timeout) on a reference that times out.
func TestRefreshPair_TTLCoversWorstCasePass(t *testing.T) {
	const (
		cadence = 330 * time.Second
		pairs   = 12
		perRef  = 5 * time.Second
	)
	refs := []divergence.Reference{&stubReference{name: "a", price: 1.00}}
	svc, _, mr := newTestService(t, refs, divergence.ServiceOptions{
		RefreshInterval:     cadence,
		PairCount:           pairs,
		PerReferenceTimeout: perRef,
	})
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	worstGap := cadence + pairs*2*perRef
	if ttl := mr.TTL(cachekeys.Divergence(xlmUSD(t)).String()); ttl <= worstGap {
		t.Errorf("div: TTL = %v, want > %v (cadence + %d pairs x 2 x %v)", ttl, worstGap, pairs, perRef)
	}
}
