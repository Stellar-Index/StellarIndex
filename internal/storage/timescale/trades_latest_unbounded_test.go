// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The two "latest" raw-trade readers walk `trades` with no time bound,
// and they have to keep doing it.
//
// This is a NEGATIVE-SPACE guard: it exists to stop a fix, not to prove
// one. On 2026-09-09 an unbounded existence read in
// [Store.hasNonClassicAsset] took GET /v1/assets/native past the 15 s
// budget on r1 and was correctly window-bounded. The sweep that found it
// flagged these two readers as the same shape. They are not, and a
// window here would be a data-loss change dressed as a performance fix,
// so the reasoning is pinned in code where the next sweep will run into
// it:
//
//   - The HasAsset arm bound `base_asset = $1 OR quote_asset = $1`.
//     `trades` is compressed with compress_segmentby = 'base_asset,
//     quote_asset, source' (migration 0001), so a compressed chunk is
//     indexed on those three in that order and a lone `quote_asset`
//     predicate has no leading equality to seek on — every compressed
//     chunk had to be scanned. Both readers below bind `base_asset = $n
//     AND quote_asset = $m` per arm, the leading two segmentby columns,
//     so every chunk is an index SEEK.
//     [TestRawTradeReadsSpanBothStoredDirections] already pins those
//     arms; this file pins the other half.
//   - Measured on r1 2026-08-03 (recorded in
//     internal/api/v1/history_cache.go as a retraction of an earlier
//     estimate that was off by ~1000x): 49 ms native/fiat:USD, 289 ms
//     heaviest pair, 47 ms to prove a novel pair EMPTY — the
//     full-history walk. EXPLAIN on r1 2026-09-05 put the second arm at
//     exactly 2x with the skip scan surviving.
//   - No window preserves the answer. "The latest trade" bounded by W is
//     "the latest trade within W", so a market whose last trade predates
//     W reports NOTHING where it reported a real trade. HasAsset could
//     escape its own window by answering XLM from first principles; a
//     last-trade read cannot, because the answer IS the unbounded
//     question. And the loss lands on quiet networks, where testing does
//     not look: futurenet had ZERO XLM trades in a 14-day window on
//     2026-09-09 while testnet had 2,030.
//
// A test that seeds a row and asserts the trade comes back cannot see
// any of this — a windowed query answers a seeded in-window row exactly
// as correctly. The contract is in the STATEMENT and its bound args, so
// that is what this reads, through the scripted driver.

// latestUnboundedPair is the market under test. Nothing about it is
// load-bearing beyond being a legal non-XLM pair, so no alias family
// widens the read.
func latestUnboundedPair(t *testing.T) canonical.Pair {
	t.Helper()
	base, err := canonical.ParseAsset(dirAQUA)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", dirAQUA, err)
	}
	quote, err := canonical.ParseAsset(dirUSDC)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", dirUSDC, err)
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return pair
}

// tsLowerBound matches a lower bound on `ts` in any spelling a recency
// window could arrive in — `ts >= $4`, `ts > now() - …`, `ts BETWEEN …`.
// An UPPER bound (`ts <= $3`, what [Store.FXQuoteAtOrBefore] binds as
// its cutoff) is deliberately not matched: it caps how NEW a row may be,
// which cannot hide an old one.
// No trailing \b after the operator: `>=` ends in `=`, which is not a
// word character, so `>=\b` never matches anything (it silently passed
// the first mutation run this test was written against).
var tsLowerBound = regexp.MustCompile(`(?i)\bts\s*(?:>=|>|between\b)`)

// TestLatestTradeReadsTakeNoRecencyBound is the guard. Both channels a
// bound could arrive on are checked, because the repo's own idiom
// forbids one of them and would otherwise route around this test: the
// twin fix binds its floor Go-side as a `time.Time` parameter precisely
// so the planner can prune chunks at plan time, and
// TestHasAsset_ProbeWindowIsTheListingWindow forbids the `now() -
// INTERVAL` form. So a window here is either a bound time.Time or a
// now() in the SQL, and both are refused.
func TestLatestTradeReadsTakeNoRecencyBound(t *testing.T) {
	t.Parallel()

	pair := latestUnboundedPair(t)

	reads := []struct {
		name    string
		surface string
		loss    string
		run     func(*Store) error
	}{
		{
			name:    "LatestTradePerSource",
			surface: "/v1/observations and its SSE stream",
			loss: "a market whose last trade predates the window reports NO " +
				"observations at all, where it reported one row per source",
			run: func(s *Store) error {
				_, err := s.LatestTradePerSource(context.Background(), pair, "")
				return err
			},
		},
		{
			name:    "LatestTradesForPair",
			surface: "/v1/price's last-trade arm",
			loss: "a market whose last trade predates the window stops having " +
				"a price at all, rather than having a stale one",
			run: func(s *Store) error {
				_, err := s.LatestTradesForPair(context.Background(), pair, 1)
				return err
			},
		},
	}

	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()

			store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols})
			defer func() { _ = store.db.Close() }()
			if err := r.run(store); err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}
			stmt := conn.only(t)

			const why = `

"Latest" bounded by a window is "latest within the window": a market
whose last trade predates it reports nothing where it reported a real
trade, and the loss lands on the QUIET networks nobody tests against —
futurenet had zero XLM trades in a 14-day window on 2026-09-09 while
testnet had 2,030 in the same one. Unlike Store.HasAsset, which escapes
its window by answering XLM from first principles, there is no
first-principles answer to "what traded last".

The walk is affordable without one: each arm binds base_asset AND
quote_asset, the leading two compress_segmentby columns (migration
0001), so every chunk is an index seek — 47 ms on r1 2026-08-03 to walk
the whole hypertable and prove a novel pair empty. It is not the
Store.HasAsset shape, which bound quote_asset with no leading equality
and had to scan every compressed chunk.

If you are landing something that genuinely keeps the answer — an
expanding window with an unbounded final probe, say — change this test
and say why here. Do not delete it.`

			if loc := tsLowerBound.FindString(stmt.sql); loc != "" {
				t.Errorf("%s now bounds `ts` from below (%q), which silently empties %s.\n\n%s\n\nSQL:\n%s",
					r.name, loc, r.surface, r.loss+why, indent(stmt.sql))
			}
			for i, a := range stmt.args {
				if ts, ok := a.(time.Time); ok {
					t.Errorf("%s binds a timestamp at $%d (%s) — a recency floor by the "+
						"Go-side idiom the twin fix uses.\n\n%s\n\nSQL:\n%s",
						r.name, i+1, ts, r.loss+why, indent(stmt.sql))
				}
			}
			if strings.Contains(strings.ToLower(stmt.sql), "now()") {
				t.Errorf("%s calls now() in SQL — the other spelling of a recency floor.\n\n%s\n\nSQL:\n%s",
					r.name, r.loss+why, indent(stmt.sql))
			}
		})
	}
}

// TestLatestTradePerSource_AnswersAMarketQuietForYears is the
// behavioural companion. It cannot see a window in the SQL — the
// scripted driver replays its row whatever the statement says — but it
// does pin the CONTRACT the guard above protects: a trade from years
// before any plausible window is returned, at its real timestamp, not
// dropped and not re-stamped.
//
// It is here because the guard is a shape assertion, and a shape
// assertion with no statement of what the shape is FOR reads as
// arbitrary to whoever hits it next.
func TestLatestTradePerSource_AnswersAMarketQuietForYears(t *testing.T) {
	t.Parallel()

	pair := latestUnboundedPair(t)
	// Older than MarketsRecencyWindow by three orders of magnitude, and
	// older than any window a future fixer would plausibly pick.
	last := time.Now().UTC().Add(-5 * 365 * 24 * time.Hour).Truncate(time.Second)

	store, _ := newScriptedStore(t, scriptedResult{
		cols: latestTradeCols,
		rows: [][]driver.Value{{
			"sdex", int64(1), dirTxA, int64(0), last,
			pair.Base.String(), pair.Quote.String(),
			dirAQUAAmount, dirUSDCAmount,
			"", "", "",
		}},
	})
	defer func() { _ = store.db.Close() }()

	got, err := store.LatestTradePerSource(context.Background(), pair, "")
	if err != nil {
		t.Fatalf("LatestTradePerSource: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d trade(s), want 1 — a market's last trade is its last "+
			"trade however long ago it was; /v1/observations has no staleness floor", len(got))
	}
	if !got[0].Timestamp.Equal(last) {
		t.Errorf("timestamp = %s, want %s — the row is served as observed, not re-stamped",
			got[0].Timestamp, last)
	}
	if got[0].BaseAmount.String() != dirAQUAAmount || got[0].QuoteAmount.String() != dirUSDCAmount {
		t.Errorf("amounts = (%s, %s), want (%s, %s) — the pair is stored in the requested "+
			"orientation, so no leg swap applies",
			got[0].BaseAmount, got[0].QuoteAmount, dirAQUAAmount, dirUSDCAmount)
	}
}
