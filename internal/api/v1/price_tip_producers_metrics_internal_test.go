package v1

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// The producer ceiling is only operable if an operator can see it being
// hit: these pin that each refusal reason moves its own counter child and
// that the gauge tracks registry entries through the linger.

func TestTipProducerRegistry_RefusalsAreCountedByReason(t *testing.T) {
	blockForever := func(ctx context.Context) { <-ctx.Done() }
	quota := obs.APITipProducersRefusedTotal.WithLabelValues(tipProducerAtCallerQuota.String())
	ceiling := obs.APITipProducersRefusedTotal.WithLabelValues(tipProducerAtGlobalCeiling.String())
	quotaBefore, ceilingBefore := testutil.ToFloat64(quota), testutil.ToFloat64(ceiling)

	perCaller := &tipProducerRegistry{maxPerCaller: 1}
	rel, outcome := perCaller.acquireFor(tipProducerKey{asset: "native", quote: "fiat:USD", window: 11},
		"203.0.113.0", nil, blockForever)
	if outcome != tipProducerAdmitted {
		t.Fatalf("first mint = %v, want admitted", outcome)
	}
	defer rel()
	if _, outcome = perCaller.acquireFor(tipProducerKey{asset: "native", quote: "fiat:USD", window: 12},
		"203.0.113.0", nil, blockForever); outcome != tipProducerAtCallerQuota {
		t.Fatalf("second mint = %v, want caller_quota", outcome)
	}

	global := &tipProducerRegistry{maxProducers: 1}
	rel2, ok := global.acquire(tipProducerKey{asset: "native", quote: "fiat:USD", window: 13}, nil, blockForever)
	if !ok {
		t.Fatal("first acquire refused")
	}
	defer rel2()
	if _, outcome = global.acquireFor(tipProducerKey{asset: "native", quote: "fiat:USD", window: 14},
		"198.51.100.0", nil, blockForever); outcome != tipProducerAtGlobalCeiling {
		t.Fatalf("over-ceiling mint = %v, want global_ceiling", outcome)
	}

	if got := testutil.ToFloat64(quota) - quotaBefore; got != 1 {
		t.Errorf("caller_quota refusals counted = %v, want 1", got)
	}
	if got := testutil.ToFloat64(ceiling) - ceilingBefore; got != 1 {
		t.Errorf("global_ceiling refusals counted = %v, want 1", got)
	}
}

func TestTipProducerRegistry_GaugeTracksRegisteredProducers(t *testing.T) {
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_tip_producers"})
	reg := &tipProducerRegistry{lingerFor: 10 * time.Millisecond, gauge: gauge}
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 15}

	rel, ok := reg.acquire(key, nil, func(ctx context.Context) { <-ctx.Done() })
	if !ok {
		t.Fatal("acquire refused")
	}
	rel2, _ := reg.acquire(key, nil, func(ctx context.Context) { <-ctx.Done() })
	if got := testutil.ToFloat64(gauge); got != 1 {
		t.Fatalf("gauge = %v with one shared producer, want 1", got)
	}
	rel()
	rel2()
	if !waitFor(time.Second, func() bool { return testutil.ToFloat64(gauge) == 0 }) {
		t.Fatalf("gauge = %v after the linger expired, want 0", testutil.ToFloat64(gauge))
	}
}
