package v1_test

import (
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const vwapBDBase = "/v1/vwap?base=native&quote=fiat:USD"

func bdTrade(source string, ts time.Time, base, quote int64) canonical.Trade {
	t := mkVWAPTrade(base, quote)
	t.Source = source
	t.Timestamp = ts
	return t
}

func bdGet(t *testing.T, reader *stubHistoryReader, url string) v1.VWAPResult {
	t.Helper()
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))
	resp := mustGet(t, ts.URL+url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, url)
	}
	var env struct {
		Data v1.VWAPResult `json:"data"`
	}
	mustDecode(t, resp, &env)
	return env.Data
}

func ratOf(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("not a decimal: %q", s)
	}
	return r
}

func TestVWAPBreakdown_AbsentWithoutParam(t *testing.T) {
	reader := &stubHistoryReader{trades: []canonical.Trade{mkVWAPTrade(20, 40), mkVWAPTrade(100, 300)}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))
	resp := mustGet(t, ts.URL+vwapBDBase)
	body, _ := readAll(resp)
	if strings.Contains(body, "breakdown") {
		t.Errorf("response without ?breakdown= must not mention breakdown: %s", body)
	}
}

func TestVWAPBreakdown_SourcesSumToBucketAndWeightsToOne(t *testing.T) {
	t0 := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubHistoryReader{trades: []canonical.Trade{
		bdTrade("soroswap", t0, 10, 30),
		bdTrade("soroswap", t0.Add(time.Second), 20, 50),
		bdTrade("sdex", t0.Add(2*time.Second), 5, 20),
	}}
	res := bdGet(t, reader, vwapBDBase+"&breakdown=source")
	bd := res.Breakdown
	if bd == nil || len(bd.Buckets) != 1 || bd.Interval != nil || bd.Truncated {
		t.Fatalf("breakdown = %+v, want one bucket, null interval, not truncated", bd)
	}
	b := bd.Buckets[0]
	if b.QuoteVolume != res.QuoteVolume || b.TradeCount != res.TradeCount {
		t.Errorf("bucket (%s,%d) != headline (%s,%d)", b.QuoteVolume, b.TradeCount, res.QuoteVolume, res.TradeCount)
	}
	sumQ, sumW := new(big.Int), new(big.Rat)
	for _, s := range b.Sources {
		q, _ := new(big.Int).SetString(s.QuoteVolume, 10)
		sumQ.Add(sumQ, q)
		sumW.Add(sumW, ratOf(t, s.Weight))
	}
	if sumQ.String() != res.QuoteVolume {
		t.Errorf("Σ source quote = %s, want %s", sumQ, res.QuoteVolume)
	}
	if d := new(big.Rat).Sub(big.NewRat(1, 1), sumW); d.Sign() < 0 || d.Cmp(big.NewRat(1, 1_000_000_000)) > 0 {
		t.Errorf("Σ weights = %s, want 1 within 1e-9", sumW.FloatString(12))
	}
	top := b.Sources[0]
	if top.Source != "soroswap" || top.QuoteVolume != "80" || top.BaseVolume != "30" || top.TradeCount != 2 ||
		top.Weight != "0.8000000000" || top.OutliersExcluded != 0 || top.Price == nil || *top.Price != "2.6666666666" {
		t.Errorf("soroswap = %+v", top)
	}
}

func TestVWAPBreakdown_IntervalBuckets(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC)
	reader := &stubHistoryReader{trades: []canonical.Trade{
		bdTrade("soroswap", t0, 1, 2),
		bdTrade("sdex", t0.Add(time.Hour), 1, 4),
	}}
	bd := bdGet(t, reader, vwapBDBase+"&breakdown=source&interval=1h").Breakdown
	if bd.Interval == nil || *bd.Interval != "1h" || len(bd.Buckets) != 2 {
		t.Fatalf("breakdown = %+v, want 2 buckets at 1h", bd)
	}
	if got := time.Time(bd.Buckets[0].Start); !got.Equal(time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("first bucket start = %v", got)
	}
	if bd.Buckets[0].Sources[0].Source != "soroswap" || bd.Buckets[1].Sources[0].Source != "sdex" {
		t.Errorf("buckets not in ascending time order: %+v", bd.Buckets)
	}
}

func TestVWAPBreakdown_MixedDecimalsWeightsNormalised(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatal(err)
	}
	xlm, _ := canonical.ParseAsset("native")
	pair, _ := canonical.NewPair(xlm, usdc)
	// Same real value: 100 at 7 decimals == 1000 at 8 decimals.
	on := bdTrade("soroswap", time.Unix(1_772_000_000, 0).UTC(), 10, 100)
	cex := bdTrade("binance", time.Unix(1_772_000_001, 0).UTC(), 100, 1000)
	on.Pair, cex.Pair = pair, pair
	res := bdGet(t, &stubHistoryReader{trades: []canonical.Trade{on, cex}},
		"/v1/vwap?base=native&quote="+w2t2USDC+"&breakdown=source")
	for _, s := range res.Breakdown.Buckets[0].Sources {
		if ratOf(t, s.Weight).Cmp(big.NewRat(1, 2)) != 0 {
			t.Errorf("%s weight = %s, want 0.5 after scale normalisation", s.Source, s.Weight)
		}
		// Lifted to the window's 8dp scale, the units of the headline sums.
		if s.QuoteVolume != "1000" {
			t.Errorf("%s quote_volume = %s, want 1000 at 8dp", s.Source, s.QuoteVolume)
		}
	}
	if res.QuoteVolume != "2000" {
		t.Errorf("quote_volume = %s, want 2000 (the sources sum to it)", res.QuoteVolume)
	}
}

func TestVWAPBreakdown_OutlierExclusionsPerSource(t *testing.T) {
	t0 := time.Unix(1_772_000_000, 0).UTC()
	baseline := []int64{100, 101, 99, 100, 102, 98, 101, 100, 99, 100, 101, 100, 99, 101, 100, 102, 99, 100, 101, 100}
	var trades []canonical.Trade
	for i, p := range baseline {
		trades = append(trades, bdTrade("sdex", t0.Add(time.Duration(i)*time.Second), 1, p))
	}
	trades = append(trades, bdTrade("soroswap", t0.Add(time.Minute), 1, 10_000))

	res := bdGet(t, &stubHistoryReader{trades: trades}, vwapBDBase+"&breakdown=source&outlier_sigma=3")
	if res.OutliersFiltered != 1 {
		t.Fatalf("OutliersFiltered = %d, want 1", res.OutliersFiltered)
	}
	got := map[string]v1.VWAPSourceBreakdown{}
	for _, s := range res.Breakdown.Buckets[0].Sources {
		got[s.Source] = s
	}
	if s := got["sdex"]; s.OutliersExcluded != 0 || s.TradeCount != 20 {
		t.Errorf("sdex = %+v, want 0 excluded / 20 trades", s)
	}
	s := got["soroswap"]
	if s.OutliersExcluded != 1 || s.TradeCount != 0 || s.Price != nil || s.Weight != "0" || s.QuoteVolume != "0" {
		t.Errorf("soroswap = %+v, want fully excluded, null price, weight 0", s)
	}

	// Without sigma nothing is excluded.
	res = bdGet(t, &stubHistoryReader{trades: trades}, vwapBDBase+"&breakdown=source")
	for _, s := range res.Breakdown.Buckets[0].Sources {
		if s.OutliersExcluded != 0 {
			t.Errorf("%s excluded %d with sigma 0", s.Source, s.OutliersExcluded)
		}
	}
}

func TestVWAPBreakdown_TruncatedFlag(t *testing.T) {
	t0 := time.Unix(1_772_000_000, 0).UTC()
	trades := make([]canonical.Trade, 10000)
	for i := range trades {
		trades[i] = bdTrade("sdex", t0, 1, 2)
	}
	res := bdGet(t, &stubHistoryReader{trades: trades}, vwapBDBase+"&breakdown=source")
	if !res.Truncated || !res.Breakdown.Truncated {
		t.Errorf("truncated = %v / breakdown %v, want both true at the fetch cap", res.Truncated, res.Breakdown.Truncated)
	}
	res = bdGet(t, &stubHistoryReader{trades: trades[:9999]}, vwapBDBase+"&breakdown=source")
	if res.Breakdown.Truncated {
		t.Error("breakdown.truncated true below the cap")
	}
}

func TestVWAPBreakdown_ParamValidation400(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{History: &stubHistoryReader{trades: []canonical.Trade{mkVWAPTrade(1, 2)}}}))
	for _, q := range []string{
		"&breakdown=venue",
		"&breakdown=source&interval=7m",
		"&interval=1h",
	} {
		resp := mustGet(t, ts.URL+vwapBDBase+q)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestVWAPBreakdown_MixedDecimalsRankedByWeight(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatal(err)
	}
	xlm, _ := canonical.ParseAsset("native")
	pair, _ := canonical.NewPair(xlm, usdc)
	// 300 at 7 decimals outweighs 1000 at 8 decimals (== 100 at 7), so a
	// raw-volume sort would put binance first.
	on := bdTrade("soroswap", time.Unix(1_772_000_000, 0).UTC(), 30, 300)
	cex := bdTrade("binance", time.Unix(1_772_000_001, 0).UTC(), 100, 1000)
	on.Pair, cex.Pair = pair, pair
	res := bdGet(t, &stubHistoryReader{trades: []canonical.Trade{on, cex}},
		"/v1/vwap?base=native&quote="+w2t2USDC+"&breakdown=source")
	if got := res.Breakdown.Buckets[0].Sources[0].Source; got != "soroswap" {
		t.Errorf("top source = %s, want soroswap (weight 0.75)", got)
	}
}
