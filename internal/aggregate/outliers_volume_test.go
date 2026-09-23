package aggregate

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

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

// dustOverBlockWindow is the RLT-277 worked example: one venue, one
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
// and become the published window: the survivors used to be the 4 wash
// prints alone, a served VWAP of 0.114 (+12.3 %) under the WarnPct.
func TestFilterOutliersLocal_DustCountMajorityCannotOverrideVolumeMajority(t *testing.T) {
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	trades := dustOverBlockWindow(t0)

	out := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4})
	if len(out) != 0 {
		v, _ := VWAP(out)
		t.Fatalf("contested window published %d of %d prints (%s of %s base units), VWAP %v; want it withheld",
			len(out), len(trades), baseSum(out), baseSum(trades), v)
	}
}

func TestFilterOutliers_DustCountMajorityCannotOverrideVolumeMajority(t *testing.T) {
	trades := dustOverBlockWindow(time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))

	out := FilterOutliers(trades, 4)
	if len(out) != 0 {
		v, _ := VWAP(out)
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
