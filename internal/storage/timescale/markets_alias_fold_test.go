package timescale

import (
	"strings"
	"testing"
	"time"
)

// GH-1098: /v1/markets and /v1/pools folded rows by orientation
// (canonOrientSQL) but not by alias spelling, so a market traded once
// under a SAC C-address and once under its classic/native spelling (e.g.
// a Soroban venue vs SDEX) surfaced as two directory rows with split 24h
// volume instead of one. The fix folds base_asset/quote_asset through the
// process AliasRegistry (canonical.AllAliasForms, via buildAliasMapValues)
// BEFORE canonOrientSQL collapses orientation, in all three GROUP BY
// sites: buildPoolsQuery, buildSourceMarketsQuery and
// buildDistinctPairsQuery.
//
// These are shape tests over the composed SQL text, matching this file's
// existing convention (markets_pools_canon_test.go,
// markets_asset_alias_test.go) rather than an executing query — the
// alias-fold join is a static, always-present part of the template, so
// its presence/absence is fully decidable from the string.
func TestBuildPoolsQuery_FoldsAliasSpellingBeforeOrientation(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	for name, order := range map[string]MarketsOrder{
		"volume desc": MarketsOrderVolume24hDesc,
		"pair":        MarketsOrderPair,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _ := buildPoolsQuery(since, PoolsFilter{}, "", 100, order)

			if strings.Contains(q, "GROUP BY p.source, p.base_asset, p.quote_asset") {
				t.Errorf("pools CTE still groups by bare base_asset/quote_asset — "+
					"a SAC-aliased and classic-spelled row of the same market split volume:\n%s", q)
			}
			for _, want := range []string{
				"WITH alias_map(form, canon) AS (",
				"LEFT JOIN alias_map bm ON bm.form = p.base_asset",
				"LEFT JOIN alias_map qm ON qm.form = p.quote_asset",
				"GROUP BY p.source, COALESCE(bm.canon, p.base_asset), COALESCE(qm.canon, p.quote_asset)",
			} {
				if !strings.Contains(q, want) {
					t.Errorf("%s ordering missing alias-fold %q:\n%s", name, want, q)
				}
			}
		})
	}
}

func TestBuildSourceMarketsQuery_FoldsAliasSpellingBeforeOrientation(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	q, _ := buildSourceMarketsQuery(since, "sdex", "", 100, MarketsOrderVolume24hDesc)
	if strings.Contains(q, "GROUP BY p.source, p.base_asset, p.quote_asset") {
		t.Errorf("per-source pools CTE still groups by bare base_asset/quote_asset:\n%s", q)
	}
	if !strings.Contains(q, "GROUP BY p.source, COALESCE(bm.canon, p.base_asset), COALESCE(qm.canon, p.quote_asset)") {
		t.Errorf("per-source pools CTE missing the alias-folded GROUP BY:\n%s", q)
	}
}

func TestBuildDistinctPairsQuery_FoldsAliasSpellingBeforeOrientation(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	for name, order := range map[string]MarketsOrder{
		"volume desc": MarketsOrderVolume24hDesc,
		"pair":        MarketsOrderPair,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _ := buildDistinctPairsQuery(since, "", "", "", 100, order)

			if strings.Contains(q, "GROUP BY p.base_asset, p.quote_asset") {
				t.Errorf("%s ordering's d/h CTEs still group by bare base_asset/quote_asset:\n%s", name, q)
			}
			for _, want := range []string{
				"WITH alias_map(form, canon) AS (",
				"LEFT JOIN alias_map dbm ON dbm.form = p.base_asset",
				"LEFT JOIN alias_map dqm ON dqm.form = p.quote_asset",
				"LEFT JOIN alias_map hbm ON hbm.form = p.base_asset",
				"LEFT JOIN alias_map hqm ON hqm.form = p.quote_asset",
				"GROUP BY COALESCE(dbm.canon, p.base_asset), COALESCE(dqm.canon, p.quote_asset)",
				"GROUP BY COALESCE(hbm.canon, p.base_asset), COALESCE(hqm.canon, p.quote_asset)",
			} {
				if !strings.Contains(q, want) {
					t.Errorf("%s ordering missing alias-fold %q:\n%s", name, want, q)
				}
			}
		})
	}
}
