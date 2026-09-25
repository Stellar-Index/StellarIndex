package timescale

import (
	"strings"
	"testing"
	"time"
)

// GH-1098: /v1/markets and /v1/pools folded rows by stored orientation
// but not by alias spelling, so a market traded on one venue under
// `crypto:XLM` and on another under `native` (or a classic asset's SAC
// wrapper) surfaced as two directory rows with split 24h volume instead
// of one canonical row. The fix folds each `canon` CTE's grouping columns
// through alias_map (COALESCE(alias_map.canon, raw_column)) BEFORE
// canonOrientSQL computes the orientation, mirroring the fold
// buildAliasMapValues already applies for the asset-volume-character
// rollup.
//
// These are shape tests: no GROUP BY in markets.go may group directly on
// the bare base_asset/quote_asset columns without first joining
// alias_map — that is exactly the defect this finding reports.
func TestMarketsQueries_CanonStepFoldsAliasesBeforeOrienting(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		q    string
		args []any
	}{
		{
			name: "buildPoolsQuery volume desc",
			q:    must(buildPoolsQuery(since, PoolsFilter{}, "", 100, MarketsOrderVolume24hDesc)),
		},
		{
			name: "buildPoolsQuery pair",
			q:    must(buildPoolsQuery(since, PoolsFilter{}, "", 100, MarketsOrderPair)),
		},
		{
			name: "buildSourceMarketsQuery volume desc",
			q:    must(buildSourceMarketsQuery(since, "sdex", "", 100, MarketsOrderVolume24hDesc)),
		},
		{
			name: "buildDistinctPairsQuery volume desc",
			q:    must(buildDistinctPairsQuery(since, "", "", "", 100, MarketsOrderVolume24hDesc)),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(tc.q, "{{ALIAS_VALUES}}") {
				t.Fatalf("query still carries the unsubstituted alias-fold token:\n%s", tc.q)
			}
			if !strings.Contains(tc.q, "alias_map") {
				t.Fatalf("query never joins alias_map — a market traded under two alias "+
					"spellings (e.g. native / crypto:XLM) folds into two directory rows "+
					"instead of one:\n%s", tc.q)
			}
			// The `canon` CTE — the one that GROUP BYs the canonical
			// (base, quote) pair — must fold through the alias join
			// BEFORE grouping, not merely reference alias_map elsewhere.
			canonIdx := strings.Index(tc.q, "canon AS (")
			if canonIdx < 0 {
				t.Fatalf("query has no `canon` CTE:\n%s", tc.q)
			}
			canonBody := tc.q[canonIdx:]
			groupIdx := strings.Index(canonBody, "GROUP BY")
			if groupIdx < 0 {
				t.Fatalf("`canon` CTE has no GROUP BY:\n%s", canonBody)
			}
			preGroup := canonBody[:groupIdx]
			if !strings.Contains(preGroup, "LEFT JOIN alias_map") {
				t.Errorf("`canon` CTE groups without first joining alias_map — "+
					"the fold must precede the GROUP BY:\n%s", preGroup)
			}
			if !strings.Contains(preGroup, "COALESCE(") {
				t.Errorf("`canon` CTE's canonical base/quote expressions don't "+
					"COALESCE through the alias fold:\n%s", preGroup)
			}
		})
	}
}

func must(q string, args []any) string { //nolint:unparam // args intentionally discarded; the shape assertions are on the query text
	_ = args
	return q
}
