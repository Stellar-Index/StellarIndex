package aggregate

import (
	"math/rand"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Differential guard, legacy [FilterOutliers] vs [FilterOutliersLocal]:
//
//   - on the shapes the local filter was NOT built to change — a normal
//     multi-venue window, a fat-finger, a zero-MAD cluster, a tight
//     cluster with a lone wild print — its survivor set and the VWAP
//     over it are BYTE-IDENTICAL to the legacy filter's. This is what
//     lets the orchestrator switch filters without moving any served
//     value on an unaffected target;
//   - on ANY series, every print the legacy filter keeps the local
//     filter keeps too (the window band is always in the reference
//     set), so the switch can only ever ADD survivors — never trim a
//     print the whole-window band accepted.

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
	v, err := VWAP(trades)
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
	// order), plus a 1 % print (kept by the MNY-22 floor) and a 3 %
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
