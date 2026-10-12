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

// TestPopularPriceless_Census pins the tripwire's verdict; each row is decided by exactly ONE guard (market-character wash
// exclusion, priced short-circuit, popularity floor), so removing it flips the row. The withheld verdict is pinned by
// TestSweep_WithheldIsTheServingGatesVerdict.
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
			// (B) the reported scam AUD: ~$205k/day raw volume, 99% in one wallet pair (a wash farm). A raw-volume floor would page; the
			// market-character exclusion must stay SILENT.
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
		{
			// Mid-range wash: its 0.80 share is under the 0.90 washConcentrated gate, but $16k of $20k raw 7d volume is one wash pair,
			// leaving $4k market-character volume under the $10k floor. A raw-volume comparison fires; the market-character discount must
			// not.
			name: "mid_range_wash_discount_silent",
			in: timescale.AssetCoverageSignals{
				AssetID: "MIDWASH-GISSUER", HasPriceUSD: false,
				Volume7dUSD: 20_000, Trades7d: 200, Volume24hUSD: 15_000,
				TopAccountPairVolShare: 0.80,
				TopAccountPairVolUSD:   16_000,
				TopAccountPairTrades7d: 160,
			},
			want: false,
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

func (f substanceStore) PairMarketSubstance(
	context.Context, []canonical.Asset, []canonical.Asset, time.Duration,
) (timescale.MarketSubstance, error) {
	return f.m, nil
}

func (f substanceStore) PairMarketSubstanceAt(
	context.Context, []canonical.Asset, []canonical.Asset, time.Time, time.Duration, timescale.HistoryGranularity,
) (timescale.MarketSubstance, error) {
	return f.m, nil
}

// "Withheld" is the serving gate's own asset verdict, not a re-derivation of one floor: a market clearing $1,000 in too few
// distinct minutes is withheld (no gap), while low candidate-row 24h volume with a clearing alias-union measure is served-or-
// missing, so its absence is a real gap.
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

// TestSweep_SetsGaugeToFiringCount proves the worker publishes the COUNT of firing assets using the real classifier over fake
// raw signals (one popular-priceless + one wash + one gate-withheld + one priced -> exactly 1).
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

// TestSweep_BoundsReadWithTimeout proves the reader gets a deadline-bearing context: the full-catalogue scan takes ~1 min warm
// and can balloon, and an unbounded read could pin a DB connection. The ceiling must sit above the warm scan yet be finite.
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

// TestSweep_SACCandidatePricedUnderItsClassicAsset: a SAC trading under its contract id whose classic asset is priced is not a
// gap for the "priceless popular asset" tripwire; one whose classic asset is also priceless still is.
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

// A per-candidate probe timeout is reported distinctly from a clean sweep,
// so an operator does not read a timeout burst as a confirmed mass gap.
func TestSweep_ProbeTimeoutReportsDegradedOutcome(t *testing.T) {
	sigs := []timescale.AssetCoverageSignals{
		{AssetID: ybtcSAC, Volume7dUSD: 43_823, Trades7d: 11, Volume24hUSD: 5_000},
	}
	w := New(&fakeReader{sigs: sigs}, Options{
		ResolveSAC: func(context.Context, string) (string, bool) { return "yBTC-G", true },
		IsPriced:   func(context.Context, string) (bool, error) { return false, context.DeadlineExceeded },
	})
	before := testutil.ToFloat64(obs.PricelessCoverageCheckRunsTotal.WithLabelValues("degraded"))
	w.Sweep(context.Background())
	if after := testutil.ToFloat64(obs.PricelessCoverageCheckRunsTotal.WithLabelValues("degraded")); after != before+1 {
		t.Errorf("degraded outcome count = %v, want %v", after, before+1)
	}
}

// Each candidate's probe must get its OWN bounded context, not the shared sweepCtx: a probe must
// not inherit an already-expired deadline, or every remaining candidate times out together,
// indistinguishable from a real mass coverage gap.
func TestSweep_ProbeContextIndependentOfSweepTimeout(t *testing.T) {
	var probeDeadline time.Duration
	sigs := []timescale.AssetCoverageSignals{
		{AssetID: ybtcSAC, Volume7dUSD: 43_823, Trades7d: 11, Volume24hUSD: 5_000},
	}
	w := New(&fakeReader{sigs: sigs}, Options{
		SweepTimeout: time.Nanosecond, // sweepCtx is already past its deadline once probes run
		ResolveSAC: func(ctx context.Context, _ string) (string, bool) {
			if dl, ok := ctx.Deadline(); ok {
				probeDeadline = time.Until(dl)
			}
			return "yBTC-G", true
		},
		IsPriced: func(context.Context, string) (bool, error) { return true, nil },
	})
	w.Sweep(context.Background())
	if probeDeadline <= 0 {
		t.Fatalf("probe context deadline = %v, want > 0: the probe inherited an already-expired "+
			"sweepCtx instead of getting its own bounded context", probeDeadline)
	}
}
