package timescale

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// rawPairCountRE matches a distinct-market count keyed on the raw stored
// (base_asset, quote_asset) columns. prices_1m and trades keep each
// market in the venue's observed direction, so such a count counts a
// two-sided market twice.
var rawPairCountRE = regexp.MustCompile(
	`(?i)(count\s*\(\s*distinct\s*\(\s*base_asset\s*,\s*quote_asset\s*\)` +
		`|select\s+distinct\s+base_asset\s*,\s*quote_asset\b)`)

// TestNoRawOrientationMarketCount is the class guard for "a market
// counted once per stored orientation": no non-test file in this
// package may count distinct markets on the raw columns. Count on
// marketKeySQL or canonOrientSQL instead. Go and SQL comment lines are
// skipped so a comment may still name the old shape.
func TestNoRawOrientationMarketCount(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "--") {
				continue
			}
			if rawPairCountRE.MatchString(line) {
				t.Errorf("%s:%d counts markets on the raw stored orientation (use marketKeySQL / canonOrientSQL): %s",
					name, i+1, trimmed)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no source files — the guard did not run")
	}
}

// TestPairSourceStatsMatchesBothOrientations pins /v1/markets/sources?
// base=&quote= to both stored directions of the market, as
// pairMarketQuery reads it.
func TestPairSourceStatsMatchesBothOrientations(t *testing.T) {
	for _, want := range []string{
		"AND base_asset = ANY($1) AND quote_asset = ANY($2)\n\t\t    UNION ALL",
		"AND base_asset = ANY($2) AND quote_asset = ANY($1)",
	} {
		if !strings.Contains(pairSourceStatsQuery, want) {
			t.Errorf("pairSourceStatsQuery must match %s:\n%s", want, pairSourceStatsQuery)
		}
	}
	for name, q := range map[string]string{
		"pairSourceStatsQuery":  pairSourceStatsQuery,
		"assetSourceStatsQuery": assetSourceStatsQuery,
	} {
		if !strings.Contains(q, "COUNT(DISTINCT ("+marketKeySQL+"))") {
			t.Errorf("%s markets_24h must count marketKeySQL's unordered pair:\n%s", name, q)
		}
	}
}

// TestAssetMarketQueriesFoldOrientation pins the asset-detail market
// readers to canonOrientSQL's folded pair: the count DISTINCTs on it and
// the top-markets preview groups volume and trade count by it.
func TestAssetMarketQueriesFoldOrientation(t *testing.T) {
	canonBase, canonQuote, _ := canonOrientSQL()
	if q := assetMarketsCountQuery(); !strings.Contains(q, "SELECT DISTINCT "+canonBase+", "+canonQuote) {
		t.Errorf("GetAssetMarketsCount must DISTINCT on the canonical pair:\n%s", q)
	}
	q := assetTopMarketsQuery()
	folded := "SELECT " + canonBase + " AS base_asset, " + canonQuote + " AS quote_asset"
	if got := strings.Count(q, folded); got != 2 {
		t.Errorf("GetAssetTopMarkets must fold both per_pair_24h and per_pair_count (%d of 2):\n%s", got, q)
	}
	if strings.Contains(q, "GROUP BY base_asset, quote_asset") {
		t.Errorf("GetAssetTopMarkets must not group on the raw stored pair:\n%s", q)
	}
}
