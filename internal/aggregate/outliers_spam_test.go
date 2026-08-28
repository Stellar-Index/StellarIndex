package aggregate

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Spam-bucket fixtures for the time-local filter (the design's
// "needs the spam-bucket test" risk). A local reference is built from
// a print's OWN neighbourhood, so without anchoring a burst that is
// the majority of its own 1-minute bucket — ≥ MinBucket prints at any
// wrong level — sets its own centre and validates itself at ANY
// density. These cases pin the anchoring rule: a local reference is
// only trusted when its centre sits within the anchor tolerance of the
// window median or of the previous anchored reference (chain
// continuity), and its band can never be wider than
// sigma × [localScaleRelCeiling].

const spamSigma = 4.0

func countQuote(trades []canonical.Trade, pred func(int64) bool) int {
	n := 0
	for _, tr := range trades {
		if pred(tr.QuoteAmount.BigInt().Int64()) {
			n++
		}
	}
	return n
}

func TestFilterOutliersLocal_DenseSingleVenueWashBurstIsDropped(t *testing.T) {
	// SDEX-only pair, 2 honest prints/min for 6 h at 0.1337. One 1 m
	// bucket in the middle additionally carries SIX wash prints at
	// 2.5× (the same venue self-trading) — the majority of that
	// bucket. Before anchoring the bucket's own median WAS the wash
	// level, so every wash print scored z≈0 and survived; the honest
	// prints must survive untouched throughout.
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	const honestPerMin, minutes = 2, 360
	trades := make([]canonical.Trade, 0, honestPerMin*minutes+6)
	for i := 0; i < honestPerMin*minutes; i++ {
		ts := t0.Add(time.Duration(i) * (time.Minute / honestPerMin))
		trades = append(trades, localTrade("sdex", noisy(1_337_000, i), ts))
	}
	const washQuote = 3_342_500 // 2.5×
	burstAt := t0.Add(180 * time.Minute)
	for j := 0; j < 6; j++ {
		trades = append(trades, localTrade("sdex", washQuote+int64(j)*100, burstAt.Add(time.Duration(j)*7*time.Second)))
	}
	honest := len(trades) - 6

	got := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: spamSigma})
	if n := countQuote(got, func(q int64) bool { return q >= washQuote }); n != 0 {
		t.Errorf("%d of 6 wash prints at 2.5× survived — a spam-majority bucket validated itself", n)
	}
	if n := countQuote(got, func(q int64) bool { return q < washQuote }); n != honest {
		t.Errorf("honest survivors = %d, want %d", n, honest)
	}
}

func TestFilterOutliersLocal_MixedVenueSpamBurstIsDropped(t *testing.T) {
	// Two honest venues (kraken, coinbase) at 2 prints/min each plus a
	// spam venue that fires TEN prints at 2.5× inside one bucket —
	// the burst dominates that bucket (10 of 14). Every spam print
	// must go; every honest print (including the four inside the
	// spam-dominated bucket, which lean on the adjacent buckets and
	// the window) must stay.
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	const minutes = 240
	trades := make([]canonical.Trade, 0, 4*minutes+10)
	for i := 0; i < 4*minutes; i++ {
		src := "kraken"
		if i%2 == 1 {
			src = "coinbase"
		}
		trades = append(trades, localTrade(src, noisy(1_337_000, i), t0.Add(time.Duration(i)*15*time.Second)))
	}
	const spamQuote = 3_342_500
	burstAt := t0.Add(120 * time.Minute)
	for j := 0; j < 10; j++ {
		trades = append(trades, localTrade("spamvenue", spamQuote+int64(j)*250, burstAt.Add(time.Duration(j)*5*time.Second)))
	}
	honest := len(trades) - 10

	got := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: spamSigma})
	for _, tr := range got {
		if tr.Source == "spamvenue" {
			t.Fatalf("spam-venue print %s survived", tr.QuoteAmount.String())
		}
	}
	if len(got) != honest {
		t.Errorf("honest survivors = %d, want %d", len(got), honest)
	}
}

// tokenFarmSeries is the 2026-08-14 SDEX token-farm signature on a
// SINGLE configured pair: an honest print every 2 minutes for 24 h at
// 0.1337 (720 prints), plus a 2 h wave of dust-sized self-trades at
// 4 prints/min (480 prints) whose CONSECUTIVE prices gap by 25–37 %
// (alternating direction, deterministic, reflected so the walk stays
// within 0.4×–2.5× of the honest level). Returns the trades in time
// order and the count of wave prints.
func tokenFarmSeries(t0 time.Time) ([]canonical.Trade, int) {
	trades := make([]canonical.Trade, 0, 720+480)
	for i := 0; i < 720; i++ {
		trades = append(trades, localTrade("sdex", noisy(1_337_000, i), t0.Add(time.Duration(i)*2*time.Minute)))
	}
	waveStart := t0.Add(10 * time.Hour)
	price := 1_337_000.0
	for j := 0; j < 480; j++ {
		gap := 0.25 + float64((j*7919)%13)/100 // 0.25 … 0.37
		if j%2 == 0 {
			price *= 1 + gap
		} else {
			price *= 1 - gap
		}
		// Reflect off the walk bounds so the wave disperses around the
		// honest level instead of decaying (each ± pair nets ≈ −gap²).
		if price < 1_337_000*0.4 {
			price = 1_337_000 * 2.2
		}
		if price > 1_337_000*2.5 {
			price = 1_337_000 * 0.45
		}
		tr := localTrade("sdex", int64(price), waveStart.Add(time.Duration(j)*15*time.Second))
		tr.BaseAmount = canonical.NewAmount(big.NewInt(1_000)) // dust
		tr.QuoteAmount = canonical.NewAmount(big.NewInt(int64(price) / 10_000))
		trades = append(trades, tr)
	}
	// Restore time order (the wave was appended after the honest run).
	ordered := make([]canonical.Trade, 0, len(trades))
	h, w := 0, 720
	for h < 720 || w < len(trades) {
		if w >= len(trades) || (h < 720 && !trades[w].Timestamp.Before(trades[h].Timestamp)) {
			ordered = append(ordered, trades[h])
			h++
		} else {
			ordered = append(ordered, trades[w])
			w++
		}
	}
	return ordered, 480
}

func TestFilterOutliersLocal_TokenFarmWaveTrimShareMatchesLegacy(t *testing.T) {
	// The design required the trim-fraction alert to be PROVEN on the
	// 2026-08-14 shape before the counter-based storm gate could be
	// retired. This test is the source of the numbers in
	// deploy/monitoring/rule-tests/aggregator_test.yml (trim_fraction
	// case "the 2026-08-14 token-farm fixture fires"): the
	// stage=class value is the fixture's print count and the
	// stage=outlier value is the survivor count pinned below. Change
	// one and the other must follow.
	t0 := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	trades, wave := tokenFarmSeries(t0)
	const (
		wantClass    = 1200
		wantSurvived = 720
	)
	if len(trades) != wantClass {
		t.Fatalf("fixture has %d prints, want %d", len(trades), wantClass)
	}

	legacy := FilterOutliers(trades, spamSigma)
	local := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: spamSigma})
	legacyDropped := len(trades) - len(legacy)
	localDropped := len(trades) - len(local)
	t.Logf("token-farm fixture: %d prints (%d wave); legacy dropped %d, local dropped %d — window_trades{stage=class}=%d, {stage=outlier}=%d",
		len(trades), wave, legacyDropped, localDropped, len(trades), len(local))

	if legacyDropped < wave*9/10 {
		t.Fatalf("fixture does not reproduce the wave under the whole-window filter: legacy dropped %d of %d", legacyDropped, wave)
	}
	// Comparable trim share: the local filter may admit the handful of
	// wave prints that land within the anchor tolerance of the honest
	// level, never more than 10 % of what the legacy band removes.
	if localDropped < legacyDropped*9/10 {
		t.Errorf("local dropped %d, legacy %d — the wave validated itself locally", localDropped, legacyDropped)
	}
	// No honest print is collateral (honest prints are the 10^7-base
	// ones; wave prints are dust-sized).
	honestSurvived := 0
	for _, tr := range local {
		if tr.BaseAmount.BigInt().Int64() == localFixtureBase {
			honestSurvived++
		}
	}
	if honestSurvived != wantClass-wave {
		t.Errorf("honest survivors = %d, want %d", honestSurvived, wantClass-wave)
	}
	// The trim-fraction alert's gate: 1 − outlier/class > 0.2 with
	// class ≥ 20. Pinned exactly so the promtool case cannot drift.
	if len(local) != wantSurvived {
		t.Errorf("stage=outlier survivors = %d, want %d (update the promtool token-farm case in lockstep)", len(local), wantSurvived)
	}
	if frac := 1 - float64(len(local))/float64(len(trades)); frac <= 0.2 {
		t.Errorf("trim fraction %.3f would not fire outlier_trim_fraction (> 0.2)", frac)
	}
}
