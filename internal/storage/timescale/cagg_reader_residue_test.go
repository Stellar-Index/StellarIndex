package timescale

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestChangeWindowsMatchDocumentedTolerance pins GH-702 item 1: every
// change lookback reads within AssetRow's documented tolerance of its
// target (1h ±5 min, 24h ±30 min, 7d ±2 h). The lower bounds were
// 90 min / 26 h / 7 d 12 h, so a thin token's 88-minute move served as
// change_1h_pct.
func TestChangeWindowsMatchDocumentedTolerance(t *testing.T) {
	want := map[string]string{
		"priceWindow1h":  `bucket BETWEEN now() - INTERVAL '65 minutes' AND now() - INTERVAL '55 minutes'`,
		"priceWindow24h": `bucket BETWEEN now() - INTERVAL '24 hours 30 minutes' AND now() - INTERVAL '23 hours 30 minutes'`,
		"priceWindow7d":  `bucket BETWEEN now() - INTERVAL '7 days 2 hours' AND now() - INTERVAL '6 days 22 hours'`,
	}
	got := map[string]string{
		"priceWindow1h":  priceWindow1h,
		"priceWindow24h": priceWindow24h,
		"priceWindow7d":  priceWindow7d,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %q, want %q", name, got[name], w)
		}
	}
	// The XLM/USD lookbacks (rollup, detail and native row) carry their
	// own copies of the same windows.
	for name, q := range map[string]string{"xlmUSDNativeCTEs": xlmUSDNativeCTEs("$1::text"), "getNativeAssetSQL": getNativeAssetSQL} {
		for _, stale := range []string{"'90 minutes'", "'26 hours'", "'7 days 12 hours'"} {
			if strings.Contains(q, stale) {
				t.Errorf("%s still reads a change lookback outside the documented tolerance (%s)", name, stale)
			}
		}
		for _, lower := range []string{"'65 minutes'", "'24 hours 30 minutes'", "'7 days 2 hours'"} {
			if !strings.Contains(q, lower) {
				t.Errorf("%s lacks the documented lower bound %s", name, lower)
			}
		}
	}
}

// untiedNewestRE matches a newest-row pick over folded orientations with
// no tie-break after last_trade_at.
var untiedNewestRE = regexp.MustCompile(`ORDER BY last_trade_at DESC NULLS LAST\s*\)`)

// TestCanonLastPriceIsTieBroken pins GH-702 item 2 for the market fold:
// both stored orientations routinely share last_trade_at, so the pick
// must not fall to scan order. Every fold goes through canonLastPriceSQL.
func TestCanonLastPriceIsTieBroken(t *testing.T) {
	_, _, flipped := canonOrientSQL(1)
	if !strings.Contains(canonLastPriceSQL(flipped), "ORDER BY last_trade_at DESC NULLS LAST, "+flipped+")") {
		t.Errorf("canonLastPriceSQL must break a last_trade_at tie on the orientation")
	}
	src, err := os.ReadFile("markets.go")
	if err != nil {
		t.Fatal(err)
	}
	if loc := untiedNewestRE.FindIndex(src); loc != nil {
		t.Errorf("markets.go picks a newest last_price with no tie-break at byte %d; use canonLastPriceSQL", loc[0])
	}
	if n := strings.Count(string(src), "canonLastPriceSQL(flipped)"); n != 3 {
		t.Errorf("markets.go folds last_price through canonLastPriceSQL %d times, want 3", n)
	}
}

// untiedBucketPickRE matches a newest-bucket pick with nothing after
// `bucket DESC` but the LIMIT, on the same line or the next.
var untiedBucketPickRE = regexp.MustCompile(`ORDER BY (?:p\.)?bucket DESC(?:[ \t]*\n\s*|[ \t]+)LIMIT 1`)

// multiFormQuoteRE matches a quote set holding several forms of one
// asset: USD's (literal or usdProxyQuotes) or XLM's (xlmQuotes).
var multiFormQuoteRE = regexp.MustCompile(`'fiat:USD'|\busdProxyQuotes\b|\bxlmQuotes\b`)

// TestUSDQuotePicksAreTieBroken pins GH-702 item 2 for the price readers:
// USDC and fiat:USD (or two XLM or peg forms) can print in the same
// minute, so every newest-bucket pick across forms needs a stable
// second key. last(vwap, bucket) has none.
func TestUSDQuotePicksAreTieBroken(t *testing.T) {
	if !strings.Contains(getNativeAssetSQL, xlmUSDNativeCTEs("$1::text")) {
		t.Error("getNativeAssetSQL must reuse xlmUSDNativeCTEs rather than carry its own XLM/USD picks")
	}
	if n := strings.Count(xlmUSDNativeCTEs("$1::text"), "ORDER BY xm.form_rank, xm.bucket DESC"); n != 4 {
		t.Errorf("xlmUSDNativeCTEs orders %d XLM/USD picks by form then minute, want 4", n)
	}
	for name, q := range map[string]string{
		"xlmUSDNativeCTEs":        xlmUSDNativeCTEs("$1::text"),
		"getNativeAssetSQL":       getNativeAssetSQL,
		"xlmLegQuery":             xlmLegQuery("AND bucket >= $5"),
		"directLegQuery":          directLegQuery(false),
		"directLegQuery(bounded)": directLegQuery(true),
	} {
		if loc := untiedBucketPickRE.FindStringIndex(q); loc != nil {
			t.Errorf("%s picks the newest bucket with no tie-break at byte %d", name, loc[0])
		}
	}
	// Every hand-written pick across quote forms in the package, not just the
	// catalogue's: the volume, MEV and transitive readers carry their own.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, loc := range untiedBucketPickRE.FindAllIndex(src, -1) {
			// The pick's own SELECT: a WHERE holds no nested SELECT here.
			pick := string(src[max(0, loc[0]-700):loc[0]])
			if i := strings.LastIndex(pick, "SELECT"); i >= 0 {
				pick = pick[i:]
			}
			if multiFormQuoteRE.MatchString(pick) {
				t.Errorf("%s:%d picks the newest bucket across quote forms with no tie-break",
					f, strings.Count(string(src[:loc[0]]), "\n")+1)
			}
		}
	}
	if !strings.Contains(xlmLegQuery(""), "ORDER BY bucket DESC, inverted") {
		t.Error("xlmLegQuery must break a cross-arm bucket tie on the stored direction")
	}
	for name, q := range map[string]string{
		"history24h":      getAssetPriceHistory24hSQL,
		"history7d":       getAssetPriceHistory7dSQL,
		"history24hBatch": getAssetsPriceHistory24hBatchSQL,
		"history7dBatch":  getAssetsPriceHistory7dBatchSQL,
	} {
		if strings.Contains(q, "last(vwap, bucket)") {
			t.Errorf("%s still picks with last(vwap, bucket), which has no tie-break", name)
		}
		// The direct arm ranks the USD quote forms; the XLM/USD leg is the
		// anchor, which weights a minute's quote forms instead of ranking them.
		if n := strings.Count(q, usdQuotePref); n != 1 {
			t.Errorf("%s ranks USD quote forms %d times, want 1 (direct)", name, n)
		}
		if !strings.Contains(q, "ORDER BY 1, xm.form_rank, xm.bucket DESC") {
			t.Errorf("%s does not take its XLM/USD leg from the anchor", name)
		}
		if !strings.Contains(q, "bucket DESC, xlm_prio") {
			t.Errorf("%s does not break an XLM-form tie in its XLM leg", name)
		}
	}
}

// TestAssetAliasRows pins the batch alias expansion: every form of every
// requested id, owned by that id, in assetAliasArray's priority order.
func TestAssetAliasRows(t *testing.T) {
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	forms, owners, prios := assetAliasRows([]string{"native", usdc})
	xlm := assetAliasArray("native")
	if len(xlm) < 2 {
		t.Fatalf("assetAliasArray(native) = %v, want XLM's alias forms", xlm)
	}
	want := len(xlm) + len(assetAliasArray(usdc))
	if len(forms) != want || len(owners) != want || len(prios) != want {
		t.Fatalf("lengths = %d/%d/%d, want %d", len(forms), len(owners), len(prios), want)
	}
	for i, f := range xlm {
		if forms[i] != f || owners[i] != "native" || prios[i] != int64(i+1) {
			t.Errorf("row %d = (%s, %s, %d), want (%s, native, %d)", i, forms[i], owners[i], prios[i], f, i+1)
		}
	}
	if owners[len(xlm)] != usdc || prios[len(xlm)] != 1 {
		t.Errorf("first USDC row = (%s, %d), want (%s, 1)", owners[len(xlm)], prios[len(xlm)], usdc)
	}
}
