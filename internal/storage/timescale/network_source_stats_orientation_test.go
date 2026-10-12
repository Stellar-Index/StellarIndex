package timescale

import (
	"strings"
	"testing"
)

// TestNetworkStatsQueryFoldsOrientation pins that markets_count_24h counts DISTINCT canonical (base,
// quote) pairs, else a market stored in both orientations (XLM/USDC and USDC/XLM) is counted twice.
func TestNetworkStatsQueryFoldsOrientation(t *testing.T) {
	q := networkStatsQuery()
	if strings.Contains(q, "SELECT DISTINCT base_asset, quote_asset") {
		t.Errorf("markets_count_24h must not DISTINCT on raw base_asset/quote_asset — it double-counts a flipped-orientation market:\n%s", q)
	}
	canonBase, canonQuote, _ := canonOrientSQL(2)
	if !strings.Contains(q, "SELECT DISTINCT "+canonBase+" AS base_asset, "+canonQuote+" AS quote_asset") {
		t.Errorf("markets_count_24h must DISTINCT on canonOrientSQL's folded (base, quote):\n%s", q)
	}
}

// TestSourceStatsQueryFoldsOrientation pins the same fold in GetSourceStats: the per_pair CTE must GROUP BY
// the canonical (base, quote), not the raw columns, or markets_24h is inflated by each flipped-orientation
// pair.
func TestSourceStatsQueryFoldsOrientation(t *testing.T) {
	q := sourceStatsQuery()
	if strings.Contains(q, "GROUP BY source, base_asset, quote_asset") {
		t.Errorf("per_pair must not GROUP BY raw base_asset/quote_asset — it double-counts a flipped-orientation market:\n%s", q)
	}
	canonBase, canonQuote, _ := canonOrientSQL(1)
	if !strings.Contains(q, "GROUP BY source, "+canonBase+", "+canonQuote) {
		t.Errorf("per_pair must GROUP BY canonOrientSQL's folded (base, quote):\n%s", q)
	}
}
