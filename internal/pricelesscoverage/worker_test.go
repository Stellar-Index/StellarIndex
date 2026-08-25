// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pricelesscoverage

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPopularPriceless_Census pins the tripwire's verdict against the three
// build-directive cases plus their guards. Each row is chosen so exactly
// ONE guard decides it, so removing that guard flips the row — the test is
// non-vacuous against each of: the market-character (wash) exclusion, the
// withheld verdict, the priced short-circuit, and the popularity floor.
func TestPopularPriceless_Census(t *testing.T) {
	cases := []struct {
		name string
		in   timescale.AssetCoverageSignals
		want bool
	}{
		{
			// (A) genuinely popular + priceless: many account pairs (low
			// concentration), lively 24h market (not withheld), no price.
			// FIRES — the coverage gap the tripwire exists to page for.
			name: "market_popular_priceless_fires",
			in: timescale.AssetCoverageSignals{
				AssetID: "GOODASSET-GISSUER", HasPriceUSD: false,
				Volume7dUSD: 50_000, Trades7d: 400, Volume24hUSD: 8_000,
				TopAccountPairVolShare: 0.20,
			},
			want: true,
		},
		{
			// (B) the reported scam AUD: ~$205k/day RAW volume but 99% in a
			// single wallet pair — a volume-painting wash farm. A raw-volume
			// floor would page for it; the market-character exclusion must
			// keep it SILENT.
			name: "wash_concentrated_priceless_silent",
			in: timescale.AssetCoverageSignals{
				AssetID: "SCAMAUD-GISSUER", HasPriceUSD: false,
				Volume7dUSD: 1_400_000, Trades7d: 763, Volume24hUSD: 205_000,
				TopAccountPairVolShare: 0.99,
			},
			want: false,
		},
		{
			// (C) withheld verdict: popular over 7d but its recent 24h
			// market is below the substance serve floor, so the gate
			// withholds the price by design — priceless is EXPECTED, silent.
			name: "withheld_below_substance_floor_silent",
			in: timescale.AssetCoverageSignals{
				AssetID: "QUIETNOW-GISSUER", HasPriceUSD: false,
				Volume7dUSD: 50_000, Trades7d: 400, Volume24hUSD: 200,
				TopAccountPairVolShare: 0.20,
			},
			want: false,
		},
		{
			// A priced asset is never a gap, even if otherwise popular.
			name: "priced_asset_silent",
			in: timescale.AssetCoverageSignals{
				AssetID: "USDC-GISSUER", HasPriceUSD: true,
				Volume7dUSD: 50_000, Trades7d: 400, Volume24hUSD: 8_000,
				TopAccountPairVolShare: 0.20,
			},
			want: false,
		},
		{
			// Below BOTH floors on market-character volume — a quiet
			// long-tail asset. No price is unremarkable; stays silent.
			name: "below_popularity_floor_silent",
			in: timescale.AssetCoverageSignals{
				AssetID: "TINY-GISSUER", HasPriceUSD: false,
				Volume7dUSD: 900, Trades7d: 40, Volume24hUSD: 2_000,
				TopAccountPairVolShare: 0.10,
			},
			want: false,
		},
		{
			// Popular by TRADE COUNT alone (thin per-trade but > 5k trades):
			// the OR-limb of the floor fires.
			name: "popular_by_trade_count_fires",
			in: timescale.AssetCoverageSignals{
				AssetID: "BUSY-GISSUER", HasPriceUSD: false,
				Volume7dUSD: 5_000, Trades7d: 6_000, Volume24hUSD: 3_000,
				TopAccountPairVolShare: 0.30,
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := popularPriceless(tc.in); got != tc.want {
				t.Errorf("popularPriceless(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestSubstanceFloor_MatchesServingDefault pins the withheld-verdict floor
// to the actual substance-gate default so the tripwire's notion of "the
// gate withheld this" can't silently drift from what the gate really does.
func TestSubstanceFloor_MatchesServingDefault(t *testing.T) {
	if substanceServeFloorUSD != float64(pricingguard.DefaultSubstanceMinVolumeUSD) {
		t.Errorf("substanceServeFloorUSD = %v, want the serving default %v — the tripwire's withheld floor must track the gate",
			substanceServeFloorUSD, pricingguard.DefaultSubstanceMinVolumeUSD)
	}
}

// fakeReader returns a fixed candidate set (and optional error).
type fakeReader struct {
	sigs []timescale.AssetCoverageSignals
	err  error
}

func (f *fakeReader) PopularPricelessCandidates(context.Context) ([]timescale.AssetCoverageSignals, error) {
	return f.sigs, f.err
}

// TestSweep_SetsGaugeToFiringCount proves the worker publishes the COUNT of
// firing assets — not a mock: the fake reader supplies raw signals and the
// real classifier decides, so the gauge reflects the census above (one
// popular-priceless + one wash + one withheld + one priced -> exactly 1).
func TestSweep_SetsGaugeToFiringCount(t *testing.T) {
	obs.AssetsPopularPriceless.Set(-1) // sentinel: prove the sweep overwrites it
	fixed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	w := New(&fakeReader{sigs: []timescale.AssetCoverageSignals{
		{AssetID: "GOODASSET-G", Volume7dUSD: 50_000, Trades7d: 400, Volume24hUSD: 8_000, TopAccountPairVolShare: 0.2},
		{AssetID: "SCAMAUD-G", Volume7dUSD: 1_400_000, Trades7d: 700, Volume24hUSD: 205_000, TopAccountPairVolShare: 0.99},
		{AssetID: "QUIETNOW-G", Volume7dUSD: 50_000, Trades7d: 400, Volume24hUSD: 200, TopAccountPairVolShare: 0.2},
		{AssetID: "USDC-G", HasPriceUSD: true, Volume7dUSD: 90_000, Trades7d: 900, Volume24hUSD: 40_000, TopAccountPairVolShare: 0.1},
	}}, Options{Clock: func() time.Time { return fixed }})

	w.Sweep(context.Background())

	if got := testutil.ToFloat64(obs.AssetsPopularPriceless); got != 1 {
		t.Errorf("gauge = %v, want 1 (only the market-popular priceless asset fires)", got)
	}
	if got := testutil.ToFloat64(obs.PricelessCoverageCheckLastSuccessUnix); got != float64(fixed.Unix()) {
		t.Errorf("last_success = %v, want %v", got, fixed.Unix())
	}
}

// TestSweep_ReadErrorDoesNotClobberGauge: a candidate-read failure records
// the error outcome and LEAVES the last good gauge value standing (a blind
// sweep must not flap the coverage count to a false 0).
func TestSweep_ReadErrorDoesNotClobberGauge(t *testing.T) {
	obs.AssetsPopularPriceless.Set(3)
	w := New(&fakeReader{err: context.DeadlineExceeded}, Options{})
	w.Sweep(context.Background())
	if got := testutil.ToFloat64(obs.AssetsPopularPriceless); got != 3 {
		t.Errorf("gauge = %v, want the last good 3 (read error must not clobber it)", got)
	}
}

// deadlineReader captures the deadline of the context it is handed.
type deadlineReader struct {
	sawDeadline bool
	within      time.Duration
}

func (d *deadlineReader) PopularPricelessCandidates(ctx context.Context) ([]timescale.AssetCoverageSignals, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.sawDeadline = true
		d.within = time.Until(dl)
	}
	return nil, nil
}

// TestSweep_BoundsReadWithTimeout proves the sweep hands the reader a
// deadline-bearing context (the full-catalogue scan is ~55s warm and can
// balloon under load; an unbounded read could pin a DB connection). The
// live smoke that motivated this measured 54.5s, so the ceiling must sit
// above that yet be finite. Red-proof: pass ctx straight through in Sweep
// and sawDeadline goes false.
func TestSweep_BoundsReadWithTimeout(t *testing.T) {
	r := &deadlineReader{}
	w := New(r, Options{SweepTimeout: 90 * time.Second})
	w.Sweep(context.Background())
	if !r.sawDeadline {
		t.Fatal("reader got a context with NO deadline — the sweep read is unbounded")
	}
	if r.within <= 0 || r.within > 90*time.Second {
		t.Errorf("deadline %v, want (0, 90s] (the configured SweepTimeout)", r.within)
	}
}

// TestSweepTimeout_DefaultApplied proves an unset SweepTimeout falls back to
// DefaultSweepTimeout rather than leaving the read unbounded.
func TestSweepTimeout_DefaultApplied(t *testing.T) {
	r := &deadlineReader{}
	w := New(r, Options{}) // no SweepTimeout
	w.Sweep(context.Background())
	if !r.sawDeadline {
		t.Fatal("reader got no deadline with a zero SweepTimeout — default not applied")
	}
	// Allow a little slack below DefaultSweepTimeout for elapsed time.
	if r.within <= DefaultSweepTimeout-5*time.Second || r.within > DefaultSweepTimeout {
		t.Errorf("deadline %v, want ~= DefaultSweepTimeout %v", r.within, DefaultSweepTimeout)
	}
}
