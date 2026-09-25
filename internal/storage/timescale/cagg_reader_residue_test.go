package timescale

import (
	"os"
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
	for name, q := range map[string]string{"xlmUSDCTEs": xlmUSDCTEs, "getNativeAssetSQL": getNativeAssetSQL} {
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
	_, _, flipped := canonOrientSQL()
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
