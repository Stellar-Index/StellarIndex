package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAccountTradesQuery_Shape pins the per-address trades read to the
// two-arm UNION discipline its file header documents: each arm keys one
// account column (taker / maker — the columns migration 0123 indexes),
// carries its OWN ORDER BY + LIMIT so it walks its partial index in
// output order and stops after one page, and the maker arm excludes
// taker-duplicated rows so a self-crossed trade appears once.
func TestAccountTradesQuery_Shape(t *testing.T) {
	for _, hasCursor := range []bool{false, true} {
		q := accountTradesQuery(hasCursor)

		parts := strings.Split(q, "UNION ALL")
		if len(parts) != 2 {
			t.Fatalf("hasCursor=%v: query is not a two-arm UNION:\n%s", hasCursor, q)
		}
		if !strings.Contains(parts[0], "WHERE taker = $1 AND ts >= $2") {
			t.Errorf("hasCursor=%v: arm 1 must key the taker index with the compression-horizon floor:\n%s", hasCursor, parts[0])
		}
		if !strings.Contains(parts[1], "WHERE maker = $1 AND ts >= $2 AND (taker IS NULL OR taker <> $1)") {
			t.Errorf("hasCursor=%v: arm 2 must key the maker index and exclude taker-arm rows:\n%s", hasCursor, parts[1])
		}
		const orderBy = "ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC LIMIT"
		if got := strings.Count(q, orderBy); got != 3 {
			t.Errorf("hasCursor=%v: want the full ORDER BY+LIMIT on both arms AND the outer merge (3 total), got %d:\n%s",
				hasCursor, got, q)
		}

		wantCursor := strings.Count(q, "(ts, ledger, tx_hash, op_index) < ($3, $4, $5, $6)")
		if hasCursor && wantCursor != 2 {
			t.Errorf("cursor page: both arms must carry the keyset tuple comparison, got %d:\n%s", wantCursor, q)
		}
		if !hasCursor && wantCursor != 0 {
			t.Errorf("first page: no cursor clause expected, got %d:\n%s", wantCursor, q)
		}
	}
}

// TestAccountTradesQuery_NoFloatCasts — ADR-0003: the NUMERIC amount
// columns must reach the driver as text, never through a float cast.
func TestAccountTradesQuery_NoFloatCasts(t *testing.T) {
	q := accountTradesQuery(true)
	for _, col := range []string{"base_amount::text", "quote_amount::text", "usd_volume::text"} {
		if !strings.Contains(q, col) {
			t.Errorf("query must cast %s server-side:\n%s", col, q)
		}
	}
	assertNoFloatSQL(t, "account trades query", q)
}

func TestClampAccountTradesLimit(t *testing.T) {
	cases := map[int]int{
		-1:   accountTradesDefaultLimit,
		0:    accountTradesDefaultLimit,
		1:    1,
		50:   50,
		200:  200,
		201:  accountTradesDefaultLimit,
		9999: accountTradesDefaultLimit,
	}
	for in, want := range cases {
		if got := clampAccountTradesLimit(in); got != want {
			t.Errorf("clampAccountTradesLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestAccountTradesQuery_OuterColumnsExistInSubquery pins the invariant
// whose violation made this endpoint 500 for every account, always.
//
// The outer SELECT projects from the UNION subquery by NAME, so every
// name it lists must be an output column of both arms. That held for
// plain columns and for `x::text` (a cast preserves the name), but NOT
// for the empty-string coalesce
//
//	COALESCE(usd_volume::text, '')
//
// — PostgreSQL names that output column `coalesce`, so `usd_volume`
// resolved against nothing and the statement failed at PLAN time with
// 42703. The endpoint had therefore
// never served a row, and TestAccountTradesQuery_Shape could not see it
// because it asserts substrings of the query STRING rather than the
// relationship between the two column lists (cold audit 2026-08-04).
//
// This test compares the two lists structurally, so it fails for ANY
// future expression added to the inner list without an alias — not just
// the two COALESCEs that caused the original outage.
func TestAccountTradesQuery_OuterColumnsExistInSubquery(t *testing.T) {
	produced := make(map[string]bool)
	for _, col := range strings.Split(accountTradesInnerCols, ",") {
		col = strings.TrimSpace(strings.ReplaceAll(col, "\n", " "))
		if col == "" {
			continue
		}
		// An aliased expression contributes its alias; a bare column
		// contributes itself. Anything else (an unaliased expression)
		// contributes a name PostgreSQL derives from the function —
		// which is the bug this test exists to catch, so record the
		// expression verbatim and let the comparison below fail.
		name := col
		if i := strings.LastIndex(strings.ToUpper(col), " AS "); i >= 0 {
			name = strings.TrimSpace(col[i+4:])
		} else if strings.ContainsAny(col, "(:") {
			name = "<unaliased expression: " + col + ">"
		}
		produced[name] = true
	}

	for _, want := range strings.Split(accountTradesOuterCols, ",") {
		want = strings.TrimSpace(strings.ReplaceAll(want, "\n", " "))
		if want == "" {
			continue
		}
		if !produced[want] {
			t.Errorf("outer SELECT projects %q, which no UNION arm produces — "+
				"the statement will fail at plan time with 42703.\narm outputs: %v",
				want, produced)
		}
	}
}

// resetTradesHorizonCache clears the package-level horizon cache
// (tradesUncompressedHorizon caches across every *Store instance) so a
// test controls whether the function issues a query, and restores the
// prior value afterward. Callers must not run in parallel with each
// other or with anything else touching the cache.
func resetTradesHorizonCache(t *testing.T) {
	t.Helper()
	tradesHorizonMu.Lock()
	origHorizon, origAt := tradesHorizon, tradesHorizonAt
	tradesHorizon, tradesHorizonAt = time.Time{}, time.Time{}
	tradesHorizonMu.Unlock()
	t.Cleanup(func() {
		tradesHorizonMu.Lock()
		tradesHorizon, tradesHorizonAt = origHorizon, origAt
		tradesHorizonMu.Unlock()
	})
}

// TestTradesUncompressedHorizon_QueryShape pins #1157's root-cause fix:
// the floor must come from max(range_end) over COMPRESSED chunks, never
// from min(range_start) over uncompressed ones. The pre-fix expression
// let an old uncompressed straggler chunk (a stuck compression job, a
// late-arriving backfill) drag the floor to that chunk's start — 2021
// was observed on r1 while 313 of 472 trades chunks were already
// compressed — because it is NOT a time prefix: a chunk newer than the
// straggler can be compressed while the straggler itself is not. The
// new floor's source set is compressed chunks only, so a straggler
// (uncompressed, by definition excluded from that set) cannot reach it
// at all, regardless of how old it is.
func TestTradesUncompressedHorizon_QueryShape(t *testing.T) {
	resetTradesHorizonCache(t)
	newestCompressedEnd := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"floor", "stranded_uncompressed_chunk"},
		rows: [][]driver.Value{{newestCompressedEnd, false}},
	})

	got := store.tradesUncompressedHorizon(context.Background())
	if !got.Equal(newestCompressedEnd) {
		t.Fatalf("horizon = %s, want %s (the newest COMPRESSED chunk's end)", got, newestCompressedEnd)
	}

	stmt := conn.only(t)
	if !strings.Contains(stmt.sql, "max(range_end)") {
		t.Errorf("floor must be computed from max(range_end):\n%s", stmt.sql)
	}
	if !strings.Contains(stmt.sql, "WHERE hypertable_name = 'trades' AND is_compressed") {
		t.Errorf("floor's source set must be compressed chunks only:\n%s", stmt.sql)
	}
	if strings.Contains(stmt.sql, "min(range_start)") {
		t.Errorf("must not resurrect the pre-#1157 min(range_start) expression:\n%s", stmt.sql)
	}
	if !strings.Contains(stmt.sql, "EXISTS") || !strings.Contains(stmt.sql, "NOT c.is_compressed") {
		t.Errorf("must carry the stranded-uncompressed-chunk consistency check:\n%s", stmt.sql)
	}
}

// TestTradesUncompressedHorizon_StrandedChunkDoesNotWidenFloor: a
// straggler uncompressed chunk predating the newest compressed chunk is
// an anomaly worth logging, but it must NOT alter the served floor —
// widening it back down toward the straggler would reintroduce the
// exact compressed-chunk scan #1157 exists to avoid.
func TestTradesUncompressedHorizon_StrandedChunkDoesNotWidenFloor(t *testing.T) {
	resetTradesHorizonCache(t)
	newestCompressedEnd := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	store, _ := newScriptedStore(t, scriptedResult{
		cols: []string{"floor", "stranded_uncompressed_chunk"},
		rows: [][]driver.Value{{newestCompressedEnd, true}},
	})

	got := store.tradesUncompressedHorizon(context.Background())
	if !got.Equal(newestCompressedEnd) {
		t.Errorf("a stranded uncompressed chunk changed the floor: got %s, want %s", got, newestCompressedEnd)
	}
}

// TestTradesUncompressedHorizon_FailsClosedOnLookupError: a catalog
// lookup error must degrade to a short, recent floor — NOT epoch/zero.
// Epoch means "no floor", which sends the read straight into the
// unindexed full-history scan that caused the original 8s-timeout/503
// incident; a short recent floor instead surfaces through the same
// "showing trades since <date>" / trades_total_since coverage note a
// legitimate horizon does.
func TestTradesUncompressedHorizon_FailsClosedOnLookupError(t *testing.T) {
	resetTradesHorizonCache(t)
	store, _ := newScriptedStore(t, scriptedResult{err: errors.New("catalog unavailable")})

	before := time.Now()
	got := store.tradesUncompressedHorizon(context.Background())
	after := time.Now()

	if got.IsZero() || got.Year() < 1971 {
		t.Fatalf("must fail CLOSED to a recent floor, not epoch/zero: got %s", got)
	}
	earliest := before.Add(-tradesHorizonFailClosedWindow)
	latest := after.Add(-tradesHorizonFailClosedWindow)
	if got.Before(earliest) || got.After(latest) {
		t.Errorf("fail-closed floor = %s, want within [%s, %s]", got, earliest, latest)
	}
}
