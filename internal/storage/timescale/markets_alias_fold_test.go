package timescale

import (
	"strings"
	"testing"
	"time"
)

// GH-1098: /v1/markets and /v1/pools grouped their canon CTE by
// orientation but never folded alias spellings first, so a market traded
// on one venue under `crypto:XLM` and on another under `native` (or a
// classic asset under its SAC wrapper) surfaced as two directory rows
// with split 24h volume instead of one. The fold happens in the shared
// `pools`/`raw` projection, BEFORE any GROUP BY — never a bare
// p.base_asset/p.quote_asset (or d./h. equivalents) grouping key, which
// would collapse only stored orientations, not alias spellings.
func TestBuildPoolsQuery_FoldsAliasMapBeforeGroupBy(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	for name, order := range map[string]MarketsOrder{
		"volume desc": MarketsOrderVolume24hDesc,
		"pair":        MarketsOrderPair,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _ := buildPoolsQuery(since, PoolsFilter{}, "", 100, order)

			if !strings.Contains(q, "LEFT JOIN alias_map bm ON bm.form = p.base_asset") ||
				!strings.Contains(q, "LEFT JOIN alias_map qm ON qm.form = p.quote_asset") {
				t.Errorf("pools CTE does not fold through alias_map:\n%s", q)
			}
			if strings.Contains(q, "GROUP BY p.source, p.base_asset, p.quote_asset") {
				t.Errorf("pools CTE still groups on the bare (unfolded) columns — "+
					"a market traded under two alias spellings on the same source "+
					"splits into two rows:\n%s", q)
			}
			if !strings.Contains(q, "GROUP BY p.source, "+poolsGroupByFoldSQL) {
				t.Errorf("pools CTE does not group on the alias-folded columns:\n%s", q)
			}
		})
	}
}

func TestBuildSourceMarketsQuery_FoldsAliasMapBeforeGroupBy(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	for name, order := range map[string]MarketsOrder{
		"volume desc": MarketsOrderVolume24hDesc,
		"pair":        MarketsOrderPair,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _ := buildSourceMarketsQuery(since, "soroswap", "", 100, order)

			if !strings.Contains(q, "LEFT JOIN alias_map bm ON bm.form = p.base_asset") ||
				!strings.Contains(q, "LEFT JOIN alias_map qm ON qm.form = p.quote_asset") {
				t.Errorf("per-source pools CTE does not fold through alias_map:\n%s", q)
			}
			if strings.Contains(q, "GROUP BY p.source, p.base_asset, p.quote_asset") {
				t.Errorf("per-source pools CTE still groups on the bare (unfolded) columns:\n%s", q)
			}
		})
	}
}

func TestBuildDistinctPairsQuery_FoldsAliasMapBeforeGroupBy(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	for name, order := range map[string]MarketsOrder{
		"volume desc": MarketsOrderVolume24hDesc,
		"pair":        MarketsOrderPair,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _ := buildDistinctPairsQuery(since, "", "", "", 100, order)

			if !strings.Contains(q, "LEFT JOIN alias_map bm ON bm.form = COALESCE(d.base_asset, h.base_asset)") ||
				!strings.Contains(q, "LEFT JOIN alias_map qm ON qm.form = COALESCE(d.quote_asset, h.quote_asset)") {
				t.Errorf("raw CTE does not fold through alias_map:\n%s", q)
			}
			if strings.Contains(q, "SELECT COALESCE(d.base_asset, h.base_asset)   AS base_asset") {
				t.Errorf("raw CTE still projects the bare (unfolded) coalesced columns:\n%s", q)
			}
		})
	}
}
