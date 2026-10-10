package aggregate

import (
	"math/big"
	"math/rand"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Fixture helpers for the time-local filter. Prices are quote/base
// with base fixed at 10^7 so `quote` reads as price × 10^7 — an
// XLM/GBP shape (0.1337 → 0.1364) is 1_337_000 → 1_364_000.
const localFixtureBase = 10_000_000

func localTrade(source string, quote int64, ts time.Time) canonical.Trade {
	return canonical.Trade{
		Source:      source,
		Timestamp:   ts,
		BaseAmount:  canonical.NewAmount(big.NewInt(localFixtureBase)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

// noisy returns centre ± up to ~0.1% of deterministic pseudo-noise so
// the majority regime has a realistic (non-zero) MAD — the shape
// under which the whole-window band is tightest and the drift
// artifact is worst.
func noisy(centre int64, i int) int64 {
	return centre + int64((i*7919)%21-10)*(centre/10_000)
}

// thinStepSeries is the XLM/GBP drift shape replayed on a
// THIN, SINGLE-SOURCE series: one Kraken print per minute for 5 h at
// 0.1337, then a genuine +2% step to 0.1364 that the last `tailMin`
// minutes hold (the cross XLM/USD × GBP/USD moved with it — every
// print after the step agrees). Returns the trades in time order and
// the index of the first post-step print.
func thinStepSeries(t0 time.Time, totalMin, tailMin int) ([]canonical.Trade, int) {
	trades := make([]canonical.Trade, 0, totalMin)
	stepAt := totalMin - tailMin
	for i := 0; i < totalMin; i++ {
		centre := int64(1_337_000)
		if i >= stepAt {
			centre = 1_364_000
		}
		trades = append(trades, localTrade("kraken", noisy(centre, i), t0.Add(time.Duration(i)*time.Minute)))
	}
	return trades, stepAt
}

func maxZ(z []*big.Rat) *big.Rat {
	var m *big.Rat
	for _, v := range z {
		if v != nil && (m == nil || v.Cmp(m) > 0) {
			m = v
		}
	}
	return m
}

func TestFilterOutliersLocal_GenuineStepOnThinSingleSourceSeriesIsNotTrimmed(t *testing.T) {
	// Regression for the XLM/GBP drift artifact: a genuine +2%
	// step held by the newest 13% of a thin single-source window.
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	trades, stepAt := thinStepSeries(t0, 300, 40)
	const sigma = 4.0

	// The fixture must actually reproduce the defect under the
	// whole-window filter — otherwise the assertions below are vacuous.
	legacy := FilterOutliers(trades, sigma)
	if dropped := len(trades) - len(legacy); dropped == 0 {
		t.Fatalf("fixture does not reproduce the drift artifact: whole-window filter dropped 0 of the %d-print tail", len(trades)-stepAt)
	} else {
		t.Logf("whole-window FilterOutliers trimmed %d prints of a %d-print agreed step (the defect)", dropped, len(trades)-stepAt)
	}

	got := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: sigma})
	if len(got) != len(trades) {
		t.Errorf("local filter trimmed %d prints of an agreed +2%% step; want 0", len(trades)-len(got))
	}
	validIdx, z := outlierScores(trades, LocalOutlierOptions{Sigma: sigma})
	if len(validIdx) != len(trades) || len(z) != len(trades) {
		t.Fatalf("outlierScores: %d valid / %d scores, want %d", len(validIdx), len(z), len(trades))
	}
	sigmaRat := new(big.Rat).SetFloat64(sigma)
	if m := maxZ(z); m == nil || m.Cmp(sigmaRat) > 0 {
		f, _ := m.Float64()
		t.Errorf("max local z = %.3f exceeds sigma %v — an agreed step must not score as an outlier", f, sigma)
	}
	for k := stepAt; k < len(trades); k++ {
		if z[k].Cmp(sigmaRat) > 0 {
			f, _ := z[k].Float64()
			t.Errorf("post-step print %d scored z=%.3f > sigma", k, f)
		}
	}
	// The survivors must carry the new regime: the newest print's
	// price is the stepped level, so a window VWAP over the survivors
	// can follow the market instead of lagging it.
	if last := got[len(got)-1]; last.QuoteAmount.BigInt().Int64() < 1_360_000 {
		t.Errorf("newest survivor quote = %s, want the stepped regime (≥1_360_000)", last.QuoteAmount.String())
	}
}

func TestFilterOutliersLocal_SinglePrintSpikeOnThinSeriesIsTrimmed(t *testing.T) {
	// Same thin series, no step, ONE +10% print in the middle: that IS
	// an outlier (disagrees with the window AND its neighbours).
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	trades, _ := thinStepSeries(t0, 300, 0)
	const spikeAt = 150
	trades[spikeAt] = localTrade("kraken", 1_470_000, trades[spikeAt].Timestamp)
	const sigma = 4.0

	got := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: sigma})
	if len(got) != len(trades)-1 {
		t.Fatalf("got %d survivors, want %d (exactly the spike removed)", len(got), len(trades)-1)
	}
	for _, tr := range got {
		if tr.QuoteAmount.BigInt().Int64() == 1_470_000 {
			t.Fatalf("spike print survived the local filter")
		}
	}
	_, z := outlierScores(trades, LocalOutlierOptions{Sigma: sigma})
	sigmaRat := new(big.Rat).SetFloat64(sigma)
	if z[spikeAt] == nil || z[spikeAt].Cmp(sigmaRat) <= 0 {
		t.Errorf("spike z = %v, want > sigma %v", z[spikeAt], sigma)
	}
	// Preserved input order among survivors.
	for k := 1; k < len(got); k++ {
		if got[k].Timestamp.Before(got[k-1].Timestamp) {
			t.Fatalf("survivor order not preserved at %d", k)
		}
	}
}

func TestFilterOutliersLocal_DenseMultiSourceTailAndFatFinger(t *testing.T) {
	// The design's red-proof shape: 8 400 old-regime + 1 600 new-regime
	// prints (16% tail) across three agreeing venues at ~50 prints/min,
	// plus ONE fat-finger 2× print inside the shift. Expect: 0 agreed
	// prints dropped, the fat-finger dropped.
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	sources := []string{"binance", "coinbase", "kraken"}
	const total, tail = 10_000, 1_600
	trades := make([]canonical.Trade, 0, total+1)
	for i := 0; i < total; i++ {
		centre := int64(1_875_000) // 0.1875
		if i >= total-tail {
			centre = 1_828_125 // −2.5%
		}
		ts := t0.Add(time.Duration(i) * 1200 * time.Millisecond) // 50/min
		trades = append(trades, localTrade(sources[i%3], noisy(centre, i), ts))
	}
	fat := localTrade("kraken", 3_656_250, trades[total-tail/2].Timestamp) // 2× inside the shift
	trades = append(trades, fat)

	got := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4})
	if len(got) != total {
		t.Errorf("got %d survivors, want %d (only the fat-finger removed)", len(got), total)
	}
	for _, tr := range got {
		if tr.QuoteAmount.BigInt().Int64() == 3_656_250 {
			t.Fatalf("fat-finger survived")
		}
	}
}

func TestFilterOutliersLocal_MidBucketStepUsesNextBucketReference(t *testing.T) {
	// A step landing 48 s into a 1-minute bucket leaves the new-regime
	// prints a 20% minority of their OWN bucket; the following bucket
	// is their honest reference. 5 prints/min, two venues.
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	trades := make([]canonical.Trade, 0, 600)
	stepAt := 60*5 + 4 // 60 full old buckets, then 4 old prints into bucket 61
	for i := 0; i < 600; i++ {
		centre := int64(1_337_000)
		if i >= stepAt {
			centre = 1_364_000
		}
		src := "kraken"
		if i%2 == 1 {
			src = "coinbase"
		}
		trades = append(trades, localTrade(src, noisy(centre, i), t0.Add(time.Duration(i)*12*time.Second)))
	}
	got := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4})
	if len(got) != len(trades) {
		t.Errorf("mid-bucket step: %d agreed prints trimmed, want 0", len(trades)-len(got))
	}
}

func TestFilterOutliersLocal_LegacyParityOnUntimedInputs(t *testing.T) {
	// Untimed trades (zero Timestamp — the /v1 handler tests' shape)
	// all share one bucket, so the local filter must reproduce the
	// whole-window verdicts exactly: masking case dropped, honest
	// dispersion kept, zero-base skipped, short input passed through.
	mk := func(quote int64) canonical.Trade { return localTrade("x", quote, time.Time{}) }
	masking := []canonical.Trade{mk(100), mk(100), mk(100), mk(100), mk(200)}
	if got := FilterOutliersLocal(masking, LocalOutlierOptions{Sigma: 4}); len(got) != 4 {
		t.Errorf("masking [100×4,200]: %d survivors, want 4", len(got))
	}
	honest := []canonical.Trade{mk(100), mk(100), mk(100), mk(100), mk(101)}
	if got := FilterOutliersLocal(honest, LocalOutlierOptions{Sigma: 4}); len(got) != 5 {
		t.Errorf("honest [100×4,101]: %d survivors, want 5", len(got))
	}
	zeroBase := localTrade("x", 100, time.Time{})
	zeroBase.BaseAmount = canonical.NewAmount(big.NewInt(0))
	withZero := append([]canonical.Trade{zeroBase}, honest...)
	if got := FilterOutliersLocal(withZero, LocalOutlierOptions{Sigma: 4}); len(got) != 5 {
		t.Errorf("zero-base: %d survivors, want 5", len(got))
	}
	for _, n := range []int{0, 1, 2} {
		if got := FilterOutliersLocal(honest[:n], LocalOutlierOptions{Sigma: 4}); len(got) != n {
			t.Errorf("n=%d passthrough: got %d", n, len(got))
		}
	}
	if got := FilterOutliersLocal(masking, LocalOutlierOptions{Sigma: 0}); len(got) != 5 {
		t.Errorf("sigma=0 must be a no-op, got %d", len(got))
	}
}

const diffSigma = 4.0

func survivorKey(tr canonical.Trade) string {
	return tr.Source + "|" + tr.Timestamp.UTC().Format(time.RFC3339Nano) + "|" + tr.QuoteAmount.String() + "|" + tr.BaseAmount.String()
}

func survivorSet(trades []canonical.Trade) map[string]int {
	m := make(map[string]int, len(trades))
	for _, tr := range trades {
		m[survivorKey(tr)]++
	}
	return m
}

func vwapText(t *testing.T, trades []canonical.Trade) string {
	t.Helper()
	if len(trades) == 0 {
		return "<empty>"
	}
	v, err := VWAPOf(trades)
	if err != nil {
		t.Fatalf("VWAP: %v", err)
	}
	return v.FloatString(12)
}

func assertIdentical(t *testing.T, name string, trades []canonical.Trade) {
	t.Helper()
	legacy := FilterOutliers(trades, diffSigma)
	local := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: diffSigma})
	if len(legacy) != len(local) {
		t.Errorf("%s: legacy kept %d, local kept %d", name, len(legacy), len(local))
	}
	for k := range legacy {
		if k < len(local) && survivorKey(legacy[k]) != survivorKey(local[k]) {
			t.Errorf("%s: survivor %d differs: legacy %s, local %s", name, k, survivorKey(legacy[k]), survivorKey(local[k]))
			break
		}
	}
	lv, ov := vwapText(t, legacy), vwapText(t, local)
	if lv != ov {
		t.Errorf("%s: VWAP over survivors differs: legacy %s, local %s", name, lv, ov)
	}
	t.Logf("%s: %d prints, %d survivors, VWAP %s (identical)", name, len(trades), len(local), ov)
}

func TestFilterOutliersLocal_IdenticalToLegacyOnUnaffectedShapes(t *testing.T) {
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	sources := []string{"binance", "coinbase", "kraken"}

	// Normal: three agreeing venues, ±0.1 % noise, 50 prints/min, 1 h.
	normal := make([]canonical.Trade, 0, 3000)
	for i := 0; i < 3000; i++ {
		normal = append(normal, localTrade(sources[i%3], noisy(1_875_000, i), t0.Add(time.Duration(i)*1200*time.Millisecond)))
	}
	assertIdentical(t, "normal", normal)

	// Normal + two fat-fingers (2× and 0.5×) from different venues.
	fat := append([]canonical.Trade(nil), normal...)
	fat = append(fat,
		localTrade("kraken", 3_750_000, normal[1500].Timestamp),
		localTrade("binance", 937_500, normal[2200].Timestamp))
	assertIdentical(t, "fat-finger", fat)

	// Zero-MAD: every print at one price (a pegged pair / one resting
	// order), plus a 1 % print (kept by the zero-MAD floor) and a 3 %
	// print (dropped).
	zero := make([]canonical.Trade, 0, 300)
	for i := 0; i < 300; i++ {
		zero = append(zero, localTrade("sdex", 10_000_000, t0.Add(time.Duration(i)*20*time.Second)))
	}
	zero = append(zero,
		localTrade("sdex", 10_100_000, t0.Add(50*time.Minute)),
		localTrade("sdex", 10_300_000, t0.Add(70*time.Minute)))
	assertIdentical(t, "zero-MAD", zero)

	// Tight: ±0.02 % cluster and a lone +1.5 % print — outside the
	// legacy band (MAD ~0.01 % → ±0.06 %) AND outside the local band
	// (0.25 % floor → ±1 %), so both drop it.
	tight := make([]canonical.Trade, 0, 600)
	for i := 0; i < 600; i++ {
		tight = append(tight, localTrade(sources[i%3], 1_875_000+int64((i*7919)%5-2)*75, t0.Add(time.Duration(i)*6*time.Second)))
	}
	tight = append(tight, localTrade("kraken", 1_903_125, t0.Add(30*time.Minute)))
	assertIdentical(t, "tight", tight)
}

func TestFilterOutliersLocal_LegacySurvivorsAreAlwaysLocalSurvivors(t *testing.T) {
	// 200 random series: random venue count (1–3), cadence, noise level,
	// optional step, optional wild prints. Legacy survivors ⊆ local
	// survivors on every one of them.
	rng := rand.New(rand.NewSource(20260828)) //nolint:gosec // deterministic fixture generator, not security
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	checked := 0
	for series := 0; series < 200; series++ {
		n := 50 + rng.Intn(1500)
		venues := 1 + rng.Intn(3)
		cadence := time.Duration(5+rng.Intn(120)) * time.Second
		noiseBps := 1 + rng.Intn(50)
		stepAt, stepPct := n, 0.0
		if rng.Intn(2) == 0 {
			stepAt = rng.Intn(n)
			stepPct = (rng.Float64() - 0.5) * 0.12 // ±6 %
		}
		trades := make([]canonical.Trade, 0, n+10)
		for i := 0; i < n; i++ {
			centre := 1_337_000.0
			if i >= stepAt {
				centre *= 1 + stepPct
			}
			q := int64(centre * (1 + float64(rng.Intn(2*noiseBps+1)-noiseBps)/10_000))
			src := []string{"kraken", "coinbase", "sdex"}[rng.Intn(venues)]
			trades = append(trades, localTrade(src, q, t0.Add(time.Duration(i)*cadence)))
		}
		for w := rng.Intn(6); w > 0; w-- {
			at := rng.Intn(n)
			q := int64(float64(trades[at].QuoteAmount.BigInt().Int64()) * (0.3 + rng.Float64()*3))
			trades = append(trades, localTrade("kraken", q, trades[at].Timestamp))
		}
		legacy := survivorSet(FilterOutliers(trades, diffSigma))
		local := survivorSet(FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: diffSigma}))
		for key, cnt := range legacy {
			if local[key] < cnt {
				t.Fatalf("series %d: legacy survivor %s (×%d) missing from local survivors (×%d)", series, key, cnt, local[key])
			}
		}
		checked++
	}
	if checked != 200 {
		t.Fatalf("checked %d of 200 series", checked)
	}
	t.Logf("legacy-survivors ⊆ local-survivors held on %d of 200 random series", checked)
}

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

// tokenFarmSeries is the observed SDEX token-farm signature on a
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
	// token-farm shape before the counter-based storm gate could be
	// retired. This test is the source of the numbers in
	// deploy/monitoring/rule-tests/aggregator_test.yml (trim_fraction
	// case 4, the token-farm fixture): the
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

// sizedTrade is a print of `base` smallest units at price quote/base,
// with quote given as price × base via num/den so the price is exact.
func sizedTrade(source string, base int64, num, den int64, ts time.Time) canonical.Trade {
	q := new(big.Int).Mul(big.NewInt(base), big.NewInt(num))
	q.Quo(q, big.NewInt(den))
	return canonical.Trade{
		Source:      source,
		Timestamp:   ts,
		BaseAmount:  canonical.NewAmount(big.NewInt(base)),
		QuoteAmount: canonical.NewAmount(q),
	}
}

// dustOverBlockWindow is the dust-over-block worked example: one venue, one
// 5 m window, sigma 4 — 3 honest prints of 1,000,000 XLM at 0.100
// ($300k) and 4 wash prints of 30,000 XLM at 0.114 ($13.7k), all
// inside one 1 m bucket. The count median is the wash level.
func dustOverBlockWindow(t0 time.Time) []canonical.Trade {
	const stroops = 10_000_000
	var trades []canonical.Trade
	for i := 0; i < 3; i++ {
		trades = append(trades, sizedTrade("sdex", 1_000_000*stroops, 100, 1000, t0.Add(time.Duration(i)*10*time.Second)))
	}
	for i := 0; i < 4; i++ {
		trades = append(trades, sizedTrade("sdex", 30_000*stroops, 114, 1000, t0.Add(time.Duration(i)*10*time.Second+5*time.Second)))
	}
	return trades
}

func baseSum(trades []canonical.Trade) *big.Int {
	s := new(big.Int)
	for i := range trades {
		s.Add(s, trades[i].BaseAmount.BigInt())
	}
	return s
}

// A count majority of dust prints must not delete a volume majority
// and become the published window: a count-only filter keeps the 4 wash
// prints alone, a served VWAP of 0.114 (+12.3 %) under the WarnPct.
func TestFilterOutliersLocal_DustCountMajorityCannotOverrideVolumeMajority(t *testing.T) {
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	trades := dustOverBlockWindow(t0)

	out := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4})
	if len(out) != 0 {
		v, _ := VWAPOf(out)
		t.Fatalf("contested window published %d of %d prints (%s of %s base units), VWAP %v; want it withheld",
			len(out), len(trades), baseSum(out), baseSum(trades), v)
	}
}

func TestFilterOutliers_DustCountMajorityCannotOverrideVolumeMajority(t *testing.T) {
	trades := dustOverBlockWindow(time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))

	out := FilterOutliers(trades, 4)
	if len(out) != 0 {
		v, _ := VWAPOf(out)
		t.Fatalf("contested window published %d of %d prints, VWAP %v; want it withheld", len(out), len(trades), v)
	}
}

// The mirror image — one large print that is a count minority but the
// volume majority — is equally contested. Serving the retail prints
// alone would publish a price that ignores most of the money traded;
// serving the large print would let one wash trade set the price
// (ADR-0046 §5). Neither side is published.
func TestFilterOutliersLocal_LoneVolumeMajorityPrintWithholdsWindow(t *testing.T) {
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	trades := []canonical.Trade{sizedTrade("sdex", 5_000_000, 200, 1000, t0)}
	for i := 1; i <= 4; i++ {
		trades = append(trades, sizedTrade("sdex", 100_000, 100, 1000, t0.Add(time.Duration(i)*time.Second)))
	}

	if out := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4}); len(out) != 0 {
		t.Fatalf("published %d prints of a window whose volume majority was trimmed; want it withheld", len(out))
	}
}

// Ordinary trimming — a dust-sized fat finger in an honest window —
// is unchanged: the guard only fires when the trim removes more base
// volume than it keeps.
func TestFilterOutliersLocal_DustFatFingerStillTrimmedAlone(t *testing.T) {
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	trades := make([]canonical.Trade, 0, 6)
	for i := 0; i < 5; i++ {
		trades = append(trades, sizedTrade("sdex", 1_000_000, 100, 1000, t0.Add(time.Duration(i)*time.Second)))
	}
	trades = append(trades, sizedTrade("sdex", 10_000, 200, 1000, t0.Add(6*time.Second)))

	out := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4})
	if len(out) != 5 {
		t.Fatalf("kept %d, want the 5 honest prints", len(out))
	}
	for i := range out {
		if out[i].BaseAmount.BigInt().Int64() != 1_000_000 {
			t.Fatalf("fat finger survived: %+v", out[i])
		}
	}
}

// Base volume is compared at one smallest-unit scale. A CEX print
// stamped at 1e8 carries 10× the raw base units of an equal on-chain
// 1e7 print, so on raw units the 8dp wash prints below outweigh the
// honest block; on real volume they are a minority and the window is
// contested.
func TestFilterOutliersLocal_VolumeGuardComparesAtCommonScale(t *testing.T) {
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	var trades []canonical.Trade
	for i := 0; i < 3; i++ {
		trades = append(trades, sizedTrade("onchain7", 1_000_000*10_000_000, 100, 1000, t0.Add(time.Duration(i)*time.Second)))
	}
	for i := 0; i < 4; i++ {
		trades = append(trades, sizedTrade("cex8", 400_000*100_000_000, 114, 1000, t0.Add(time.Duration(i)*time.Second+500*time.Millisecond)))
	}
	scale := func(source string) int {
		if source == "cex8" {
			return 8
		}
		return 7
	}

	if out := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4}); len(out) != 4 {
		t.Fatalf("raw-unit comparison: kept %d, want the 4 raw-majority prints (fixture sanity)", len(out))
	}
	if out := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4, AmountScaleDecimals: scale}); len(out) != 0 {
		t.Fatalf("common-scale comparison published %d wash prints over 3,000,000 XLM of honest volume; want withheld", len(out))
	}
}

// Property: whatever either filter publishes carries at least as much
// base volume as it trimmed away. Exhaustive over small windows mixing
// three price levels and three size magnitudes.
func TestOutlierFilters_NeverPublishAVolumeMinority(t *testing.T) {
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	prices := []int64{100, 103, 140}
	sizes := []int64{1_000, 100_000, 10_000_000}
	cases := 0
	for n := 3; n <= 6; n++ {
		combos := 1
		for i := 0; i < n; i++ {
			combos *= len(prices) * len(sizes)
		}
		for c := 0; c < combos; c += 97 {
			trades := make([]canonical.Trade, n)
			x := c
			for i := 0; i < n; i++ {
				k := x % (len(prices) * len(sizes))
				x /= len(prices) * len(sizes)
				trades[i] = sizedTrade("sdex", sizes[k%len(sizes)], prices[k/len(sizes)], 1000, t0.Add(time.Duration(i)*time.Second))
			}
			total := baseSum(trades)
			for name, out := range map[string][]canonical.Trade{
				"FilterOutliers":      FilterOutliers(trades, 4),
				"FilterOutliersLocal": FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4}),
			} {
				if len(out) == 0 {
					continue
				}
				kept := baseSum(out)
				dropped := new(big.Int).Sub(total, kept)
				if kept.Cmp(dropped) < 0 {
					t.Fatalf("%s published %d of %d prints carrying %s base units while trimming %s", name, len(out), n, kept, dropped)
				}
			}
			cases++
		}
	}
	if cases < 100 {
		t.Fatalf("property covered only %d windows", cases)
	}
}

func TestFilterOutliers_BandEdgesAreInclusiveAndRatioSymmetric(t *testing.T) {
	// Majority at 100 → MAD 0 → scale = 100/200 = 0.5 → threshold at
	// sigma 4 is 2, so the band is [100²/102, 102] exactly.
	mk := func(hash string, base, quote int64) canonical.Trade {
		tr := mkOrderedTrade("s", 1, hash, 0, time.Unix(0, 0), quote)
		tr.BaseAmount = canonical.NewAmount(big.NewInt(base))
		return tr
	}
	// The centre outweighs every probe, so a dropped probe never trips
	// the volume-majority withhold.
	centre := []canonical.Trade{mk("a", 1000, 100000), mk("b", 1000, 100000), mk("c", 1000, 100000), mk("d", 1000, 100000)}
	cases := []struct {
		name        string
		base, quote int64
		keep        bool
	}{
		{"upper edge 102", 1, 102, true},
		{"just above 102", 1000, 102001, false},
		{"lower edge 100²/102", 102, 10000, true},
		{"just below 100²/102", 103, 10000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append(append([]canonical.Trade{}, centre...), mk("x", tc.base, tc.quote))
			got := FilterOutliers(in, 4)
			kept := len(got) == 5
			if kept != tc.keep || len(got) < 4 {
				t.Fatalf("kept %d/5, want probe kept=%v", len(got), tc.keep)
			}
		})
	}
}
