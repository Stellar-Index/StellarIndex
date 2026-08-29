//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// legacyAccountOperationsSQL is the AccountOperations page query EXACTLY as
// it stood before the 2026-08-28 rewrite (origin/main 6762d0a1,
// accountOperationsQuery(hasCursor, hasBound=true) with opCols expanded),
// frozen here as the differential oracle: the rewrite must serve
// byte-identical pages — same rows, order, cursor semantics and
// source/participant dedupe — while no longer reading one
// stellar.operations granule per key the account ever touched.
func legacyAccountOperationsSQL(hasCursor bool) string {
	cursorClause := ""
	if hasCursor {
		cursorClause = ` AND (ledger_seq, tx_index, op_index) < (?, ?, ?)`
	}
	boundClause := ` AND ledger_seq <= ?`
	return `SELECT ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr FROM stellar.operations
		WHERE (ledger_seq, tx_index, op_index) IN (
		  SELECT ledger_seq, tx_index, op_index FROM (
		    (SELECT ledger_seq, tx_index, op_index FROM stellar.operations
		       WHERE (ledger_seq, tx_index, op_index) IN (
		            SELECT ledger_seq, tx_index, op_index FROM stellar.ops_by_source
		            WHERE source_account = ? AND op_index != 4294967295)` + boundClause + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		       LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?)
		    UNION ALL
		    (SELECT ledger_seq, tx_index, op_index FROM stellar.operations
		       WHERE (ledger_seq, tx_index, op_index) IN (
		            SELECT ledger_seq, tx_index, op_index FROM stellar.operation_participants WHERE account = ?)` + boundClause + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		       LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?)
		  ) ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT ?)
		ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?
		SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_bytes_before_external_group_by = 4000000000, max_bytes_before_external_sort = 4000000000`
}

// legacyAccountTransactionsSQL is AccountTransactions' page query as it stood
// before the same rewrite (origin/main 6762d0a1, accountTransactionsQuery with
// txCols expanded) — the differential oracle for the transactions walk.
func legacyAccountTransactionsSQL(hasCursor bool) string {
	cursorClause := ""
	if hasCursor {
		cursorClause = ` AND (ledger_seq, tx_index) < (?, ?)`
	}
	return `SELECT ledger_seq, close_time, tx_hash, tx_index, source_account,
	fee_charged, max_fee, operation_count, successful, result_code, memo_type, memo FROM stellar.transactions
		WHERE (ledger_seq, tx_index) IN (
		  SELECT ledger_seq, tx_index FROM (
		    (SELECT ledger_seq, tx_index FROM stellar.transactions
		       WHERE (ledger_seq, tx_index) IN (
		            SELECT DISTINCT ledger_seq, tx_index FROM stellar.ops_by_source WHERE source_account = ?)` + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?)
		    UNION ALL
		    (SELECT ledger_seq, tx_index FROM stellar.transactions
		       WHERE (ledger_seq, tx_index) IN (
		            SELECT DISTINCT ledger_seq, tx_index FROM stellar.operation_participants WHERE account = ?)` + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?)
		  ) ORDER BY ledger_seq DESC, tx_index DESC LIMIT ?)
		ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?
		SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_bytes_before_external_group_by = 4000000000, max_bytes_before_external_sort = 4000000000`
}

// TestClickHouseAccountOperationsPageBoundedByPageSize is the live-ClickHouse
// proof for the 2026-08-28 AccountOperations rewrite (r1: `explorer
// AccountOperations deadline exceeded` 503s, 14×/24h, for an account with
// 11,925 sourced + 26,064 participant ops).
//
// Pathology: the arms resolved their keys OVER stellar.operations with
// `pk IN (SELECT pk FROM ops_by_source WHERE source_account = ?)`.
// ClickHouse's set-based index analysis prunes that to exact granules — one
// granule PER KEY IN THE SET, before the LIMIT — so a page cost the account's
// whole history in granules (live query_log: 164–238 M rows, ~8 s, page of
// 50). The fix pages ops_by_source / operation_participants directly
// (primary-key-prefix range reads) and hydrates ≤ 3×limit keys.
//
// Fixture: 2 M ledgers × one tx × one op, in ONE ingest so they land in the
// same part and each 8192-row granule is distinct. The hot account:
//   - sources every 10,000th op AND its tx (200 keys, one per granule);
//   - is a non-source participant on a different 200 ops (offset 5,000),
//     whose tx is sourced by the filler account;
//   - sources a further 200 TXs (offset 2,500) whose only op is sourced by
//     the filler account and has hot as a participant — the tx appears in
//     BOTH transaction arms (legitimate per accountTransactionsQuery's
//     "DISTINCT dedups the rare tx that is both sourced and participated"),
//     while on the OPERATIONS side it is participant-only, honouring the
//     op-level XOR invariant accountOperationsQuery relies on (participants
//     exclude the op's own source; that listing's merge would serve a
//     SHORT page if the invariant were violated — see the "no cross-arm
//     dedupe needed" note there). On the TRANSACTIONS side this overlap
//     is what #290 was: the key was emitted by both arms and ate two of
//     the merge's LIMIT slots.
//
// A re-ingested duplicate part covers the ReplacingMergeTree dedupe on
// both listings. The test then:
//
//  1. DIFFERENTIAL: walks the ENTIRE history of BOTH listings (page size
//     7, ~58 / ~86 pages) through the reader and through the frozen
//     pre-fix SQL with the same args, plus a cursor set MID-page. The
//     OPERATIONS walk asserts every page identical — rows, order, cursor
//     continuation, dedupe. The TRANSACTIONS walk asserts the same
//     SEQUENCE of rows in the same order, but not the same page
//     boundaries: since #290 the reader dedupes the two overlapping arms
//     at the keyset merge, so its pages are full where the frozen
//     pre-fix shape still hands back a SHORT one (the walk requires the
//     legacy side to still produce one, else the reader-side fullness
//     assertion would be vacuous). Also pins the absolute expectations
//     (600 distinct ops / 600 distinct txs, strictly descending, no key
//     served twice, and no non-final short page on EITHER listing from
//     the reader — the handler withholds next_cursor on a short page, so
//     one truncates the client's history walk).
//  2. READ-ROWS: one page of 50 via each path, `system.query_log`
//     read_rows. The pre-fix shape reads ≥ one granule per key (≥ 200
//     granules ≈ 1.6 M rows here; 38 k granules live); the new shape reads
//     ≤ 3×limit granules. Red-proof: with the old query text in the reader
//     the reader's read_rows equal the legacy figure and the 4× bound
//     fails.
func TestClickHouseAccountOperationsPageBoundedByPageSize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		hot  = "GTEST_PKP_HOT_ACCOUNT_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		fill = "GTEST_PKP_FILLER_ACCOUNT_AAAAAAAAAAAAAAAAAAAAAAAAA"
		base = uint32(9_500_001) // partitions 9..11; nowhere near other tests' ledgers
		n    = 2_000_000
		// Sourced every `stride` ledgers (one key per 8192-row granule);
		// participant on the same stride at `partOff`; overlap every
		// `overlapStride` (a sourced op HOT also participates in).
		stride  = 10_000
		partOff = 5_000
		txOff   = 2_500
	)
	// close_time: ms offsets keep the whole fixture inside 2026-05-01 so
	// it never becomes the global close_time tip another test anchors on
	// (see account_activity_watermark_test.go).
	closeExpr := `toDateTime('2026-05-01 00:00:00', 'UTC') + toIntervalMillisecond(number)`
	seqExpr := fmt.Sprintf(`toUInt32(%d + number)`, base)
	hashExpr := `lpad(toString(` + seqExpr + `), 64, '0')`

	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := raw.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %.80q: %v", q, err)
		}
	}
	hotOp := fmt.Sprintf(`number %% %d = 0`, stride)
	hotPart := fmt.Sprintf(`(number %% %d = %d OR number %% %d = %d)`, stride, partOff, stride, txOff)
	hotTx := fmt.Sprintf(`(number %% %d = 0 OR number %% %d = %d)`, stride, stride, txOff)
	insertOps := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.operations
			(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)
			SELECT %s, %s, %s, 0, 0, 'OperationTypePayment', if(%s, '%s', '%s'), 'Ym9keQ=='
			FROM numbers(%d) WHERE %s`, seqExpr, closeExpr, hashExpr, hotOp, hot, fill, n, where))
	}
	insertTxs := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.transactions
			(ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee, operation_count, successful, result_code, memo_type, memo)
			SELECT %s, %s, %s, 0, if(%s, '%s', '%s'), 100, 200, 1, 1, 0, 'none', ''
			FROM numbers(%d) WHERE %s`, seqExpr, closeExpr, hashExpr, hotTx, hot, fill, n, where))
	}
	insertParts := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.operation_participants
			(account, ledger_seq, close_time, tx_hash, tx_index, op_index)
			SELECT '%s', %s, %s, %s, 0, 0 FROM numbers(%d) WHERE %s AND %s`,
			hot, seqExpr, closeExpr, hashExpr, n, hotPart, where))
	}
	insertOps(`1`)
	insertTxs(`1`)
	insertParts(`1`)
	// Re-ingested duplicate PARTS near the bottom of the range (MVs
	// re-fire, so ops_by_source and account_activity get duplicates too).
	dup := fmt.Sprintf(`number < %d`, 3*stride)
	insertOps(dup)
	insertTxs(dup)
	insertParts(dup)

	var wm uint32
	if err := raw.QueryRow(ctx,
		`SELECT max(last_ledger) FROM stellar.account_activity WHERE account_id = ?`, hot).Scan(&wm); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if wm == 0 {
		t.Fatal("fixture: account_activity watermark missing — the reader would take the unbounded path and the legacy oracle (bounded) would not be the same query")
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	legacyPage := func(qctx context.Context, limit int, cur chstore.ExplorerCursor) []chstore.OpRow {
		t.Helper()
		args := []any{hot, wm}
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A, cur.B)
		}
		args = append(args, limit, hot, wm)
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A, cur.B)
		}
		args = append(args, limit, limit, limit)
		rows, err := raw.Query(qctx, legacyAccountOperationsSQL(cur.IsSet()), args...)
		if err != nil {
			t.Fatalf("legacy page: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []chstore.OpRow
		for rows.Next() {
			var r chstore.OpRow
			if err := rows.Scan(&r.Seq, &r.CloseTime, &r.TxHash, &r.TxIndex, &r.OpIndex, &r.OpType, &r.SourceAccount, &r.BodyXDR); err != nil {
				t.Fatalf("legacy scan: %v", err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("legacy rows: %v", err)
		}
		return out
	}

	legacyTxPage := func(qctx context.Context, limit int, cur chstore.ExplorerCursor) []chstore.TxSummary {
		t.Helper()
		args := []any{hot}
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A)
		}
		args = append(args, limit, hot)
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A)
		}
		args = append(args, limit, limit, limit)
		rows, err := raw.Query(qctx, legacyAccountTransactionsSQL(cur.IsSet()), args...)
		if err != nil {
			t.Fatalf("legacy tx page: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []chstore.TxSummary
		for rows.Next() {
			var r chstore.TxSummary
			var ok uint8
			if err := rows.Scan(&r.Seq, &r.CloseTime, &r.TxHash, &r.TxIndex, &r.SourceAccount,
				&r.FeeCharged, &r.MaxFee, &r.OperationCount, &ok, &r.ResultCode, &r.MemoType, &r.Memo); err != nil {
				t.Fatalf("legacy tx scan: %v", err)
			}
			r.Successful = ok != 0
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("legacy tx rows: %v", err)
		}
		return out
	}

	// (1) Full-history differential walk, page size 7.
	const pageSize = 7
	type key [3]uint32
	older := func(k, prev key) bool {
		return k[0] < prev[0] || (k[0] == prev[0] && (k[1] < prev[1] || (k[1] == prev[1] && k[2] < prev[2])))
	}
	// walk pages `name` through reader vs legacy until the reader serves
	// an empty page, asserting page-for-page equality; returns the
	// distinct keys served. Every page is `pageSize` long but the last:
	// a non-final short page is exactly what makes a client stop early
	// (the handler withholds next_cursor on it — see the transactions
	// walk below and #290).
	walk := func(name string, page func(cur chstore.ExplorerCursor) (got, want []key)) map[key]bool {
		t.Helper()
		var (
			cur   chstore.ExplorerCursor
			seen  = map[key]bool{}
			pages int
			last  key
			short int // pages shorter than pageSize; only the final one may be
		)
		for {
			got, want := page(cur)
			if len(got) > 0 {
				if short > 0 {
					t.Fatalf("%s page %d (cursor %+v) follows a SHORT page — a non-final short page means a client stopping on it truncates history", name, pages, cur)
				}
				if len(got) < pageSize {
					short++
				}
			}
			if len(got) != len(want) {
				t.Fatalf("%s page %d (cursor %+v): reader %d rows, legacy %d rows", name, pages, cur, len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s page %d row %d differs (cursor %+v): reader %v legacy %v", name, pages, i, cur, got[i], want[i])
				}
				if seen[got[i]] {
					t.Fatalf("%s page %d row %d: key %v served twice (dedupe / cursor regression)", name, pages, i, got[i])
				}
				if (pages > 0 || i > 0) && !older(got[i], last) {
					t.Fatalf("%s page %d row %d: key %v not strictly older than previous %v", name, pages, i, got[i], last)
				}
				seen[got[i]] = true
				last = got[i]
			}
			if len(got) == 0 {
				break
			}
			pages++
			cur = chstore.ExplorerCursor{Ledger: last[0], A: last[1], B: last[2]}
		}
		t.Logf("%s: %d distinct keys over %d pages", name, len(seen), pages)
		return seen
	}

	opKeys := func(rows []chstore.OpRow) []key {
		out := make([]key, len(rows))
		for i, r := range rows {
			out[i] = key{r.Seq, r.TxIndex, r.OpIndex}
		}
		return out
	}
	opsSeen := walk("operations", func(cur chstore.ExplorerCursor) ([]key, []key) {
		got, err := er.AccountOperations(ctx, hot, pageSize, cur)
		if err != nil {
			t.Fatalf("AccountOperations (cursor %+v): %v", cur, err)
		}
		want := legacyPage(ctx, pageSize, cur)
		// Full-row equality (close_time, hash, type, source, body), not
		// just keys — the hydration must serve the same wide row.
		for i := range got {
			if i < len(want) && got[i] != want[i] {
				t.Fatalf("operations row differs (cursor %+v):\n reader %+v\n legacy %+v", cur, got[i], want[i])
			}
		}
		return opKeys(got), opKeys(want)
	})
	if want := 3 * (n / stride); len(opsSeen) != want {
		t.Fatalf("operations: %d distinct ops, want %d (sourced %d + participant %d + participant-on-hot-sourced-tx %d)",
			len(opsSeen), want, n/stride, n/stride, n/stride)
	}
	// The tx listing's two arms legitimately overlap (the txOff txs, sourced
	// by hot AND carrying it as a non-source participant). Until #290 an
	// overlap key occupied TWO of the keyset merge's LIMIT slots — the merge
	// took its LIMIT before anything deduped — so the page came back SHORT
	// while older history remained, and the handler emits next_cursor only on
	// a FULL page (internal/api/v1/explorer/accounts.go): a client walking
	// the history stopped there, silently truncated. The reader now dedupes
	// at the merge; the frozen legacy shape still serves those short pages,
	// so the differential here is over the WHOLE walk (same rows, same order,
	// none gained, lost, reordered or repeated) rather than page-for-page,
	// and the reader's pages must additionally be full except the last.
	txWalk := func(name string, page func(cur chstore.ExplorerCursor) []chstore.TxSummary) (rows []chstore.TxSummary, nonFinalShort int) {
		t.Helper()
		var (
			cur       chstore.ExplorerCursor
			seen      = map[key]bool{}
			pages     int
			last      key
			prevShort bool
		)
		for {
			got := page(cur)
			if len(got) > 0 && prevShort {
				nonFinalShort++
			}
			if len(got) == 0 {
				break
			}
			prevShort = len(got) < pageSize
			for i, r := range got {
				k := key{r.Seq, r.TxIndex, 0}
				if seen[k] {
					t.Fatalf("%s page %d row %d: tx %v served twice (dedupe / cursor regression)", name, pages, i, k)
				}
				if (pages > 0 || i > 0) && !older(k, last) {
					t.Fatalf("%s page %d row %d: tx %v not strictly older than previous %v", name, pages, i, k, last)
				}
				seen[k] = true
				last = k
				rows = append(rows, r)
			}
			pages++
			cur = chstore.ExplorerCursor{Ledger: last[0], A: last[1]}
		}
		t.Logf("%s: %d txs over %d pages (%d non-final short pages)", name, len(rows), pages, nonFinalShort)
		return rows, nonFinalShort
	}

	readerTxs, readerShort := txWalk("transactions (reader)", func(cur chstore.ExplorerCursor) []chstore.TxSummary {
		got, err := er.AccountTransactions(ctx, hot, pageSize, cur)
		if err != nil {
			t.Fatalf("AccountTransactions (cursor %+v): %v", cur, err)
		}
		return got
	})
	legacyTxs, legacyShort := txWalk("transactions (legacy)", func(cur chstore.ExplorerCursor) []chstore.TxSummary {
		return legacyTxPage(ctx, pageSize, cur)
	})
	if readerShort != 0 {
		t.Fatalf("transactions: %d non-final SHORT pages from the reader — the handler withholds next_cursor on a short page, so a client stops there with older history unreached (#290)", readerShort)
	}
	if legacyShort == 0 {
		t.Fatal("fixture no longer reproduces #290: the pre-fix query text served no non-final short page, so the fullness assertion above is vacuous — the arms must still overlap (the txOff txs)")
	}
	if len(readerTxs) != len(legacyTxs) {
		t.Fatalf("transactions: reader walked %d txs, legacy %d — the merge dedupe must change page BOUNDARIES, never the rows served", len(readerTxs), len(legacyTxs))
	}
	for i := range readerTxs {
		if readerTxs[i] != legacyTxs[i] {
			t.Fatalf("transactions row %d differs:\n reader %+v\n legacy %+v", i, readerTxs[i], legacyTxs[i])
		}
	}
	if want := 3 * (n / stride); len(readerTxs) != want {
		t.Fatalf("transactions: %d distinct txs, want %d (sourced-with-op %d + participant-only %d + sourced-tx-and-participant %d)",
			len(readerTxs), want, n/stride, n/stride, n/stride)
	}

	// A cursor set MID-page (not at a page boundary): the next page must
	// start exactly at the following row, identically on both paths.
	first, err := er.AccountOperations(ctx, hot, 10, chstore.ExplorerCursor{})
	if err != nil || len(first) != 10 {
		t.Fatalf("first page: %v (%d rows)", err, len(first))
	}
	mid := chstore.ExplorerCursor{Ledger: first[3].Seq, A: first[3].TxIndex, B: first[3].OpIndex}
	fromMid, err := er.AccountOperations(ctx, hot, 10, mid)
	if err != nil {
		t.Fatalf("mid-page cursor: %v", err)
	}
	legacyMid := legacyPage(ctx, 10, mid)
	if len(fromMid) != 10 || fromMid[0] != first[4] || len(legacyMid) != 10 || legacyMid[0] != first[4] {
		t.Fatalf("mid-page cursor %+v: reader first row %+v, legacy first row %+v, want %+v",
			mid, fromMid[0], legacyMid[0], first[4])
	}

	// (2) read_rows: one page of 50 via each path.
	const limit = 50
	t0 := time.Now().Add(-time.Second)
	legacyID := uuid.NewString()
	_ = legacyPage(clickhouse.Context(ctx, clickhouse.WithQueryID(legacyID)), limit, chstore.ExplorerCursor{})
	if _, err := er.AccountOperations(ctx, hot, limit, chstore.ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations read_rows page: %v", err)
	}
	mustExec(`SYSTEM FLUSH LOGS`)
	readRows := func(where string, args ...any) uint64 {
		t.Helper()
		var rr uint64
		if err := raw.QueryRow(ctx, `SELECT read_rows FROM system.query_log
			WHERE type = 'QueryFinish' AND event_time >= ? AND `+where+`
			ORDER BY event_time_microseconds DESC LIMIT 1`, append([]any{t0}, args...)...).Scan(&rr); err != nil {
			t.Fatalf("query_log (%s): %v", where, err)
		}
		return rr
	}
	legacyRead := readRows(`query_id = ?`, legacyID)
	// The reader's page query is the newest finished query that hydrates
	// body_xdr for the hot account (the watermark probe carries neither).
	readerRead := readRows(`query_id != ? AND query LIKE '%body_xdr%' AND query LIKE ? AND query NOT LIKE '%system.query_log%'`,
		legacyID, "%"+hot+"%")
	t.Logf("read_rows: legacy=%d reader=%d (fixture: %d hot op keys, one per 8192-row granule; page %d)",
		legacyRead, readerRead, len(opsSeen), limit)
	if legacyRead < uint64(n/stride)*4096 {
		t.Fatalf("fixture no longer reproduces the pathology: legacy read %d rows, expected ≥ one granule per hot key (~%d) — the differential still holds but the read_rows bound below would be vacuous",
			legacyRead, (n/stride)*8192)
	}
	if readerRead*4 > legacyRead {
		t.Fatalf("reader read %d rows vs legacy %d — the page must be bounded by the page size (≤ 3×limit granules), not by the account's history",
			readerRead, legacyRead)
	}
}
