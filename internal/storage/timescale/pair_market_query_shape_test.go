package timescale

import (
	"regexp"
	"strings"
	"testing"
)

// [pairMarketQuery] (/v1/pairs) cost is per request, so two shapes are pinned: both orientations as UNION ALL
// of single-direction branches (an OR defeats the pair index), and each aggregate bounded to its smallest
// window (MAX(ts) probe, 24h count), not one 14-day scan of `trades`.
func TestPairMarketQueryShape(t *testing.T) {
	q := pairMarketQuery

	// ── the disjunction is the defect; its absence is the guard ──
	if strings.Contains(q, "OR (base_asset") || strings.Contains(q, "OR (t.base_asset") {
		t.Errorf("pairMarketQuery folds both directions with an OR disjunction. "+
			"The planner cannot drive the (base_asset, quote_asset, ts|bucket DESC) "+
			"indexes from an OR of two (base,quote) pairs, so it degrades to a bare "+
			"time-index scan with the pair as a post-index filter. Use a UNION ALL of "+
			"two single-direction branches.\n\nquery:\n%s", q)
	}
	if n := strings.Count(q, "UNION ALL"); n < 3 {
		t.Errorf("pairMarketQuery has %d UNION ALL branches, want >= 3 — the "+
			"last_trade_at, vol_24h_usd and last_price reads each fold two stored "+
			"orientations and each must be its own indexable branch", n)
	}

	// Correctness half: both orientations must still be read. A UNION ALL that dropped a leg would be fast and
	// wrong: the SDEX decoder records XLM/USDC and USDC/XLM as separate rows, so a single-leg read halves
	// count_24h and vol_24h_usd.
	for _, leg := range []struct{ what, sql string }{
		{"forward, trades", "base_asset = $2 AND t.quote_asset = $3"},
		{"flipped, trades", "base_asset = $3 AND t.quote_asset = $2"},
		{"forward, prices_1m", "base_asset = $2 AND quote_asset = $3"},
		{"flipped, prices_1m", "base_asset = $3 AND quote_asset = $2"},
	} {
		if !strings.Contains(q, leg.sql) {
			t.Errorf("pairMarketQuery lost the %s direction (%q)", leg.what, leg.sql)
		}
	}

	// Window half: nothing may read MarketsRecencyWindow in bulk. $1 (the 14-day bound) may appear only on the
	// two LIMIT 1 last_trade_at index probes; any other use is a 14-day scan answering a 24-hour question.
	if n := strings.Count(q, "$1"); n != 2 {
		t.Errorf("pairMarketQuery references the MarketsRecencyWindow bound $1 %d "+
			"times, want exactly 2 (one per last_trade_at direction probe). Every "+
			"other aggregate needs at most 24 hours; widening one back to 14 days is "+
			"how this query came to materialise 17.2M rows for crypto:BTC/crypto:USDT."+
			"\n\nquery:\n%s", n, q)
	}
	// Each $1 branch must terminate at one row — an unbounded branch
	// re-introduces the full-window walk this guard prevents.
	probes := regexp.MustCompile(`(?s)t\.ts >= \$1\s+ORDER BY t\.ts DESC LIMIT 1`).FindAllString(q, -1)
	if len(probes) != 2 {
		t.Errorf("pairMarketQuery has %d bounded `ts >= $1 ORDER BY ts DESC LIMIT 1` "+
			"probes, want 2 — MAX(ts) over the recency window must be a single "+
			"backwards index probe per direction, not an aggregate over the window",
			len(probes))
	}
	// The old form computed count_24h as a FILTER over the 14-day scan.
	// The FILTER is cheap; the scan it rode on was not.
	if strings.Contains(q, "count(*) FILTER") {
		t.Error("pairMarketQuery computes count_24h as a FILTER over a wider scan. " +
			"Bind the 24-hour predicate into the scan itself so the planner reads " +
			"only the last day's chunks")
	}
	// Every non-$1 branch (count_24h x2, vol_24h_usd x2, last_price x2) must carry its own
	// 24-hour bound. The $1/$2/$3 checks above prove the $1 branches are the ONLY 14-day reads;
	// this proves none of the other six reads unbounded or rides the 14-day window back in.
	if n := strings.Count(q, "NOW() - INTERVAL '24 hours'"); n != 6 {
		t.Errorf("pairMarketQuery has %d \"NOW() - INTERVAL '24 hours'\" bounds, want "+
			"6 — one on each of count_24h's two directions, vol_24h_usd's two "+
			"directions and last_price's two directions. Fewer means a branch lost "+
			"its bound (an unbounded prices_1m/trades walk); more means a bound was "+
			"duplicated onto a branch that should stay a $1 recency-window probe."+
			"\n\nquery:\n%s", n, q)
	}

	// ── determinism: the last_price sort needs a total order ──
	// `bucket DESC` alone is not one once a bucket holds both orientations, so which leg won
	// would be planner-defined. Same tiebreaker, and same reasoning, as the prices_1m readers.
	if !strings.Contains(q, "ORDER BY bucket DESC, base_asset LIMIT 1") {
		t.Error("pairMarketQuery's last_price sort lacks the base_asset tiebreaker. " +
			"With both directions present in one bucket the winner would be " +
			"planner-defined, which ADR-0015 byte-identical cross-region serving " +
			"cannot rely on")
	}
}
