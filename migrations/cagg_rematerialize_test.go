package migrations

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// refreshCall matches one header recipe line:
// `--   CALL refresh_continuous_aggregate('<view>', <start>, <end>);`.
var refreshCall = regexp.MustCompile(`^--\s+CALL refresh_continuous_aggregate\('([a-z0-9_]+)',\s*(.+?),\s*(now\(\)(?: - INTERVAL '[^']+')?)\);$`)

// priceCAGGFamily is every view the 0115 / 0147 downs drop and recreate
// WITH NO DATA; twap_* are hierarchical over prices_1m.
var priceCAGGFamily = []string{
	"prices_1m", "prices_15m", "prices_1h", "prices_4h",
	"prices_1d", "prices_1w", "prices_1mo", "twap_1h", "twap_1d",
}

type refreshStep struct{ view, start, end string }

func headerRefreshSteps(t *testing.T, name string) []refreshStep {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var steps []refreshStep
	for _, line := range strings.Split(string(b), "\n") {
		if m := refreshCall.FindStringSubmatch(line); m != nil {
			steps = append(steps, refreshStep{m[1], m[2], m[3]})
		}
	}
	return steps
}

// coversAllTime reports whether a view's refresh windows chain from NULL
// (the beginning) to now() without a gap.
func coversAllTime(steps []refreshStep, view string) bool {
	at := "NULL"
	for range steps {
		next := ""
		for _, s := range steps {
			if s.view == view && s.start == at {
				next = s.end
				break
			}
		}
		if next == "" {
			return false
		}
		if next == "now()" {
			return true
		}
		at = next
	}
	return false
}

// A down that recreates the price CAGGs WITH NO DATA leaves them empty,
// and a migration cannot refresh them (refresh_continuous_aggregate
// refuses a transaction block). The header's recipe is therefore the
// only re-materialization there is: it must cover every recreated view
// over all time, and refresh the hierarchical twap_* views only after
// prices_1m is whole, or they materialize from an empty parent.
func TestCAGGRecreatingDownsCarryACompleteOrderedRefresh(t *testing.T) {
	for _, name := range []string{
		"0115_ohlc_extremes_notional_floor.down.sql",
		"0147_ohlc_deterministic_tiebreak.down.sql",
	} {
		steps := headerRefreshSteps(t, name)
		for _, view := range priceCAGGFamily {
			if !coversAllTime(steps, view) {
				t.Errorf("%s: header refresh_continuous_aggregate sequence does not re-materialize %s from NULL to now()", name, view)
			}
		}
		lastParent := -1
		for i, s := range steps {
			if s.view == "prices_1m" {
				lastParent = i
			}
		}
		for i, s := range steps {
			if strings.HasPrefix(s.view, "twap_") && i < lastParent {
				t.Errorf("%s: %s is refreshed (step %d) before prices_1m is whole (step %d); it would materialize from a partial parent", name, s.view, i+1, lastParent+1)
			}
		}
	}
}
