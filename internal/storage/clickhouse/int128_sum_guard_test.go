package clickhouse

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var int128SumRe = regexp.MustCompile(`sum(?:If)?\(\s*toInt128\(([^)]*)\)`)

// int128SumAllowed lists every sum over toInt128 this package may run. Each source is a classic Int64 column, so the
// sum stays far below 2^127; an i128 Soroban amount summed this way wraps silently and must use toInt256.
var int128SumAllowed = map[string]map[string]string{
	"asset_stats_daily.go":   {"balance": "trustline balances are Int64 stroops", "rank": "rank × Int64 balance, bounded by holders × 2^63"},
	"asset_supply_reader.go": {"balance": "trustline balances are Int64 stroops"},
	"account_creators_rollup.go": {
		"amount":       "create_account starting balances are Int64 stroops",
		"live_balance": "native balances are Int64 stroops",
	},
	"account_state_reader.go":  {"balance": "native balances are Int64 stroops"},
	"account_graph.go":         {"funded_stroops": "per-creator totals of Int64 starting balances"},
	"asset_holders_rollup.go":  {"balance": "trustline balances are Int64 stroops"},
	"account_cohort_rollup.go": {"e.balance": "account and trustline balances are Int64 stroops"},
}

// A new sum over toInt128 fails here until its source is shown to be Int64-bounded; i128 sums use toInt256.
func TestInt128SumsAreOverInt64Sources(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range int128SumRe.FindAllStringSubmatch(string(src), -1) {
			seen++
			expr := strings.TrimSpace(m[1])
			if _, ok := int128SumAllowed[f][expr]; !ok {
				t.Errorf("%s: %s sums toInt128(%s); an i128 amount wraps at 2^127 — use toInt256, or allow it here with the reason its source is Int64", f, m[0], expr)
			}
		}
	}
	if seen == 0 {
		t.Fatal("matched no toInt128 sums: the scan is not reading this package's sources")
	}
}
