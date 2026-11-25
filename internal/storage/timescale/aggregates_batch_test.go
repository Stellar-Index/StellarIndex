package timescale

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var updatePriceSQLGolden = flag.Bool("update-price-sql", false, "rewrite testdata/price_reader_sql.golden")

// priceReaderSQL is every per-key price statement as rendered at a fixed instant, with the set form that wraps it;
// the single and set paths read these same renders.
func priceReaderSQL() []struct{ name, sql string } {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	singles := []struct{ name, sql string }{
		{"recentClosedVWAP1mExists", recentClosedVWAP1mExistsSQL(at)},
		{"latestClosedVWAP1m", latestClosedVWAP1mSQL(at)},
		{"recentClosedVWAP1mCombined", recentClosedVWAP1mCombinedSQL(at)},
		{"closedVWAP1mCombinedBefore", closedVWAP1mCombinedBeforeSQL(at)},
		{"closedVWAP1mAtOrBefore", closedVWAP1mAtOrBeforeQuery},
		{"pairMarketSubstance", pairMarketSubstanceSQL(at, 24*time.Hour)},
	}
	out := append([]struct{ name, sql string }{}, singles...)
	for _, s := range singles {
		set := vwapRowsSetSQL(s.sql)
		switch s.name {
		case "recentClosedVWAP1mExists":
			set = existsSetSQL(s.sql)
		case "pairMarketSubstance":
			set = substanceSetSQL(s.sql)
		}
		out = append(out, struct{ name, sql string }{s.name + " (set)", set})
	}
	return out
}

// TestPriceReaderSQL_Golden pins each rendered statement, so a changed predicate or a dropped literal lower bound
// (plan-time chunk pruning) shows in review for the single and set forms alike. Regenerate with -update-price-sql.
func TestPriceReaderSQL_Golden(t *testing.T) {
	var b strings.Builder
	for _, s := range priceReaderSQL() {
		b.WriteString("-- " + s.name + "\n" + s.sql + "\n")
	}
	path := filepath.Join("testdata", "price_reader_sql.golden")
	if *updatePriceSQLGolden {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) // #nosec G304 -- fixed testdata path
	if err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != string(want) {
		t.Errorf("rendered price reader SQL differs from %s (rerun with -update-price-sql and review the diff):\n%s", path, got)
	}
}

// twoDigitLegParam matches $1 or $2 followed by a digit, which the textual $1/$2 rebinding of a set form would corrupt.
var twoDigitLegParam = regexp.MustCompile(`\$[12][0-9]`)

func TestPriceReaderSQL_NoTwoDigitLegParams(t *testing.T) {
	if !twoDigitLegParam.MatchString("WHERE base_asset = $1 AND bucket < $10") {
		t.Fatal("the check does not reject $10")
	}
	for _, s := range priceReaderSQL()[:6] {
		if twoDigitLegParam.MatchString(s.sql) {
			t.Errorf("%s binds a parameter $1N/$2N; its set form would rewrite it as a key column", s.name)
		}
	}
}
