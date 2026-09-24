// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pricelesscoverage

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPopularPriceless_Census pins the tripwire's verdict against the three
// build-directive cases plus their guards. Each row is chosen so exactly
// ONE guard decides it, so removing that guard flips the row — the test is
// non-vacuous against each of: the market-character (wash) exclusion, the
// priced short-circuit, and the popularity floor. The withheld verdict is
// the serving gate's, pinned by TestSweep_WithheldIsTheServingGatesVerdict.
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

// substanceStore answers every pair's trailing substance with one reading.
type substanceStore struct{ m timescale.MarketSubstance }

func (f substanceStore) PairMarketSubstance(context.Context, canonical.Pair, time.Duration) (timescale.MarketSubstance, error) {
	return f.m, nil
}

// The tripwire's "withheld" is the serving gate's own asset verdict, not a
// re-derivation of one of its floors. Each case disagrees with the old
// 24h-volume-only reading in one direction: a market clearing $1,000 in
// too few distinct minutes is withheld by the gate (no gap), and one whose
// candidate-row 24h volume is low while the gate's alias-union measure
// clears is served-or-missing, so its absence is a real gap.
func TestSweep_WithheldIsTheServingGatesVerdict(t *testing.T) {
	const assetID = "GOLD-GATISXX6BZ6NC7IKQBY37CJD4SOZL3CYZJWXEDG6JVIY4WBS6KXJHN6Q"
	cases := []struct {
		name      string
		substance timescale.MarketSubstance
		vol24h    float64
		want      float64
	}{
		{
			name:      "gate_withholds_few_buckets_despite_volume",
			substance: timescale.MarketSubstance{VolumeUSD: "8000", Buckets: 5, SpanSeconds: 86_400},
			vol24h:    8_000,
			want:      0,
		},
		{
			name:      "gate_allows_so_absence_is_a_gap",
			substance: timescale.MarketSubstance{VolumeUSD: "50000", Buckets: 600, SpanSeconds: 86_400},
			vol24h:    200,
			want:      1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := pricingguard.NewSubstanceGate(substanceStore{m: tc.substance}, pricingguard.SubstanceGateOptions{})
			w := New(&fakeReader{sigs: []timescale.AssetCoverageSignals{{
				AssetID: assetID, Volume7dUSD: 50_000, Trades7d: 400,
				Volume24hUSD: tc.vol24h, TopAccountPairVolShare: 0.2,
			}}}, Options{Withheld: SubstanceWithheld(gate, nil)})
			w.Sweep(context.Background())
			if got := testutil.ToFloat64(obs.AssetsPopularPriceless); got != tc.want {
				t.Errorf("gauge = %v, want %v", got, tc.want)
			}
		})
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
// popular-priceless + one wash + one gate-withheld + one priced -> exactly 1).
func TestSweep_SetsGaugeToFiringCount(t *testing.T) {
	obs.AssetsPopularPriceless.Set(-1) // sentinel: prove the sweep overwrites it
	fixed := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	w := New(&fakeReader{sigs: []timescale.AssetCoverageSignals{
		{AssetID: "GOODASSET-G", Volume7dUSD: 50_000, Trades7d: 400, Volume24hUSD: 8_000, TopAccountPairVolShare: 0.2},
		{AssetID: "SCAMAUD-G", Volume7dUSD: 1_400_000, Trades7d: 700, Volume24hUSD: 205_000, TopAccountPairVolShare: 0.99},
		{AssetID: "QUIETNOW-G", Volume7dUSD: 50_000, Trades7d: 400, Volume24hUSD: 200, TopAccountPairVolShare: 0.2},
		{AssetID: "USDC-G", HasPriceUSD: true, Volume7dUSD: 90_000, Trades7d: 900, Volume24hUSD: 40_000, TopAccountPairVolShare: 0.1},
	}}, Options{
		Clock:    func() time.Time { return fixed },
		Withheld: func(_ context.Context, id string) bool { return id == "QUIETNOW-G" },
	})

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

const ybtcSAC = "CB2XMFB6BDIHFOSFB5IXHDOYV3SI3IXMNIZLPDZHC7ENDCXSBEBZAO2Y"

// r1, 2026-09-17: yBTC's SAC traded $43.8k on aquarius under its contract
// id while yBTC was priced under its classic id, and the tripwire ticketed
// a "priceless popular asset" that had a price. A SAC candidate whose
// classic asset is priced is not a gap; one whose classic asset is also
// priceless still is.
func TestSweep_SACCandidatePricedUnderItsClassicAssetIsNotAGap(t *testing.T) {
	sigs := []timescale.AssetCoverageSignals{
		{AssetID: ybtcSAC, Volume7dUSD: 43_823, Trades7d: 11, Volume24hUSD: 5_000, TopAccountPairVolShare: 0},
	}
	const classic = "yBTC-GBUVRNH4RW4VLHP4C5MOF46RRIRZLAVHYGX45MVSTKA2F6TMR7E7L6NW"
	resolve := func(_ context.Context, id string) (string, bool) {
		if id == ybtcSAC {
			return classic, true
		}
		return "", false
	}
	pricedClassic := map[string]bool{classic: true}
	isPriced := func(_ context.Context, id string) (bool, error) { return pricedClassic[id], nil }

	obs.AssetsPopularPriceless.Set(-1)
	w := New(&fakeReader{sigs: sigs}, Options{ResolveSAC: resolve, IsPriced: isPriced})
	w.Sweep(context.Background())
	if got := testutil.ToFloat64(obs.AssetsPopularPriceless); got != 0 {
		t.Errorf("gauge = %v, want 0: the SAC is priced under its classic asset", got)
	}

	pricedClassic[classic] = false
	w.Sweep(context.Background())
	if got := testutil.ToFloat64(obs.AssetsPopularPriceless); got != 1 {
		t.Errorf("gauge = %v, want 1: neither spelling is priced", got)
	}

	obs.AssetsPopularPriceless.Set(-1)
	New(&fakeReader{sigs: sigs}, Options{}).Sweep(context.Background())
	if got := testutil.ToFloat64(obs.AssetsPopularPriceless); got != 1 {
		t.Errorf("gauge = %v, want 1 without a resolver: the candidate is read as given", got)
	}
}

// A probe error must not silently clear a gap.
func TestSweep_AliasProbeErrorStaysAGap(t *testing.T) {
	sigs := []timescale.AssetCoverageSignals{
		{AssetID: ybtcSAC, Volume7dUSD: 43_823, Trades7d: 11, Volume24hUSD: 5_000},
	}
	w := New(&fakeReader{sigs: sigs}, Options{
		ResolveSAC: func(context.Context, string) (string, bool) { return "yBTC-G", true },
		IsPriced:   func(context.Context, string) (bool, error) { return true, context.DeadlineExceeded },
	})
	obs.AssetsPopularPriceless.Set(-1)
	w.Sweep(context.Background())
	if got := testutil.ToFloat64(obs.AssetsPopularPriceless); got != 1 {
		t.Errorf("gauge = %v, want 1: a failed probe is not a price", got)
	}
}
