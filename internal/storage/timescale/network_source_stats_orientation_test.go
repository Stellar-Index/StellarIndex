package timescale

import (
	"strings"
	"testing"
)

// TestNetworkStatsQueryFoldsOrientation pins RLT-274: markets_count_24h
// must count DISTINCT canonical (base, quote) pairs, not raw stored
// ones — otherwise a market recorded in both orientations (XLM/USDC and
// USDC/XLM) is counted twice. Before the fix, networkStatsQuery emitted
// a bare `SELECT DISTINCT base_asset, quote_asset`.
func TestNetworkStatsQueryFoldsOrientation(t *testing.T) {
	q := networkStatsQuery()
	if strings.Contains(q, "SELECT DISTINCT base_asset, quote_asset") {
		t.Errorf("markets_count_24h must not DISTINCT on raw base_asset/quote_asset — it double-counts a flipped-orientation market:\n%s", q)
	}
	canonBase, canonQuote, _ := canonOrientSQL("base_asset", "quote_asset")
	if !strings.Contains(q, "SELECT DISTINCT "+canonBase+" AS base_asset, "+canonQuote+" AS quote_asset") {
		t.Errorf("markets_count_24h must DISTINCT on canonOrientSQL's folded (base, quote):\n%s", q)
	}
}

// TestSourceStatsQueryFoldsOrientation pins RLT-274 in GetSourceStats:
// the per_pair CTE must GROUP BY the canonical (base, quote), not the
// raw stored columns, or a source's markets_24h is inflated by one per
// flipped-orientation pair it printed.
func TestSourceStatsQueryFoldsOrientation(t *testing.T) {
	q := sourceStatsQuery()
	if strings.Contains(q, "GROUP BY source, base_asset, quote_asset") {
		t.Errorf("per_pair must not GROUP BY raw base_asset/quote_asset — it double-counts a flipped-orientation market:\n%s", q)
	}
	canonBase, canonQuote, _ := canonOrientSQL("base_asset", "quote_asset")
	if !strings.Contains(q, "GROUP BY source, "+canonBase+", "+canonQuote) {
		t.Errorf("per_pair must GROUP BY canonOrientSQL's folded (base, quote):\n%s", q)
	}
}
