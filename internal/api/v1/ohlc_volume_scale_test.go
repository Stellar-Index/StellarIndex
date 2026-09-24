package v1_test

import (
	"fmt"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ohlcScaledBar decodes /v1/ohlc's single-bar payload with the two
// volume-scale fields as POINTERS on purpose: this file has to compile
// against a build that does not serve them, so that the assertions below
// are provably about the served scale and not about a struct tag.
type ohlcScaledBar struct {
	BaseVolume          string `json:"base_volume"`
	QuoteVolume         string `json:"quote_volume"`
	BaseVolumeDecimals  *int   `json:"base_volume_decimals"`
	QuoteVolumeDecimals *int   `json:"quote_volume_decimals"`
	TradeCount          int    `json:"trade_count"`
}

// mkScaledOHLCTrade builds one native/fiat:USD print attributed to
// `source`, with the raw smallest-unit amounts given verbatim — the
// point of these fixtures is that the caller chooses the scale, because
// the source is what decides it (sdex 1e7, coinbase 1e8 — CS-040).
func mkScaledOHLCTrade(source string, base, quote *big.Int, opIndex uint32, ts time.Time) canonical.Trade {
	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)
	return canonical.Trade{
		Source:      source,
		Ledger:      1,
		TxHash:      fmt.Sprintf("%064d", opIndex),
		OpIndex:     opIndex,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}
}

// scaledUnits is `n` whole asset units at a `decimals`-place smallest
// unit — n × 10^decimals.
func scaledUnits(n int64, decimals int) *big.Int {
	return new(big.Int).Mul(big.NewInt(n),
		new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
}

// assetUnits renders a served volume integer in the asset's own units,
// the way a consumer must: value / 10^stated_decimals. A response that
// states no scale cannot be rendered at all, which is the F096 defect
// and is reported as such.
func assetUnits(t *testing.T, raw string, decimals *int) string {
	t.Helper()
	if decimals == nil {
		t.Fatalf("response states no volume scale, so %q cannot be rendered in asset units "+
			"— a consumer is left guessing a divisor (finding F096)", raw)
	}
	if *decimals < 0 {
		t.Fatalf("volume scale = %d, want a non-negative decimal exponent", *decimals)
	}
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		t.Fatalf("volume %q is not an integer", raw)
	}
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(*decimals)), nil)
	return new(big.Rat).SetFrac(n, pow).FloatString(*decimals)
}

// showDecimals renders a stated scale for a failure message: the value,
// or "absent" when the response carried none at all.
func showDecimals(d *int) string {
	if d == nil {
		return "absent"
	}
	return fmt.Sprintf("%d", *d)
}

func getOHLCBar(t *testing.T, reader *stubHistoryReader, query string) ohlcScaledBar {
	t.Helper()
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD"+query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data ohlcScaledBar `json:"data"`
	}
	mustDecode(t, resp, &env)
	return env.Data
}

// TestOHLC_VolumeScale_CEXWindowIsEightDecimals is the F096 headline: a
// Coinbase-fed pair's volume integers are at 1e8, and until the bar said
// so the /markets/[pair] page divided them by a hardcoded 1e7 and printed
// a quote volume ten times the market's.
func TestOHLC_VolumeScale_CEXWindowIsEightDecimals(t *testing.T) {
	base := time.Unix(1_772_000_000, 0).UTC()
	// 1000 XLM at $0.25, three times: 750 USD of quote volume.
	reader := &stubHistoryReader{
		trades: []canonical.Trade{
			mkScaledOHLCTrade("coinbase", scaledUnits(1000, 8), scaledUnits(250, 8), 1, base),
			mkScaledOHLCTrade("coinbase", scaledUnits(1000, 8), scaledUnits(250, 8), 2, base.Add(time.Second)),
			mkScaledOHLCTrade("coinbase", scaledUnits(1000, 8), scaledUnits(250, 8), 3, base.Add(2*time.Second)),
		},
	}
	got := getOHLCBar(t, reader, "")

	if got.QuoteVolumeDecimals == nil || *got.QuoteVolumeDecimals != 8 {
		t.Errorf("quote_volume_decimals = %s, want 8 (coinbase stamps amounts at 1e8)",
			showDecimals(got.QuoteVolumeDecimals))
	}
	if u := assetUnits(t, got.QuoteVolume, got.QuoteVolumeDecimals); u != "750.00000000" {
		t.Errorf("quote volume in asset units = %s, want 750.00000000", u)
	}
	if u := assetUnits(t, got.BaseVolume, got.BaseVolumeDecimals); u != "3000.00000000" {
		t.Errorf("base volume in asset units = %s, want 3000.00000000", u)
	}
}

// TestOHLC_VolumeScale_SurvivesOutlierFilterDroppingTheOnlyCEXPrint is
// the narrow window an earlier F096 fix re-opened: the scale must be
// resolved over the PRE-outlier-filter population, because the lift to
// the common scale ran over that population and the filter does not
// un-lift the rows it keeps.
//
// Four honest on-chain prints at 1e7 plus one aberrant CEX print at 1e8:
// the CEX print is dropped by the default sigma, but the four survivors
// were already multiplied by ten to meet it. A bar that resolves its
// scale over the post-filter slice states 7 for integers that are at 8
// and renders 14000 USD where the market did 1400.
func TestOHLC_VolumeScale_SurvivesOutlierFilterDroppingTheOnlyCEXPrint(t *testing.T) {
	base := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubHistoryReader{
		trades: []canonical.Trade{
			// Four on-chain prints: 1000 XLM at $0.35 each, 1e7 scale.
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 1, base),
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 2, base.Add(time.Second)),
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 3, base.Add(2*time.Second)),
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 4, base.Add(3*time.Second)),
			// One wild CEX print at $50 — the only 1e8 venue in the
			// window, and the one the default sigma removes.
			mkScaledOHLCTrade("coinbase", scaledUnits(1, 8), scaledUnits(50, 8), 5, base.Add(4*time.Second)),
		},
	}
	got := getOHLCBar(t, reader, "")

	if got.TradeCount != 4 {
		t.Fatalf("trade_count = %d, want 4 — the fixture's outlier must be filtered "+
			"for this test to exercise the population it is about", got.TradeCount)
	}
	if got.QuoteVolumeDecimals == nil || *got.QuoteVolumeDecimals != 8 {
		t.Errorf("quote_volume_decimals = %s, want 8 — the surviving rows were lifted "+
			"to the dropped venue's scale and stay there", showDecimals(got.QuoteVolumeDecimals))
	}
	if u := assetUnits(t, got.QuoteVolume, got.QuoteVolumeDecimals); u != "1400.00000000" {
		t.Errorf("quote volume in asset units = %s, want 1400.00000000", u)
	}
	if u := assetUnits(t, got.BaseVolume, got.BaseVolumeDecimals); u != "4000.00000000" {
		t.Errorf("base volume in asset units = %s, want 4000.00000000", u)
	}
}

// TestOHLC_VolumeScale_OnChainWindowIsSevenDecimals pins the other
// direction: an all-sdex window really is 1e7 and must say 7, so the fix
// cannot degenerate into "always answer 8".
func TestOHLC_VolumeScale_OnChainWindowIsSevenDecimals(t *testing.T) {
	base := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubHistoryReader{
		trades: []canonical.Trade{
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 1, base),
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 2, base.Add(time.Second)),
			mkScaledOHLCTrade("sdex", scaledUnits(1000, 7), scaledUnits(350, 7), 3, base.Add(2*time.Second)),
		},
	}
	got := getOHLCBar(t, reader, "")

	if got.BaseVolumeDecimals == nil || *got.BaseVolumeDecimals != 7 {
		t.Errorf("base_volume_decimals = %s, want 7 (sdex stamps amounts at 1e7)",
			showDecimals(got.BaseVolumeDecimals))
	}
	if u := assetUnits(t, got.QuoteVolume, got.QuoteVolumeDecimals); u != "1050.0000000" {
		t.Errorf("quote volume in asset units = %s, want 1050.0000000", u)
	}
}

// TestOHLC_VolumeScale_UnregisteredSourceIsUnknown is GH-1285: a source
// absent from external.Registry must not be answered with the registry's
// CEX-flavoured 8-decimal fallback. An unregistered on-chain DEX at 1e7
// would otherwise be stated as 8, overstating its volume tenfold the
// opposite way F096 already fixed.
func TestOHLC_VolumeScale_UnregisteredSourceIsUnknown(t *testing.T) {
	base := time.Unix(1_772_000_000, 0).UTC()
	reader := &stubHistoryReader{
		trades: []canonical.Trade{
			mkScaledOHLCTrade("brand_new_dex", scaledUnits(1000, 7), scaledUnits(350, 7), 1, base),
			mkScaledOHLCTrade("brand_new_dex", scaledUnits(1000, 7), scaledUnits(350, 7), 2, base.Add(time.Second)),
		},
	}
	got := getOHLCBar(t, reader, "")

	if got.BaseVolumeDecimals != nil {
		t.Errorf("base_volume_decimals = %s, want absent (null) — \"brand_new_dex\" has "+
			"no external.Registry entry, so its scale is unknown, not the registry's "+
			"8-decimal fallback", showDecimals(got.BaseVolumeDecimals))
	}
	if got.QuoteVolumeDecimals != nil {
		t.Errorf("quote_volume_decimals = %s, want absent (null)", showDecimals(got.QuoteVolumeDecimals))
	}
}
