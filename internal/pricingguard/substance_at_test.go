package pricingguard

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The thin-market gate must not measure a window ending NOW for reads
// that serve the price AS OF a past instant. These
// tests pin the property — the verdict for an instant is made
// from the market that existed at that instant — in both directions a
// NOW-anchored window gets wrong.

var (
	thickSubstance = timescale.MarketSubstance{VolumeUSD: "250000.5", Buckets: 900, SpanSeconds: 23 * 3600}
	dustSubstance  = timescale.MarketSubstance{VolumeUSD: "8.57", Buckets: 1, SpanSeconds: 0}
)

type atCall struct {
	asOf   time.Time
	window time.Duration
	grain  timescale.HistoryGranularity
}

// timedSubstanceReader is a market whose substance CHANGED over time:
// `live` is what a trailing-from-now measurement sees, `at` is what a
// measurement ending at the given instant sees.
type timedSubstanceReader struct {
	live      timescale.MarketSubstance
	at        func(asOf time.Time) timescale.MarketSubstance
	atErr     error
	liveCalls int
	atCalls   []atCall
}

func (r *timedSubstanceReader) PairMarketSubstance(
	context.Context, []canonical.Asset, []canonical.Asset, time.Duration,
) (timescale.MarketSubstance, error) {
	r.liveCalls++
	return r.live, nil
}

func (r *timedSubstanceReader) PairMarketSubstanceAt(
	_ context.Context, _, _ []canonical.Asset, asOf time.Time, window time.Duration, g timescale.HistoryGranularity,
) (timescale.MarketSubstance, error) {
	r.atCalls = append(r.atCalls, atCall{asOf: asOf, window: window, grain: g})
	if r.atErr != nil {
		return timescale.MarketSubstance{}, r.atErr
	}
	return r.at(asOf), nil
}

var gateNow = time.Date(2026, 9, 18, 12, 30, 45, 0, time.UTC)

func newTimedGate(r *timedSubstanceReader) *SubstanceGate {
	gate := NewSubstanceGate(r, SubstanceGateOptions{Policy: testPolicy()})
	gate.now = func() time.Time { return gateNow }
	return gate
}

func scamPair(t *testing.T) (canonical.Asset, canonical.Asset) {
	t.Helper()
	return mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"), mustAsset(t, "native")
}

// Gate.PriceWithholdingAt is one expression over both gates; the substance half
// is the point-in-time one.
func TestPriceWithheldAt_UsesThePointInTimeSubstanceVerdict(t *testing.T) {
	base, quote := scamPair(t)
	r := &timedSubstanceReader{
		live: thickSubstance,
		at:   func(time.Time) timescale.MarketSubstance { return dustSubstance },
	}
	gate := newTimedGate(r)
	if (Gate{Substance: gate}).PriceWithheld(context.Background(), base, quote, "test") {
		t.Fatal("fixture: the live read must not be withheld")
	}
	if (Gate{Substance: gate}).PriceWithholdingAt(context.Background(), base, quote, time.Date(2021, 3, 1, 9, 0, 0, 0, time.UTC), "test") == NotWithheld {
		t.Fatal("PriceWithholdingAt did not withhold a dust instant")
	}
	if (Gate{}).PriceWithholdingAt(context.Background(), base, quote, gateNow, "test") != NotWithheld {
		t.Error("nil gates must allow")
	}
}
