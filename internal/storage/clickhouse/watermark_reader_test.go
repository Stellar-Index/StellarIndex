package clickhouse

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// wmFakeRow scans back fixed uint64 values, one per destination.
type wmFakeRow struct {
	driver.Row
	vals []uint64
}

func (r *wmFakeRow) Scan(dest ...any) error {
	for i := range dest {
		*dest[i].(*uint64) = r.vals[i]
	}
	return nil
}

// wmFakeConn answers the watermark query (four columns) and the lake-min
// query (one column), counting every QueryRow on this one connection.
type wmFakeConn struct {
	driver.Conn
	queryRowCalls int
	lastArgs      []any
	wmVals        []uint64
}

func (c *wmFakeConn) QueryRow(_ context.Context, q string, args ...any) driver.Row {
	c.queryRowCalls++
	c.lastArgs = args
	if q == `SELECT toUInt64(min(ledger_seq)) FROM stellar.ledgers` {
		return &wmFakeRow{vals: []uint64{2}}
	}
	if c.wmVals != nil {
		return &wmFakeRow{vals: c.wmVals}
	}
	return &wmFakeRow{vals: []uint64{200, 0, 100, 200}} // chMax, firstGap, minPresent, maxInWindow
}

// TestWatermarkReader_ReusesConnectionAcrossCalls: every read a
// WatermarkReader serves runs on the one connection it holds.
func TestWatermarkReader_ReusesConnectionAcrossCalls(t *testing.T) {
	conn := &wmFakeConn{}
	w := &WatermarkReader{conn: conn}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		got, err := w.ContiguousWatermark(ctx, 100, 1_000)
		if err != nil {
			t.Fatalf("call %d: ContiguousWatermark: %v", i, err)
		}
		if got != 200 {
			t.Fatalf("call %d: ContiguousWatermark = %d, want 200", i, got)
		}
	}
	lo, err := w.LakeMinLedger(ctx)
	if err != nil {
		t.Fatalf("LakeMinLedger: %v", err)
	}
	if lo != 2 {
		t.Fatalf("LakeMinLedger = %d, want 2", lo)
	}
	if conn.queryRowCalls != 4 {
		t.Fatalf("queryRowCalls = %d, want 4 (every read on the reader's one connection)", conn.queryRowCalls)
	}
}

// TestWatermarkReader_BoundsScanAndClamps: the gap scan is bound to [from, to]
// and the answer never exceeds to, even when the lake extends past it.
func TestWatermarkReader_BoundsScanAndClamps(t *testing.T) {
	conn := &wmFakeConn{wmVals: []uint64{200, 0, 100, 150}}
	w := &WatermarkReader{conn: conn}

	got, err := w.ContiguousWatermark(context.Background(), 100, 150)
	if err != nil {
		t.Fatalf("ContiguousWatermark: %v", err)
	}
	if got != 150 {
		t.Fatalf("ContiguousWatermark = %d, want 150 (clamped to the bound; lake tip is 200)", got)
	}
	if want := []any{uint32(100), uint32(150), uint32(100), uint32(100), uint32(150)}; !slices.Equal(conn.lastArgs, want) {
		t.Fatalf("query args = %v, want %v", conn.lastArgs, want)
	}
}

// TestWatermarkReader_HoleStraddlingBound: a hole running from inside the
// window past `to` is invisible to the bounded gap scan; the watermark must stop
// below it rather than report the whole window contiguous.
func TestWatermarkReader_HoleStraddlingBound(t *testing.T) {
	tests := []struct {
		name string
		vals []uint64 // chMax, firstGap, minPresent, maxInWindow
		from uint32
		to   uint32
		want uint32
	}{
		{"lake 100..200 missing 146..160, window [100,150]", []uint64{200, 0, 100, 145}, 100, 150, 145},
		{"single ledger missing at to", []uint64{200, 0, 100, 149}, 100, 150, 149},
		{"window wholly beyond the lake tip", []uint64{200, 0, 0, 0}, 300, 400, 299},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &WatermarkReader{conn: &wmFakeConn{wmVals: tt.vals}}
			got, err := w.ContiguousWatermark(context.Background(), tt.from, tt.to)
			if err != nil {
				t.Fatalf("ContiguousWatermark: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ContiguousWatermark = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestLakeWatermark_ClampsToContiguousTip: LakeWatermark must not surface as_of_ledger past a live-sink
// hole, since LiveSink drops whole ledgers under buffer pressure (ADR-0041). A hole inside the gap window
// must clamp the ledger and close time to the contiguous tip (firstGap-1), not the raw max.
func TestLakeWatermark_ClampsToContiguousTip(t *testing.T) {
	const (
		rawTip        = uint32(100_020)
		firstGap      = uint64(100_005) // ledger 100_005 is missing
		wantWatermark = uint32(100_004) // firstGap - 1
	)
	from := rawTip - 10_000 // 90_020: rawTip - lakeWatermarkGapWindow, the query's own `from`

	rawCloseTime := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)   // stamped on the (wrong) raw tip
	wantCloseTime := time.Date(2026, 9, 21, 11, 30, 0, 0, time.UTC) // stamped on the true watermark

	isContiguityQuery := func(q string) bool { return strings.Contains(q, "leadInFrame") }
	// Unclamped shape: ONE query selecting both max(ledger_seq) and
	// max(close_time), no WHERE, no window function.
	isCombinedTipQuery := func(q string) bool {
		return strings.Contains(q, "max(ledger_seq)") && strings.Contains(q, "max(close_time)") && !isContiguityQuery(q)
	}
	// Fixed shape, query 1: raw tip alone.
	isRawTipOnlyQuery := func(q string) bool {
		return strings.Contains(q, "max(ledger_seq)") && !strings.Contains(q, "max(close_time)") && !isContiguityQuery(q)
	}
	// Fixed shape, query 3: close time AT the resolved watermark ledger.
	isCloseTimeAtLedgerQuery := func(q string) bool {
		return strings.Contains(q, "max(close_time)") && strings.Contains(q, "WHERE ledger_seq = ?")
	}

	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isContiguityQuery(q):
			return &stubRows{data: [][]any{{uint64(rawTip), firstGap, uint64(from), uint64(rawTip)}}}, nil
		case isCloseTimeAtLedgerQuery(q):
			return &stubRows{data: [][]any{{wantCloseTime}}}, nil
		case isCombinedTipQuery(q):
			return &stubRows{data: [][]any{{rawTip, rawCloseTime}}}, nil
		case isRawTipOnlyQuery(q):
			return &stubRows{data: [][]any{{rawTip}}}, nil
		default:
			t.Fatalf("unexpected query shape: %s", q)
			return nil, nil
		}
	}

	r := &ExplorerReader{conn: conn}
	ledger, closedAt, err := r.LakeWatermark(context.Background())
	if err != nil {
		t.Fatalf("LakeWatermark: %v", err)
	}
	if ledger != wantWatermark {
		t.Fatalf("LakeWatermark ledger = %d, want %d (the contiguous tip below the hole at %d) — "+
			"as_of_ledger must never point past an uncaptured ledger", ledger, wantWatermark, firstGap)
	}
	if !closedAt.Equal(wantCloseTime) {
		t.Fatalf("LakeWatermark close time = %s, want %s (the watermark ledger's own close time, "+
			"not the raw tip's)", closedAt, wantCloseTime)
	}
}

// Tests for the per-account activity watermark bound: each AccountOperations key arm bounds its resolve with
// `ledger_seq <= watermark` so the reverse read starts at the account's last activity. They pin SQL and bind
// order; test/integration/clickhouse_accounts_test.go proves it against live ClickHouse.

// Query-shape classifiers for the watermark reads.
func isAccountActivityProbe(q string) bool {
	return strings.Contains(q, "stellar.account_activity") && !strings.Contains(q, "WHERE")
}

func isAccountActivityLookup(q string) bool {
	return strings.Contains(q, "max(last_ledger)") &&
		strings.Contains(q, "stellar.account_activity") &&
		strings.Contains(q, "WHERE account_id = ?")
}

// watermarkStubConn routes the ops_by_source probe and the account_activity
// probe + lookup to real single-column rows (so the reader takes the bounded
// path) and everything else to `rows`.
func watermarkStubConn(watermark uint32, rows *stubRows) *stubConn {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isOpsBySourceProbe(q):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case isAccountActivityProbe(q):
			return &stubRows{data: [][]any{{watermark}}}, nil
		case isAccountActivityLookup(q):
			return &stubRows{data: [][]any{{watermark}}}, nil
		default:
			return rows, nil
		}
	}
	return conn
}

// keyReads returns the emitted key reads (ops_by_source / participants) and
// their args.
func keyReads(conn *stubConn) (qs []string, args [][]any) {
	for i, q := range conn.queries {
		if isOpsBySourceProbe(q) || isVisibilityLookup(q) {
			continue
		}
		if strings.Contains(q, "FROM stellar.ops_by_source") || strings.Contains(q, "FROM stellar.operation_participants") {
			qs, args = append(qs, q), append(args, conn.args[i])
		}
	}
	return qs, args
}

func TestAccountOperations_WatermarkBoundsEachArmResolve(t *testing.T) {
	const (
		limit     = 37
		watermark = uint32(63_411_270)
	)
	conn := watermarkStubConn(watermark, &stubRows{})
	if _, _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	// BOTH arms must carry the bound — a bound on one arm, or on the
	// hydration only, leaves the other arm's reverse read walking the tip.
	qs, args := keyReads(conn)
	if len(qs) != 2 {
		t.Fatalf("key reads = %d, want 2 (sourced + participant windows): %v", len(qs), conn.queries)
	}
	for i, q := range qs {
		if !strings.Contains(q, "AND ledger_seq <= ?") || args[i][1] != watermark {
			t.Errorf("key read %d is unbounded or binds %v as the bound (#31):\n%s", i, args[i], q)
		}
	}
	q, got := exactOpsArm(t, watermarkStubConn(watermark, &stubRows{}).respond, limit, ExplorerCursor{})
	assertArgs(t, q, got, []any{"GTEST", watermark, limit})
}

func TestAccountOperations_WatermarkPreservesCursorArgOrder(t *testing.T) {
	const (
		limit     = 9
		watermark = uint32(63_411_270)
	)
	cur := ExplorerCursor{Ledger: 63_000_000, A: 4, B: 2}
	q, got := exactOpsArm(t, watermarkStubConn(watermark, &stubRows{}).respond, limit, cur)
	// The bound precedes the cursor tuple in the text, so it must in the args.
	if i, j := strings.Index(q, "ledger_seq <= ?"), strings.Index(q, "< (?, ?, ?)"); !(i >= 0 && j > i) {
		t.Fatalf("bound clause must precede the cursor clause in the emitted SQL:\n%s", q)
	}
	assertArgs(t, q, got, []any{"GTEST", watermark, cur.Ledger, cur.Ledger, cur.A, cur.B, limit})
}

// TestAccountOperations_NoWatermarkFallsBackUnbounded pins the degrade direction: no watermark row means no
// bound. A missing watermark may only cost performance; a fabricated bound of 0 would hide the account's
// whole history.
func TestAccountOperations_NoWatermarkFallsBackUnbounded(t *testing.T) {
	const limit = 37
	conn := watermarkStubConn(0, &stubRows{})
	if _, _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	qs, _ := keyReads(conn)
	for _, q := range qs {
		if strings.Contains(q, "ledger_seq <= ?") {
			t.Fatalf("no watermark, yet a key read carries a bound:\n%s", q)
		}
	}
	q, got := exactOpsArm(t, watermarkStubConn(0, &stubRows{}).respond, limit, ExplorerCursor{})
	assertArgs(t, q, got, []any{"GTEST", limit})
}

func assertArgs(t *testing.T, q string, got, want []any) {
	t.Helper()
	if n := strings.Count(q, "?"); n != len(got) {
		t.Fatalf("query has %d placeholders, reader bound %d args: %v\n%s", n, len(got), got, q)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

// stellar.operations is ReplacingMergeTree, so account listings collapse duplicates on the narrow primary
// key, never a DISTINCT over opCols (large body_xdr). body_xdr is read once, by the FINAL hydration (see
// opColsLight).
func TestAccountOperations_DedupsOnThePrimaryKey(t *testing.T) {
	const limit = 3
	filled := make([][]any, windowRows(limit, windowFactorKeys))
	for i := range filled {
		filled[i] = keyRow(100, 0, 0) // forces the exact sourced arm
	}
	router := &opsArmRouter{sourced: filled, hydrated: [][]any{opRowFor(100, 0, 0)}}
	conn := &stubConn{respond: withOpsBySourceRows(func(q string) (driver.Rows, error) {
		if strings.Contains(q, "LIMIT 1 BY") {
			return &stubRows{data: [][]any{keyRow(100, 0, 0)}}, nil
		}
		return router.respond(q)
	})}
	rows, _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{})
	if err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	var exact, wide int
	for _, q := range conn.queries {
		if strings.Contains(q, "LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?") {
			exact++
		}
		if strings.Contains(q, "body_xdr") {
			wide++
			if !strings.Contains(q, "FROM stellar.operations FINAL") {
				t.Errorf("body_xdr read outside the FINAL hydration: %s", q)
			}
		}
		if strings.Contains(q, "DISTINCT") {
			t.Errorf("an account-operations read dedups with DISTINCT: %s", q)
		}
	}
	if exact != 1 || wide != 1 {
		t.Fatalf("exact LIMIT 1 BY reads = %d, body_xdr reads = %d; want 1 and 1: %v", exact, wide, conn.queries)
	}
}

func TestAccountOperations_SkipsFailedParticipantTxs(t *testing.T) {
	const limit = 3
	router := &opsArmRouter{
		sourced:     [][]any{keyRow(98, 0, 0)},
		participant: [][]any{keyRow(100, 0, 1), keyRow(100, 0, 0), keyRow(99, 0, 0), keyRow(97, 0, 0)},
		failed:      map[[2]uint32]bool{{100, 0}: true},
	}
	conn := &stubConn{respond: withOpsBySourceRows(router.respond)}
	if _, _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	last := conn.queries[len(conn.queries)-1]
	if !strings.Contains(last, "IN ((99,0,0),(98,0,0),(97,0,0))") {
		t.Fatalf("hydration must skip both ops of the failed participant tx and still fill the page: %s", last)
	}
}

// When the participant arm stops at its frontier, the reader serves a short
// page with resume at that frontier, drops sourced keys past it, and the
// next page carries on with no row skipped or repeated.
func TestAccountOperations_BudgetResumeIsExact(t *testing.T) {
	const (
		limit  = 50
		failed = 10000
	)
	lake := &historyLake{participantLake: participantLake{failed: map[accountTxKey]bool{}}}
	var want []accountOpKey
	ledger := uint32(failed + 2*limit)
	for i := 0; i < limit/2; i++ {
		k := accountOpKey{ledger, 1, 0}
		lake.rows = append(lake.rows, k)
		want = append(want, k)
		ledger--
	}
	for i := 0; i < failed; i++ {
		lake.rows = append(lake.rows, accountOpKey{ledger, 1, 0})
		lake.failed[accountTxKey{ledger, 1}] = true
		if i%200 == 0 { // the account's own ops, inside and past the run
			k := accountOpKey{ledger, 0, 0}
			lake.sourced = append(lake.sourced, k)
			want = append(want, k)
		}
		ledger--
	}
	tail := accountOpKey{ledger, 1, 0}
	lake.rows = append(lake.rows, tail)
	want = append(want, tail)

	r := &ExplorerReader{conn: lake}
	var got []accountOpKey
	var cur ExplorerCursor
	resumes := 0
	for page := 0; page < 1000; page++ {
		rows, resume, err := r.AccountOperations(context.Background(), "GTEST", limit, cur)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, o := range rows {
			got = append(got, accountOpKey{o.Seq, o.TxIndex, o.OpIndex})
		}
		switch {
		case resume.IsSet():
			if len(rows) >= limit {
				t.Fatalf("page %d: resume on a full page", page)
			}
			resumes++
			cur = resume
		case len(rows) == limit:
			last := rows[len(rows)-1]
			cur = ExplorerCursor{Ledger: last.Seq, A: last.TxIndex, B: last.OpIndex}
		default:
			if resumes == 0 {
				t.Fatal("the walk never stopped at a frontier")
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("walk returned %d rows, want %d in order\n got: %v\nwant: %v", len(got), len(want), got, want)
			}
			return
		}
	}
	t.Fatal("walk did not end")
}

// TestAccountOperations_BoundArgsBindInsideKeyArms guards the exact sourced
// arm's bind order with a cursor and no watermark: no bound placeholder, and
// the args line up with the emitted SQL.
func TestAccountOperations_BoundArgsBindInsideKeyArms(t *testing.T) {
	cur := ExplorerCursor{Ledger: 63_000_000, A: 4, B: 2}
	q, args := exactOpsArm(t, withOpsBySourceRows(func(string) (driver.Rows, error) { return &stubRows{}, nil }), 9, cur)
	if strings.Count(q, "ledger_seq <= ?") != strings.Count(q, "ledger_seq <= ? AND (ledger_seq, tx_index, op_index) < (?, ?, ?)") {
		t.Fatalf("unbounded path must not carry a bound placeholder: %s", q)
	}
	assertArgs(t, q, args, []any{"GTEST", cur.Ledger, cur.Ledger, cur.A, cur.B, 9})
}

func TestAccountOperations_WindowedReadHasNoLimit1By(t *testing.T) {
	const limit = 3
	router := &opsArmRouter{
		sourced:     [][]any{keyRow(100, 0, 0), keyRow(100, 0, 0), keyRow(98, 0, 0)},
		participant: [][]any{keyRow(99, 0, 0), keyRow(97, 0, 0)},
		hydrated:    [][]any{opRowFor(100, 0, 0), opRowFor(99, 0, 0), opRowFor(98, 0, 0)},
	}
	conn := &stubConn{respond: withOpsBySourceRows(router.respond)}
	r := &ExplorerReader{conn: conn}

	rows, _, err := r.AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{Ledger: 200, A: 1, B: 2})
	if err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	var arms, hydrations int
	for i, q := range conn.queries {
		if isOpsBySourceProbe(q) || strings.Contains(q, "account_activity") || isVisibilityLookup(q) || isOpTypesCoverageProbe(q) {
			continue
		}
		if strings.Contains(q, "LIMIT 1 BY") {
			t.Fatalf("a windowed read carries LIMIT 1 BY (defeats read-in-order): %s", q)
		}
		switch {
		case strings.Contains(q, "FROM stellar.operations FINAL"):
			hydrations++
			if !strings.Contains(q, "IN ((100,0,0),(99,0,0),(98,0,0))") {
				t.Errorf("hydration is not keyed on the merged, deduped page: %s", q)
			}
		default:
			arms++
			want := []any{"GTEST", uint32(200), uint32(200), uint32(1), uint32(2), windowRows(limit, windowFactorKeys)}
			got := conn.args[i]
			if len(got) != len(want) {
				t.Fatalf("arm args = %v, want %v", got, want)
			}
			for j := range want {
				if got[j] != want[j] {
					t.Fatalf("arm arg %d = %v, want %v", j, got[j], want[j])
				}
			}
		}
	}
	if arms != 2 || hydrations != 1 {
		t.Fatalf("arms=%d hydrations=%d, want 2 and 1: %v", arms, hydrations, conn.queries)
	}
}

// A filled window that holds fewer than `limit` complete keys cannot prove the
// page; the reader must run the exact query rather than serve a short page.
func TestAccountOperations_UnprovenWindowFallsBackToExactQuery(t *testing.T) {
	const limit = 3
	window := windowRows(limit, windowFactorKeys)
	filled := make([][]any, window)
	for i := range filled {
		filled[i] = keyRow(100, 0, 0)
	}
	router := &opsArmRouter{sourced: filled, hydrated: [][]any{opRowFor(100, 0, 0)}}
	conn := &stubConn{respond: withOpsBySourceRows(router.respond)}
	r := &ExplorerReader{conn: conn}

	if _, _, err := r.AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	if exactQueries(conn, sourcedOpKeysExactQuery(false, false)) != 1 {
		t.Fatalf("the sourced arm did not fall back to its exact LIMIT 1 BY query: %v", conn.queries)
	}
}

// Regression tests: ContractEventsRecent and EventsByTx read a ReplacingMergeTree, so the SQL must carry
// FINAL / LIMIT 1 BY / uniqExact to avoid serving duplicates mid-merge. The stub cannot merge parts, so
// these assert the SQL; the live proof is TestClickHouseContractEventsRMTDedup.

// contractEventRecentRowFor builds the 9-column ContractActivityRow that
// ContractEventsRecent's scan expects (ledger_seq, close_time, tx_hash,
// op_index, event_index, event_type, topic_0_sym, topics_xdr, data_xdr).
func contractEventRecentRowFor(seq uint32, opIndex, eventIndex uint32) []any {
	return []any{
		seq, time.Unix(1700000000, 0).UTC(), "txhash", opIndex, eventIndex,
		"contract", "transfer",
		[]string{"AAAA"},
		"",
	}
}

// TestContractEventsRecent_FastPathKeepsReadInOrder: the fast query must carry neither `FINAL` (defeats
// the contract_id bloom skip-index) nor `LIMIT 1 BY` (disables reverse read-in-order early exit, 16.3s vs
// 0.16s); dedup is a Go-side adjacent-row collapse.
func TestContractEventsRecent_FastPathKeepsReadInOrder(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		// Ledger-index probe: empty → index unavailable → legacy path.
		if strings.Contains(q, "contract_active_ledgers") {
			return &stubRows{}, nil
		}
		if !strings.Contains(q, "FROM stellar.contract_events") {
			t.Fatalf("unexpected query: %s", q)
		}
		// Duplicate row-identity (un-merged RMT part) + a distinct row.
		return &stubRows{data: [][]any{
			contractEventRecentRowFor(100, 0, 0),
			contractEventRecentRowFor(100, 0, 0),
			contractEventRecentRowFor(99, 0, 0),
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.ContractEventsRecent(context.Background(), "CTESTCONTRACT", 100, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	// The duplicate part must still collapse — now in Go.
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (adjacent duplicate collapsed)", len(rows))
	}
	if len(conn.queries) != 2 {
		t.Fatalf("issued %d queries, want 2 (index probe + events; no fallback on a clean page)", len(conn.queries))
	}
	q := conn.queries[len(conn.queries)-1]
	if strings.Contains(q, "ledger_seq IN") {
		t.Fatalf("query = %q, must NOT carry a ledger bound when the index is unavailable", q)
	}
	if strings.Contains(q, "LIMIT 1 BY") {
		t.Fatalf("query = %q, must NOT use LIMIT 1 BY (disables reverse read-in-order early exit)", q)
	}
	if strings.Contains(q, "stellar.contract_events FINAL") {
		t.Fatalf("query = %q, must NOT use FINAL on the bloom-probed scan", q)
	}
	if !strings.Contains(q, "ORDER BY ledger_seq DESC, tx_hash DESC, op_index DESC, event_index DESC LIMIT ?") {
		t.Fatalf("query = %q, want plain ORDER BY … LIMIT ? (read-in-order eligible)", q)
	}
}

// TestContractEventsRecent_DupStormFallsBackToInCHDedup: when the raw fetch returns the full
// over-fetch and dedup still cannot fill the page, a short page would falsely read as "last page"
// (the handler keys next_cursor on a full page), so the reader must issue the LIMIT 1 BY shape.
func TestContractEventsRecent_DupStormFallsBackToInCHDedup(t *testing.T) {
	const limit = 2
	fetch := limit + contractEventsDedupHeadroom
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			return &stubRows{}, nil // index unavailable → legacy path
		}
		if strings.Contains(q, "LIMIT 1 BY") {
			// Fallback path: in-CH dedup returns a clean full page.
			return &stubRows{data: [][]any{
				contractEventRecentRowFor(100, 0, 0),
				contractEventRecentRowFor(99, 0, 0),
			}}, nil
		}
		// Fast path: every fetched row is the SAME identity — dedup
		// collapses to 1 < limit while raw == fetch.
		data := make([][]any, fetch)
		for i := range data {
			data[i] = contractEventRecentRowFor(100, 0, 0)
		}
		return &stubRows{data: data}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.ContractEventsRecent(context.Background(), "CTESTCONTRACT", limit, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(conn.queries) != 3 {
		t.Fatalf("issued %d queries, want 3 (index probe + fast path + dedup fallback)", len(conn.queries))
	}
	if !strings.Contains(conn.queries[2], "LIMIT 1 BY ledger_seq, tx_hash, op_index, event_index LIMIT ?") {
		t.Fatalf("fallback query = %q, want the in-CH LIMIT 1 BY dedup shape", conn.queries[2])
	}
	if len(rows) != limit {
		t.Fatalf("rows = %d, want %d from the fallback", len(rows), limit)
	}
}

func TestContractEventsRecent_CursorShapes(t *testing.T) {
	// Both shapes must keyset-page by the FULL row-identity tuple, with
	// and without the active-ledger bound.
	for name, q := range map[string]string{
		"fast":          contractEventsRecentQuery(true, false),
		"dedup":         contractEventsRecentDedupQuery(true, false),
		"fast+ledgers":  contractEventsRecentQuery(true, true),
		"dedup+ledgers": contractEventsRecentDedupQuery(true, true),
	} {
		if !strings.Contains(q, "(ledger_seq, tx_hash, op_index, event_index) < (?, ?, ?, ?)") {
			t.Fatalf("%s cursor query = %q, lost its keyset predicate", name, q)
		}
		if strings.Contains(name, "ledgers") && !strings.Contains(q, "ledger_seq IN (?)") {
			t.Fatalf("%s query = %q, lost its active-ledger bound", name, q)
		}
	}
}

// TestContractEventsRecent_ActiveLedgerBound: with the contract_active_ledgers index present, a non-empty
// walk of the contract's active ledgers bounds the events read; an empty walk is not authoritative (see
// ...EmptyWalkFallsThrough).
func TestContractEventsRecent_ActiveLedgerBound(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{data: [][]any{{uint32(100)}, {uint32(99)}}}, nil
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil // probe: non-empty
		}
		if !strings.Contains(q, "ledger_seq IN (?)") {
			t.Fatalf("events query lost the ledger bound: %s", q)
		}
		return &stubRows{data: [][]any{
			contractEventRecentRowFor(100, 0, 0),
			contractEventRecentRowFor(99, 0, 0),
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	// limit 2 == rows served: the bounded page is full, so it is trusted.
	rows, err := r.ContractEventsRecent(context.Background(), "CTESTCONTRACT", 2, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if len(conn.queries) != 3 {
		t.Fatalf("issued %d queries, want 3 (probe + ledgers walk + bounded events)", len(conn.queries))
	}
}

// TestContractEventsRecent_PartialWalkShortPageFallsThrough pins that a short bounded page is re-read
// unbounded: a non-empty walk can be truncated by a partial backfill, and older events must still be served.
func TestContractEventsRecent_PartialWalkShortPageFallsThrough(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{data: [][]any{{uint32(100)}}}, nil
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		}
		if strings.Contains(q, "ledger_seq IN (?)") {
			return &stubRows{data: [][]any{contractEventRecentRowFor(100, 0, 0)}}, nil
		}
		return &stubRows{data: [][]any{
			contractEventRecentRowFor(100, 0, 0),
			contractEventRecentRowFor(50, 0, 0),
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.ContractEventsRecent(context.Background(), "CTESTCONTRACT", 2, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 — the older event the partial walk omitted", len(rows))
	}
}

// TestContractEventsRecent_EmptyWalkFallsThrough: the contract_active_ledgers probe cannot see partial
// backfill coverage, so an empty walk must not short-circuit to "no events"; it falls through to the
// unbounded scan.
func TestContractEventsRecent_EmptyWalkFallsThrough(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{}, nil // partial backfill: empty per-contract walk
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil // probe: globally non-empty
		}
		// The events table DOES hold this contract's events.
		if strings.Contains(q, "ledger_seq IN (?)") {
			t.Fatalf("empty walk must NOT bound the scan by (unknown) ledgers: %s", q)
		}
		return &stubRows{data: [][]any{
			contractEventRecentRowFor(5_000_000, 0, 0),
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.ContractEventsRecent(context.Background(), "CQUIETCONTRACT", 100, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent(empty walk): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 — the real event served via the unbounded fallthrough, "+
			"not a confidently-wrong empty page", len(rows))
	}
	// Probe + empty walk + watermark + unbounded events scan = 4 queries; the events
	// query must have run (no short-circuit to nil,nil).
	if len(conn.queries) != 4 {
		t.Fatalf("issued %d queries, want 4 (probe + walk + watermark + unbounded events scan)", len(conn.queries))
	}
	last := conn.queries[len(conn.queries)-1]
	if !strings.Contains(last, "FROM stellar.contract_events") || strings.Contains(last, "ledger_seq IN") {
		t.Fatalf("final query = %q, want the UNBOUNDED contract_events scan", last)
	}
}

// completeLedgersIndexConn answers the probe, a walk of walk, and a recorded genesis watermark; any events query must carry the ledger bound.
func completeLedgersIndexConn(t *testing.T, walk [][]any, events [][]any) *stubConn {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "entry_history_watermark"):
			return &stubRows{data: [][]any{{uint32(900)}}}, nil
		case strings.Contains(q, "contract_active_ledgers") && strings.Contains(q, "WHERE contract_id"):
			return &stubRows{data: walk}, nil
		case strings.Contains(q, "contract_active_ledgers"):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case !strings.Contains(q, "ledger_seq IN (?)"):
			t.Fatalf("a proven-complete index must not fall back to the unbounded scan: %s", q)
		}
		return &stubRows{data: events}, nil
	}
	return conn
}

// TestContractEventsRecent_CompleteIndexEmptyWalkIsEmpty: with the genesis watermark an empty walk is exact, so no events scan runs.
func TestContractEventsRecent_CompleteIndexEmptyWalkIsEmpty(t *testing.T) {
	conn := completeLedgersIndexConn(t, nil, nil)
	rows, err := (&ExplorerReader{conn: conn}).ContractEventsRecent(context.Background(), "CQUIETCONTRACT", 100, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(rows) != 0 || len(conn.queries) != 3 {
		t.Fatalf("rows = %d after %d queries, want 0 after 3 (probe + walk + watermark)", len(rows), len(conn.queries))
	}
}

// TestContractEventsRecent_CompleteIndexTrustsShortPage: with the genesis watermark a short bounded page is the whole history.
func TestContractEventsRecent_CompleteIndexTrustsShortPage(t *testing.T) {
	conn := completeLedgersIndexConn(t, [][]any{{uint32(100)}, {uint32(40)}},
		[][]any{contractEventRecentRowFor(100, 0, 0), contractEventRecentRowFor(40, 1, 0)})
	rows, err := (&ExplorerReader{conn: conn}).ContractEventsRecent(context.Background(), "CQUIETCONTRACT", 100, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(rows) != 2 || len(conn.queries) != 4 {
		t.Fatalf("rows = %d after %d queries, want 2 after 4 (probe + walk + bounded events + watermark)", len(rows), len(conn.queries))
	}
}

func TestEventsByTx_UsesFinal(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		return &stubRows{data: [][]any{{
			uint32(0), uint32(0), "CTESTCONTRACT", "contract", "transfer",
		}}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.EventsByTx(context.Background(), 100, "abchash")
	if err != nil {
		t.Fatalf("EventsByTx: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	q := conn.queries[len(conn.queries)-1]
	// ledger+tx_hash-scoped so FINAL stays cheap (partition +
	// primary-key-prefix bounded), matching the byte-twin OperationsByTx.
	if !strings.Contains(q, "FROM stellar.contract_events FINAL") {
		t.Fatalf("query = %q, want `FROM stellar.contract_events FINAL`", q)
	}
}

// TestSACAssetFromEvents_ActiveLedgerBound: the /wasm 404 path probes SAC-ness for every contract without a
// captured instance. An unbounded `ORDER BY ledger_seq DESC LIMIT 1` reverse-scans the key range and times
// out, so with the index present the read must be bounded to the contract's recent active ledgers.
func TestSACAssetFromEvents_ActiveLedgerBound(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{data: [][]any{{uint32(100)}, {uint32(99)}}}, nil
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil // probe: index present
		}
		if !strings.Contains(q, "ledger_seq IN (?)") {
			t.Errorf("SAC events probe lost its ledger bound: %s", q)
		}
		return &stubRows{data: [][]any{}}, nil
	}
	r := &ExplorerReader{conn: conn}

	if _, _, err := r.SACAssetFromEvents(context.Background(), "CTESTCONTRACT"); err != nil {
		t.Fatalf("SACAssetFromEvents: %v", err)
	}
}

// TestSACAssetFromEvents_NoActiveLedgersSkipsScan: a contract the index knows has NO active
// ledgers has no events to inspect, so the answer is authoritative and the events table must not
// be touched; an unbounded fallback would cost most on the cheapest contracts to answer.
func TestSACAssetFromEvents_NoActiveLedgersSkipsScan(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{data: [][]any{}}, nil // no active ledgers
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil // probe: index present
		}
		t.Errorf("queried contract_events despite an empty active-ledger walk: %s", q)
		return &stubRows{data: [][]any{}}, nil
	}
	r := &ExplorerReader{conn: conn}

	name, found, err := r.SACAssetFromEvents(context.Background(), "CTESTCONTRACT")
	if err != nil {
		t.Fatalf("SACAssetFromEvents: %v", err)
	}
	if found || name != "" {
		t.Fatalf("got (%q, %v), want the empty authoritative answer", name, found)
	}
}

// TestContractInteractions_BusyContractWindowNarrows: the read scales with ledger span, so a busy contract
// is slow over the default window. The window is anchored to the contract's recent activity, and the
// effective floor is returned so since_ledger describes what was served, not what was asked for.
func TestContractInteractions_BusyContractWindowNarrows(t *testing.T) {
	const window = 500
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				// Exactly the cap = busy: newest first, oldest last.
				data := make([][]any, window)
				for i := range data {
					data[i] = []any{uint32(9000 - i)}
				}
				return &stubRows{data: data}, nil
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		}
		return &stubRows{data: [][]any{}}, nil
	}
	r := &ExplorerReader{conn: conn}

	_, effective, err := r.ContractInteractions(context.Background(), "CTESTCONTRACT", 50, 100)
	if err != nil {
		t.Fatalf("ContractInteractions: %v", err)
	}
	if want := uint32(9000 - (window - 1)); effective != want {
		t.Fatalf("effective floor = %d, want %d (the oldest of the contract's most recent %d active ledgers)",
			effective, want, window)
	}
	if effective <= 100 {
		t.Fatal("effective floor did not narrow the requested window")
	}
}

// TestContractInteractions_QuietContractKeepsFullWindow: most contracts have fewer active ledgers than the
// cap, so narrowing their window shrinks the answer for no gain, and the floor never widens the request.
func TestContractInteractions_QuietContractKeepsFullWindow(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{data: [][]any{{uint32(9000)}, {uint32(8000)}}}, nil
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		}
		return &stubRows{data: [][]any{}}, nil
	}
	r := &ExplorerReader{conn: conn}

	_, effective, err := r.ContractInteractions(context.Background(), "CTESTCONTRACT", 50, 100)
	if err != nil {
		t.Fatalf("ContractInteractions: %v", err)
	}
	if effective != 100 {
		t.Fatalf("effective floor = %d, want the requested 100 kept intact for a quiet contract", effective)
	}
}

// TestContractInteractions_CapsDistinctTxsNotEventRows: the inner (ledger_seq, tx_hash) cap subquery must be
// SELECT DISTINCT, or the LIMIT is consumed by event rows and a multi-event contract undercounts shared_txs.
func TestContractInteractions_CapsDistinctTxsNotEventRows(t *testing.T) {
	if !strings.Contains(contractInteractionsQuery,
		"SELECT DISTINCT ledger_seq, tx_hash FROM stellar.contract_events") {
		t.Fatalf("contractInteractionsQuery inner subquery must SELECT DISTINCT (ledger_seq, tx_hash) "+
			"so the subjectTxCap caps transactions, not event rows; got:\n%s", contractInteractionsQuery)
	}

	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "contract_active_ledgers"):
			if strings.Contains(q, "SELECT ledger_seq FROM stellar.contract_active_ledgers WHERE") { // per-contract walk
				return &stubRows{}, nil // quiet: keep the full requested window
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil // probe present
		default:
			// Outer interactions scan: the DISTINCT-capped IN set must be present.
			if !strings.Contains(q, "SELECT DISTINCT ledger_seq, tx_hash") {
				t.Fatalf("interactions query lost its DISTINCT tx cap: %s", q)
			}
			return &stubRows{data: [][]any{{"CPEER", int64(4)}}}, nil
		}
	}
	r := &ExplorerReader{conn: conn}

	edges, _, err := r.ContractInteractions(context.Background(), "CTESTCONTRACT", 50, 100)
	if err != nil {
		t.Fatalf("ContractInteractions: %v", err)
	}
	if len(edges) != 1 || edges[0].SharedTxs != 4 {
		t.Fatalf("edges = %+v, want one peer with 4 shared txs", edges)
	}
}

func TestHashChainWindowResult_Checked(t *testing.T) {
	cases := []struct {
		name    string
		present uint64
		want    uint64
	}{
		{"typical-window", 1_000_000, 999_999},
		{"single-ledger", 1, 0},
		// Present==0 is a wholly missing window, where Present-1 would wrap a uint64 subtraction to ~1.8e19.
		// Checked() MUST saturate to 0 here.
		{"empty-window", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := HashChainWindowResult{Present: tc.present}
			if got := w.Checked(); got != tc.want {
				t.Fatalf("Checked() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestContractActiveLedgers_CollapsesDuplicatesAcrossPages: un-merged RMT rows repeat a ledger, so a page can come back
// short of n distinct ledgers; the walk must collapse them and continue strictly below the page's oldest row.
func TestContractActiveLedgers_CollapsesDuplicatesAcrossPages(t *testing.T) {
	lake := []uint32{50, 50, 50, 50, 50, 50, 50, 50, 50, 49, 49, 48, 47, 46, 45}
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "DISTINCT") {
			t.Fatalf("DISTINCT defeats the reverse key walk: %s", q)
		}
		args := conn.args[len(conn.args)-1]
		before, limit := uint32(0), args[len(args)-1].(int)
		if len(args) == 3 {
			before = args[1].(uint32)
		}
		var data [][]any
		for _, l := range lake {
			if (before == 0 || l <= before) && len(data) < limit {
				data = append(data, []any{l})
			}
		}
		return &stubRows{data: data}, nil
	}
	r := &ExplorerReader{conn: conn}

	got, err := r.contractActiveLedgers(context.Background(), "CTEST", 0, 2)
	if err != nil {
		t.Fatalf("contractActiveLedgers: %v", err)
	}
	if want := []uint32{50, 49}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(conn.queries) < 2 {
		t.Fatalf("queries = %d, want a second page after duplicates filled the first", len(conn.queries))
	}

	conn.queries, conn.args = nil, nil
	got, err = r.contractActiveLedgers(context.Background(), "CTEST", 46, 10)
	if err != nil {
		t.Fatalf("contractActiveLedgers before=46: %v", err)
	}
	if want := []uint32{46, 45}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v (exhausted lake, inclusive cursor)", got, want)
	}
}
