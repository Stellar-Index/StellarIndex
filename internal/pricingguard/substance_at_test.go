package pricingguard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Finding T038: the thin-market gate measured a window ending NOW and
// was applied to reads that serve the price AS OF a past instant. These
// tests pin the corrected property — the verdict for an instant is made
// from the market that existed at that instant — in both directions the
// old behaviour got wrong.

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

func (r *timedSubstanceReader) PairMarketSubstance(context.Context, canonical.Pair, time.Duration) (timescale.MarketSubstance, error) {
	r.liveCalls++
	return r.live, nil
}

func (r *timedSubstanceReader) PairMarketSubstanceAt(
	_ context.Context, _ canonical.Pair, asOf time.Time, window time.Duration, g timescale.HistoryGranularity,
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

// The dangerous direction: thick TODAY, attacker-seeded dust at the
// requested instant. The old gate asked about today, passed, and the
// point-in-time read served the manipulated historical price.
func TestSubstanceGate_AllowedAt_WithholdsInstantThatWasDustThoughMarketIsThickToday(t *testing.T) {
	base, quote := scamPair(t)
	seeded := time.Date(2021, 3, 1, 9, 0, 0, 0, time.UTC)
	r := &timedSubstanceReader{
		live: thickSubstance,
		at:   func(time.Time) timescale.MarketSubstance { return dustSubstance },
	}
	gate := newTimedGate(r)

	if !gate.Allowed(context.Background(), base, quote, "test") {
		t.Fatal("fixture: the live market must clear the floor, or this test proves nothing about time")
	}
	if gate.AllowedAt(context.Background(), base, quote, seeded, "test") {
		t.Fatal("AllowedAt served an instant whose own market was one $8.57 bucket, because " +
			"the market is thick TODAY — the historical read would publish the seeded price")
	}
}

// The harmful direction: deep and honest at the requested instant,
// dormant today. The old gate withheld every historical price we hold.
func TestSubstanceGate_AllowedAt_ServesInstantThatWasDeepThoughMarketIsDormantToday(t *testing.T) {
	base, quote := scamPair(t)
	r := &timedSubstanceReader{
		live: timescale.MarketSubstance{VolumeUSD: "0"},
		at:   func(time.Time) timescale.MarketSubstance { return thickSubstance },
	}
	gate := newTimedGate(r)

	if gate.Allowed(context.Background(), base, quote, "test") {
		t.Fatal("fixture: the live market must be below the floor")
	}
	if !gate.AllowedAt(context.Background(), base, quote, time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "test") {
		t.Fatal("AllowedAt withheld an instant whose own market was deep, because the market " +
			"is dormant TODAY — a cost-basis read 404s for data we hold and trust")
	}
}

// The measurement must END at the requested instant, at the grain the
// point-in-time reader can serve that instant from.
func TestSubstanceGate_AllowedAt_MeasuresTheWindowEndingAtTheInstant(t *testing.T) {
	base, quote := scamPair(t)
	cases := []struct {
		name      string
		at        time.Time
		wantAsOf  time.Time
		wantGrain timescale.HistoryGranularity
	}{
		{
			name:      "inside the minute rung: minute grain, truncated to the minute",
			at:        gateNow.Add(-time.Hour),
			wantAsOf:  time.Date(2026, 9, 18, 11, 30, 0, 0, time.UTC),
			wantGrain: timescale.Granularity1m,
		},
		{
			name:      "exactly at the minute-rung boundary: still minute grain",
			at:        gateNow.Add(-timescale.PriceAtMinuteRungMaxAge),
			wantAsOf:  time.Date(2026, 9, 16, 12, 30, 0, 0, time.UTC),
			wantGrain: timescale.Granularity1m,
		},
		{
			name:      "past the minute rung: hour grain, truncated to the hour",
			at:        time.Date(2024, 6, 1, 15, 42, 10, 0, time.UTC),
			wantAsOf:  time.Date(2024, 6, 1, 15, 0, 0, 0, time.UTC),
			wantGrain: timescale.Granularity1h,
		},
		{
			name:      "a future instant is measured as of now, never past it",
			at:        gateNow.Add(72 * time.Hour),
			wantAsOf:  time.Date(2026, 9, 18, 12, 30, 0, 0, time.UTC),
			wantGrain: timescale.Granularity1m,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &timedSubstanceReader{at: func(time.Time) timescale.MarketSubstance { return thickSubstance }}
			gate := newTimedGate(r)
			gate.AllowedAt(context.Background(), base, quote, tc.at, "test")

			if r.liveCalls != 0 {
				t.Errorf("made %d trailing-from-now measurement(s) for a point-in-time verdict", r.liveCalls)
			}
			if len(r.atCalls) == 0 {
				t.Fatal("no point-in-time measurement was made")
			}
			for _, c := range r.atCalls {
				if !c.asOf.Equal(tc.wantAsOf) {
					t.Errorf("measured as of %s, want %s", c.asOf, tc.wantAsOf)
				}
				if c.grain != tc.wantGrain {
					t.Errorf("measured at grain %q, want %q", c.grain, tc.wantGrain)
				}
				if c.window != testPolicy().withDefaults().Window {
					t.Errorf("measured a %s window, want the policy's %s", c.window, testPolicy().withDefaults().Window)
				}
			}
		})
	}
}

// The hour floor must admit exactly the weakest market the minute floor
// admits — 20 minutes across a 6h span can be as few as two hour
// buckets — and must still refuse a single burst.
func TestSubstanceGate_AllowedAt_HourGrainFloor(t *testing.T) {
	base, quote := scamPair(t)
	old := time.Date(2024, 6, 1, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		sub  timescale.MarketSubstance
		want bool
	}{
		{"two hour buckets six hours apart, over the volume floor", timescale.MarketSubstance{VolumeUSD: "5000", Buckets: 2, SpanSeconds: 6 * 3600}, true},
		{"one hour bucket — a single burst, whatever its size", timescale.MarketSubstance{VolumeUSD: "9000000", Buckets: 1, SpanSeconds: 0}, false},
		{"two adjacent hours — span leg unchanged at hour grain", timescale.MarketSubstance{VolumeUSD: "5000", Buckets: 2, SpanSeconds: 3600}, false},
		{"spread out but under the volume floor — volume leg unchanged", timescale.MarketSubstance{VolumeUSD: "8.57", Buckets: 12, SpanSeconds: 20 * 3600}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &timedSubstanceReader{at: func(time.Time) timescale.MarketSubstance { return tc.sub }}
			if got := newTimedGate(r).AllowedAt(context.Background(), base, quote, old, "test"); got != tc.want {
				t.Errorf("AllowedAt = %v, want %v", got, tc.want)
			}
		})
	}
}

// Inside the minute rung the floor is the LIVE floor, unweakened: two
// buckets that would clear the hour floor must not clear this one, or
// /v1/price/at?ts=<a minute ago> republishes what /v1/price refuses.
func TestSubstanceGate_AllowedAt_RecentInstantKeepsTheMinuteFloor(t *testing.T) {
	base, quote := scamPair(t)
	r := &timedSubstanceReader{at: func(time.Time) timescale.MarketSubstance {
		return timescale.MarketSubstance{VolumeUSD: "5000", Buckets: 2, SpanSeconds: 6 * 3600}
	}}
	if newTimedGate(r).AllowedAt(context.Background(), base, quote, gateNow.Add(-5*time.Minute), "test") {
		t.Fatal("a recent instant was held to the hour-grain bucket floor — the minute rung " +
			"serves the raw 1m bucket and must keep the live gate's floor")
	}
}

// Verdicts for different instants must not share a cache slot, and must
// not share one with the live verdict.
func TestSubstanceGate_AllowedAt_CachesPerInstant(t *testing.T) {
	base, quote := scamPair(t)
	dustDay := time.Date(2021, 3, 1, 9, 0, 0, 0, time.UTC)
	deepDay := time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC)
	r := &timedSubstanceReader{
		live: thickSubstance,
		at: func(asOf time.Time) timescale.MarketSubstance {
			if asOf.Equal(dustDay) {
				return dustSubstance
			}
			return thickSubstance
		},
	}
	gate := newTimedGate(r)
	ctx := context.Background()

	gate.Allowed(ctx, base, quote, "test")
	if gate.AllowedAt(ctx, base, quote, dustDay, "test") {
		t.Error("dust instant inherited the live verdict")
	}
	if !gate.AllowedAt(ctx, base, quote, deepDay, "test") {
		t.Error("deep instant inherited the dust instant's verdict")
	}
	before := len(r.atCalls)
	gate.AllowedAt(ctx, base, quote, dustDay.Add(20*time.Minute), "test") // same hour → same verdict
	if len(r.atCalls) != before {
		t.Errorf("an instant in an already-measured hour re-queried the store (%d → %d calls)", before, len(r.atCalls))
	}
	if !gate.Allowed(ctx, base, quote, "test") {
		t.Error("a withheld point-in-time verdict leaked into the live verdict")
	}
}

// Same asymmetric posture as the live gate: a store error serves, and
// is not cached.
func TestSubstanceGate_AllowedAt_FailsOpenOnStoreErrorAndDoesNotCache(t *testing.T) {
	base, quote := scamPair(t)
	r := &timedSubstanceReader{atErr: errors.New("connection reset")}
	gate := newTimedGate(r)
	at := time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC)

	if !gate.AllowedAt(context.Background(), base, quote, at, "test") {
		t.Fatal("a store error must fail open — a DB blip must not 404 the price surface")
	}
	r.atErr = nil
	r.at = func(time.Time) timescale.MarketSubstance { return dustSubstance }
	if gate.AllowedAt(context.Background(), base, quote, at, "test") {
		t.Fatal("the fail-open answer was cached: a dust instant stayed served after the store recovered")
	}
}

func TestSubstanceGate_AllowedAt_NilGateAndUngatedPairAllow(t *testing.T) {
	var gate *SubstanceGate
	base, quote := scamPair(t)
	if !gate.AllowedAt(context.Background(), base, quote, gateNow, "test") {
		t.Error("nil gate must allow — a disabled [pricing_guard] must not withhold")
	}
	r := &timedSubstanceReader{at: func(time.Time) timescale.MarketSubstance { return dustSubstance }}
	if !newTimedGate(r).AllowedAt(context.Background(), mustAsset(t, "fiat:EUR"), mustAsset(t, "fiat:USD"), gateNow, "test") {
		t.Error("an off-chain pair is out of the gate's scope at any instant")
	}
	if len(r.atCalls) != 0 {
		t.Error("an ungated pair was measured")
	}
}

// PriceWithheldAt is one expression over both gates; the substance half
// is the point-in-time one.
func TestPriceWithheldAt_UsesThePointInTimeSubstanceVerdict(t *testing.T) {
	base, quote := scamPair(t)
	r := &timedSubstanceReader{
		live: thickSubstance,
		at:   func(time.Time) timescale.MarketSubstance { return dustSubstance },
	}
	gate := newTimedGate(r)
	if PriceWithheld(context.Background(), gate, nil, base, quote, "test") {
		t.Fatal("fixture: the live read must not be withheld")
	}
	if !PriceWithheldAt(context.Background(), gate, nil, base, quote, time.Date(2021, 3, 1, 9, 0, 0, 0, time.UTC), "test") {
		t.Fatal("PriceWithheldAt did not withhold a dust instant")
	}
	if PriceWithheldAt(context.Background(), nil, nil, base, quote, gateNow, "test") {
		t.Error("nil gates must allow")
	}
}
