package divergence

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// chainlinkSeedChild reports whether vec already has a registered child
// series for the given consumer/pair label pair, WITHOUT creating one
// (unlike WithLabelValues, which always creates it as a side effect).
func chainlinkSeedChild(t *testing.T, vec *prometheus.CounterVec, consumer, pair string) bool {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		var gotConsumer, gotPair string
		for _, lp := range pb.GetLabel() {
			switch lp.GetName() {
			case "consumer":
				gotConsumer = lp.GetValue()
			case "pair":
				gotPair = lp.GetValue()
			}
		}
		if gotConsumer == consumer && gotPair == pair {
			return true
		}
	}
	return false
}

// TestNewChainlinkReferenceSeedsDecimalsCounters: the decimals counters have no producer until the first refusal, so "zero forever" would read as an absent series; NewChainlinkReference must pre-register the zero child per feed.
// Uses a pair name unique in this package so the assertion is independent of registration order and shared-counter pollution.
func TestNewChainlinkReferenceSeedsDecimalsCounters(t *testing.T) {
	const pair = "crypto:GH641SEEDTEST/fiat:USD"

	if chainlinkSeedChild(t, obs.ChainlinkFeedDecimalsMismatchTotal, "divergence", pair) {
		t.Fatalf("mismatch counter already has a %q child before construction — test pair collides", pair)
	}
	if chainlinkSeedChild(t, obs.ChainlinkFeedDecimalsVerifyFailedTotal, "divergence", pair) {
		t.Fatalf("verify-failed counter already has a %q child before construction — test pair collides", pair)
	}

	NewChainlinkReference(ChainlinkOptions{
		FeedMap: map[string]ChainlinkFeed{
			pair: {Address: "0x0000000000000000000000000000000000000001", Decimals: 8},
		},
	})

	if !chainlinkSeedChild(t, obs.ChainlinkFeedDecimalsMismatchTotal, "divergence", pair) {
		t.Errorf("NewChainlinkReference did not seed a zero-valued mismatch series for %q", pair)
	}
	if !chainlinkSeedChild(t, obs.ChainlinkFeedDecimalsVerifyFailedTotal, "divergence", pair) {
		t.Errorf("NewChainlinkReference did not seed a zero-valued verify-failed series for %q", pair)
	}
}
