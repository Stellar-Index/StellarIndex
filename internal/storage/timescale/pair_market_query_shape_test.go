package timescale

import (
	"regexp"
	"strings"
	"testing"
)

// [pairMarketQuery] — the single-pair summary behind /v1/pairs — must
// keep two shape properties. Both were absent until 2026-09-03, and
// their absence made /v1/pairs the slowest route on r1: a per-route p99 of
// 4,875-4,950 ms against ADR-0009's 500 ms target, measured in 6-hour
// windows both during and well away from any load event, so chronic rather
// than an artefact. The cost is per-REQUEST, not per-cold-slot: five
// consecutive identical ?base=crypto:BTC&quote=crypto:USDT requests measured
// 4.55 / 4.66 / 4.44 / 4.37 s, because CachedMarketsReader.PairMarket is a
// straight pass-through and no prewarm names the route. The endpoint stays
// invisible in the GLOBAL p99 the status page publishes only because organic
// traffic is ~2.5 req/h -- which is what makes a shape guard worth having
// here: nothing in production monitoring would report the regression.
//
// 1. Both stored orientations are a UNION ALL of two single-direction
// branches, never one `(A AND B) OR (B AND A)` disjunction. The planner
// cannot drive trades_pair_ts_idx / prices_1m_pair_bucket_idx
// (base_asset, quote_asset, ts|bucket DESC) from an OR of two different
// equality pairs, so it falls back to the bare time index and applies
// the pair test as a post-index filter. This is the same defect
// [TestBothDirectionReadersUseUnionNotOr] guards for prices_1m (#441);
// PairMarket carried it on BOTH prices_1m subqueries AND on the trades
// scan, and was missed by that sweep because it is not driven by
// `ORDER BY bucket DESC LIMIT n`.
//
// 2. Each aggregate is bounded by the smallest window that can produce
// it. The old form asked ONE 14-day (MarketsRecencyWindow) scan of
// `trades` for two answers that need far less: MAX(ts) is a single
// backwards index probe per direction, and count_24h needs 24 hours.
// Reading 14 days for both meant crypto:BTC/crypto:USDT materialised
// 17.2 MILLION rows to return a timestamp and a count. This half is the
// larger win and has no prices_1m analogue, so it is pinned here.
//
// Measured on r1 (2026-09-03) on an idle box, trades at 140 GB. Two
// runs per form, so the first column is a cold buffer pool and the
// second a warm one:
//
//	pair                        OLD                NEW
//	crypto:BTC/crypto:USDT   95727.9 / 4324.3   120.3 / 107.0 ms
//	crypto:XLM/fiat:USD       1168.6 / 1202.8    22.0 /  13.6 ms
//	native/USDC-GA5Z…         2999.3 /  741.6    26.1 /  19.5 ms
//	native/fiat:USD (empty)     74.5 /   63.3     2.3 /   2.4 ms
//
// The 95.7-second cold read is past the API's 8-second handler
// ceiling: that request could not complete at all.
//
// Guarding the template text rather than a plan is deliberate, for the
// reason [TestBothDirectionReadersUseUnionNotOr] gives: it needs no
// database, so a regression fails in the unit suite instead of surfacing
// as production latency. The behaviour-level counterpart — that the plan
// really does stop reading at the 24-hour boundary — is
// TestPairMarket_DoesNotScanTheWholeRecencyWindow in test/integration.
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

	// ── correctness half: BOTH orientations must still be read ──
	// A UNION ALL that dropped a leg would be fast and wrong, which is
	// exactly the trade this guard exists to prevent: the SDEX decoder
	// records XLM/USDC and USDC/XLM as separate rows, so a single-leg
	// read silently halves count_24h and vol_24h_usd.
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

	// ── window half: nothing may read MarketsRecencyWindow in bulk ──
	// $1 is the 14-day bound. It may appear ONLY on branches that are
	// LIMIT 1 index probes (the two last_trade_at reads). Any other use
	// is a 14-day scan answering a 24-hour question.
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

	// ── determinism: the last_price sort needs a total order ──
	// `bucket DESC` alone is not one once a bucket holds both
	// orientations — native/USDC has 1,270 such buckets in a day on r1 —
	// so which leg won was planner-defined. Same tiebreaker, and same
	// reasoning, as #441.
	if !strings.Contains(q, "ORDER BY bucket DESC, base_asset LIMIT 1") {
		t.Error("pairMarketQuery's last_price sort lacks the base_asset tiebreaker. " +
			"With both directions present in one bucket the winner would be " +
			"planner-defined, which ADR-0015 byte-identical cross-region serving " +
			"cannot rely on")
	}
}
