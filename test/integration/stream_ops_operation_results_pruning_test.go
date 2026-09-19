//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// This file is the live-ClickHouse cost proof for the op-stream lake readers
// (StreamSDEXOps, StreamClassicOps). It pins TWO properties that a text grep
// for "CreatingSet" or "Join" cannot tell apart, because three different query
// shapes satisfy one each:
//
//	IN-subquery   (F111/T385, the defect): operation_results IS pruned, but the
//	              window's whole successful-tx set is materialised first —
//	              CreatingSetsTransform, the 10 GiB blowout of 2026-07-11.
//	derived-outer (the rejected first fix): no set-build, but the outer ledger
//	              window is hoisted into a derived table, so ClickHouse cannot
//	              propagate it through o.ledger_seq = r.ledger_seq and
//	              stellar.operation_results full-scans as grace_hash's spilled
//	              build side. One unbounded read traded for another.
//	shipped       (contractCallOpsQuery's shape): the successful-tx join spelled
//	              BEFORE the outer WHERE — no set-build AND pruned.
//
// Both rejected shapes are frozen below as oracles and both are asserted to
// still exhibit their pathology against this fixture, so neither assertion can
// pass vacuously.

const (
	// A ledger range of this test's own (partitions 30..31), clear of every
	// other ClickHouse integration fixture's.
	opsPruneBase = uint32(30_500_001)
	// One op / result / tx per ledger. ~123 granules of operation_results, so
	// the 1,000-ledger read window is one granule when pruning works and all
	// of them when it does not.
	opsPruneRows = 1_000_000
	// The window every query under test reads.
	opsPruneWindow = uint32(1_000)
	// Every opsPruneFailMod'th tx is a FAILED tx: the successful-tx filter has
	// to actually exclude rows, or the differential proves nothing.
	opsPruneFailMod = 7
	// A re-ingested duplicate PART of stellar.transactions covering the read
	// window. These readers take no FINAL, so it puts the same tx_hash on the
	// join's build side twice — what `GROUP BY tx_hash` in the derived table
	// exists to absorb.
	opsPruneDupRows = 5_000

	opsPruneSDEXType    = "OperationTypeManageSellOffer"
	opsPruneClassicType = "OperationTypePayment"
)

// ── the shapes this test exists to rule out ─────────────────────────────────

// inSubqueryOpsSQL is the successful-tx filter EXACTLY as sdexOpsQuery and
// classicOpsQuery carried it before this fix (F111 / T385) — an IN-subquery,
// whose CreatingSet step materialises the whole window's tx-hash set before
// the join runs. The third sibling, contractCallOpsQuery, was moved off this
// shape after the 2026-07-11 blowout; these two were not.
func inSubqueryOpsSQL(opTypes string, from, to uint32) string {
	return fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM stellar.operations AS o
		INNER JOIN stellar.operation_results AS r
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		WHERE o.ledger_seq BETWEEN %d AND %d
		  AND o.op_type IN (%s)
		  AND o.tx_hash IN (
		      SELECT tx_hash FROM stellar.transactions
		      WHERE successful = 1 AND ledger_seq BETWEEN %d AND %d
		  )
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`,
		from, to, opTypes, from, to)
}

// derivedWindowOpsSQL is the REJECTED first remediation: the IN-set is gone,
// but the outer ledger window is wrapped in a derived table, which costs
// stellar.operation_results its primary-key pruning entirely.
func derivedWindowOpsSQL(opTypes string, from, to uint32) string {
	return fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM (
		    SELECT ledger_seq, close_time, tx_hash, op_index, source_account, body_xdr
		    FROM stellar.operations
		    WHERE ledger_seq BETWEEN %d AND %d
		      AND op_type IN (%s)
		) AS o
		INNER JOIN (
		    SELECT tx_hash FROM stellar.transactions
		    WHERE successful = 1 AND ledger_seq BETWEEN %d AND %d
		    GROUP BY tx_hash
		) AS t ON o.tx_hash = t.tx_hash
		INNER JOIN stellar.operation_results AS r
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`,
		from, to, opTypes, from, to)
}

// joinNoDedupeOpsSQL is the SHIPPED shape with the derived table's
// `GROUP BY tx_hash` removed — the correctness hazard IN's set semantics used
// to cover for free, and the reason the dedupe is not cosmetic.
func joinNoDedupeOpsSQL(opTypes string, from, to uint32) string {
	return fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM stellar.operations AS o
		INNER JOIN (
		    SELECT tx_hash FROM stellar.transactions
		    WHERE successful = 1 AND ledger_seq BETWEEN %d AND %d
		) AS t ON o.tx_hash = t.tx_hash
		INNER JOIN stellar.operation_results AS r
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		WHERE o.ledger_seq BETWEEN %d AND %d
		  AND o.op_type IN (%s)
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`,
		from, to, from, to, opTypes)
}

// ── fixture ─────────────────────────────────────────────────────────────────

type opsPruneFixture struct {
	from, to    uint32
	wantSDEX    int // trade-typed ops in the window whose tx succeeded
	wantClassic int // payment-typed ops in the window whose tx succeeded
}

// seedOpsPruneFixture writes opsPruneRows ledgers of one op / one result / one
// tx each, alternating a trade op type and a classic one, with every
// opsPruneFailMod'th tx failed, plus a duplicate transactions part over the
// read window. Merges on stellar.transactions are stopped for the duration so
// that duplicate part is still there when the queries run.
func seedOpsPruneFixture(t *testing.T, ctx context.Context, raw driver.Conn) opsPruneFixture {
	t.Helper()
	const sourceAccount = "GTEST_OPSPRUNE_SOURCE_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := raw.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %.90q: %v", q, err)
		}
	}
	mustExec(`SYSTEM STOP MERGES stellar.transactions`)
	// WithoutCancel, not the test's ctx: cleanups run after the test function
	// has returned and cancelled it, and leaving merges stopped would leak into
	// every later test sharing this container.
	restoreCtx := context.WithoutCancel(ctx)
	t.Cleanup(func() { _ = raw.Exec(restoreCtx, `SYSTEM START MERGES stellar.transactions`) })

	seq := fmt.Sprintf(`toUInt32(%d + number)`, opsPruneBase)
	hash := `lpad(toString(` + seq + `), 64, '0')`
	// Kept inside 2026-04 so the fixture never becomes the global close_time
	// tip another test anchors on (account_activity_watermark_test.go).
	closeAt := `toDateTime('2026-04-01 00:00:00', 'UTC') + toIntervalMillisecond(number)`
	isTrade := `number % 2 = 0`
	body := fmt.Sprintf(`if(%s, '%s', '%s')`, isTrade,
		opsPruneBodyB64(t, opsPruneSellOfferBody()), opsPruneBodyB64(t, opsPrunePaymentBody()))

	mustExec(fmt.Sprintf(`INSERT INTO stellar.operations
		(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)
		SELECT %s, %s, %s, 0, 0, if(%s, '%s', '%s'), '%s', %s FROM numbers(%d)`,
		seq, closeAt, hash, isTrade, opsPruneSDEXType, opsPruneClassicType, sourceAccount, body, opsPruneRows))
	mustExec(fmt.Sprintf(`INSERT INTO stellar.operation_results
		(ledger_seq, tx_hash, op_index, result_code, result_xdr)
		SELECT %s, %s, 0, 0, '%s' FROM numbers(%d)`,
		seq, hash, opsPruneResultB64(t), opsPruneRows))
	insertTxs := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.transactions
			(ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee,
			 operation_count, successful, result_code, memo_type, memo)
			SELECT %s, %s, %s, 0, '%s', 100, 200, 1, if(number %% %d = 0, 0, 1), 0, 'none', ''
			FROM numbers(%d) WHERE %s`,
			seq, closeAt, hash, sourceAccount, opsPruneFailMod, opsPruneRows, where))
	}
	insertTxs(`1`)
	insertTxs(fmt.Sprintf(`number < %d`, opsPruneDupRows))

	f := opsPruneFixture{from: opsPruneBase, to: opsPruneBase + opsPruneWindow - 1}
	for n := uint32(0); n < opsPruneWindow; n++ {
		if n%opsPruneFailMod == 0 {
			continue
		}
		if n%2 == 0 {
			f.wantSDEX++
		} else {
			f.wantClassic++
		}
	}
	if f.wantSDEX == 0 || f.wantClassic == 0 {
		t.Fatalf("fixture window yields no rows (sdex %d, classic %d)", f.wantSDEX, f.wantClassic)
	}
	return f
}

func opsPruneSellOfferBody() xdr.OperationBody {
	const issuerAccount = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
	return xdr.OperationBody{
		Type: xdr.OperationTypeManageSellOffer,
		ManageSellOfferOp: &xdr.ManageSellOfferOp{
			Selling: xdr.MustNewNativeAsset(),
			Buying:  xdr.MustNewCreditAsset("USDC", issuerAccount),
			Amount:  xdr.Int64(1_000_0000),
			Price:   xdr.Price{N: 1, D: 2},
		},
	}
}

func opsPrunePaymentBody() xdr.OperationBody {
	const destAccount = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	return xdr.OperationBody{
		Type: xdr.OperationTypePayment,
		PaymentOp: &xdr.PaymentOp{
			Destination: xdr.MustMuxedAddress(destAccount),
			Asset:       xdr.MustNewNativeAsset(),
			Amount:      xdr.Int64(42_0000000),
		},
	}
}

func opsPruneBodyB64(t *testing.T, body xdr.OperationBody) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(body)
	if err != nil {
		t.Fatalf("marshal op body: %v", err)
	}
	return b64
}

func opsPruneResultB64(t *testing.T) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(xdr.OperationResult{Code: xdr.OperationResultCodeOpBadAuth})
	if err != nil {
		t.Fatalf("marshal op result: %v", err)
	}
	return b64
}

// ── plan + cost helpers ─────────────────────────────────────────────────────

type opsPruneKey struct {
	ledger  uint32
	txHash  string
	opIndex uint32
}

// explainIndexes returns `EXPLAIN indexes = 1` for sql, one plan line per
// element (indentation preserved — granulesFor walks the subtree by indent).
func explainIndexes(t *testing.T, ctx context.Context, raw driver.Conn, label, sql string) []string {
	t.Helper()
	rows, err := raw.Query(ctx, "EXPLAIN indexes = 1 "+sql)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v\n%s", label, err, sql)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN %s: %v", label, err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN %s rows: %v", label, err)
	}
	if len(plan) == 0 {
		t.Fatalf("EXPLAIN %s returned no plan", label)
	}
	return plan
}

func planHasStep(plan []string, step string) bool {
	for _, l := range plan {
		if strings.Contains(l, step) {
			return true
		}
	}
	return false
}

// granulesFor reads the LAST `Granules: read/total` of the ReadFromMergeTree
// node for `table` — the count after the final index step (Min-Max, then
// Partition, then PrimaryKey, then any skip index), i.e. what the query
// actually reads. ok is false when the node carries no index analysis at all,
// which is itself the unpruned verdict.
//
// The node's `Indexes:` block sits at the SAME indentation as the
// ReadFromMergeTree line, with its sections nested under it; the subtree walk
// has to allow that one sibling or it stops before reading anything.
func granulesFor(plan []string, table string) (read, total int, ok bool) {
	node := -1
	for i, l := range plan {
		if strings.Contains(l, "ReadFromMergeTree") && strings.Contains(l, table) {
			node = i
			break
		}
	}
	if node < 0 {
		return 0, 0, false
	}
	indent := len(plan[node]) - len(strings.TrimLeft(plan[node], " "))
	for _, l := range plan[node+1:] {
		body := strings.TrimLeft(l, " ")
		if body == "" {
			continue
		}
		switch depth := len(l) - len(body); {
		case depth < indent, depth == indent && body != "Indexes:":
			return read, total, ok
		}
		var r, tot int
		if _, err := fmt.Sscanf(body, "Granules: %d/%d", &r, &tot); err == nil {
			read, total, ok = r, tot, true
		}
	}
	return read, total, ok
}

// requirePrunedOperationResults is the assertion the previous remediation
// failed: the ledger window must still reach stellar.operation_results through
// the join key, so the read is a primary-key range and not the whole table.
func requirePrunedOperationResults(t *testing.T, label string, plan []string) {
	t.Helper()
	read, total, ok := granulesFor(plan, "operation_results")
	if !ok {
		t.Fatalf("%s: stellar.operation_results is read with NO primary-key index analysis — "+
			"the ledger window is not propagated into the join's build side, so it full-scans "+
			"full chain history of wide result_xdr:\n%s", label, strings.Join(plan, "\n"))
	}
	if read >= total {
		t.Fatalf("%s: stellar.operation_results reads ALL %d/%d granules — primary-key pruning lost:\n%s",
			label, read, total, strings.Join(plan, "\n"))
	}
	t.Logf("%s: operation_results granules %d/%d (pruned)", label, read, total)
}

// oracleKeys runs a frozen oracle query and returns its emitted keys.
func oracleKeys(t *testing.T, ctx context.Context, raw driver.Conn, label, sql string) []opsPruneKey {
	t.Helper()
	rows, err := raw.Query(ctx, sql)
	if err != nil {
		t.Fatalf("%s: %v\n%s", label, err, sql)
	}
	defer func() { _ = rows.Close() }()
	var out []opsPruneKey
	for rows.Next() {
		var (
			k                    opsPruneKey
			closeTime            time.Time
			source, body, result string
		)
		if err := rows.Scan(&k.ledger, &closeTime, &k.txHash, &k.opIndex, &source, &body, &result); err != nil {
			t.Fatalf("%s scan: %v", label, err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s rows: %v", label, err)
	}
	return out
}

// readRowsOf runs sql under a fresh query id and reports its read_rows.
func readRowsOf(t *testing.T, ctx context.Context, raw driver.Conn, label, sql string) uint64 {
	t.Helper()
	id := uuid.NewString()
	_ = oracleKeys(t, clickhouse.Context(ctx, clickhouse.WithQueryID(id)), raw, label, sql)
	return queryLogReadRows(t, ctx, raw, label, id)
}

func queryLogReadRows(t *testing.T, ctx context.Context, raw driver.Conn, label, queryID string) uint64 {
	t.Helper()
	if err := raw.Exec(ctx, `SYSTEM FLUSH LOGS`); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	var rr uint64
	if err := raw.QueryRow(ctx, `SELECT read_rows FROM system.query_log
		WHERE query_id = ? AND type = 'QueryFinish'
		ORDER BY event_time_microseconds DESC LIMIT 1`, queryID).Scan(&rr); err != nil {
		t.Fatalf("query_log read_rows (%s): %v", label, err)
	}
	return rr
}

// captureShippedQuery drives a reader through its PRODUCTION entry point and
// recovers the exact SQL ClickHouse executed (the driver binds client-side, so
// system.query_log holds the statement with its literals) plus its read_rows.
func captureShippedQuery(t *testing.T, ctx context.Context, raw driver.Conn,
	label string, stream func(context.Context) error,
) (sql string, readRows uint64) {
	t.Helper()
	id := uuid.NewString()
	if err := stream(clickhouse.Context(ctx, clickhouse.WithQueryID(id))); err != nil {
		t.Fatalf("%s stream: %v", label, err)
	}
	if err := raw.Exec(ctx, `SYSTEM FLUSH LOGS`); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	if err := raw.QueryRow(ctx, `SELECT query, read_rows FROM system.query_log
		WHERE query_id = ? AND type = 'QueryFinish'
		ORDER BY event_time_microseconds DESC LIMIT 1`, id).Scan(&sql, &readRows); err != nil {
		t.Fatalf("query_log (%s): %v", label, err)
	}
	return sql, readRows
}

// opTypeInListFrom lifts the op-type IN list out of the shipped statement, so
// the frozen oracles filter on EXACTLY what the reader filtered on and any row
// difference between them is attributable to the query SHAPE alone.
func opTypeInListFrom(t *testing.T, label, sql string) string {
	t.Helper()
	// Unqualified, so a reshaped statement that spells the filter inside a
	// derived table still reaches the pruning verdict below rather than dying
	// here on a cosmetic alias change.
	const marker = "op_type IN ("
	i := strings.Index(sql, marker)
	if i < 0 {
		t.Fatalf("%s: shipped statement has no op-type filter:\n%s", label, sql)
	}
	rest := sql[i+len(marker):]
	j := strings.Index(rest, ")")
	if j < 0 {
		t.Fatalf("%s: unterminated op-type filter:\n%s", label, sql)
	}
	return rest[:j]
}

// ── the test ────────────────────────────────────────────────────────────────

// TestStreamOpsQueriesPruneOperationResults is the live proof for F111 / T385.
func TestStreamOpsQueriesPruneOperationResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	f := seedOpsPruneFixture(t, ctx, raw)

	var sdexKeys []opsPruneKey
	sdexSQL, sdexRead := captureShippedQuery(t, ctx, raw, "sdex", func(qctx context.Context) error {
		sdexKeys = nil
		return chstore.StreamSDEXOps(qctx, addr, f.from, f.to, func(op chstore.SDEXOp) error {
			sdexKeys = append(sdexKeys, opsPruneKey{op.Ledger, op.TxHash, op.OpIndex})
			return nil
		})
	})
	assertOpStreamBounded(t, ctx, raw, f, "sdex", sdexSQL, sdexRead, sdexKeys, f.wantSDEX)

	var classicKeys []opsPruneKey
	classicSQL, classicRead := captureShippedQuery(t, ctx, raw, "classic", func(qctx context.Context) error {
		classicKeys = nil
		return chstore.StreamClassicOps(qctx, addr, f.from, f.to,
			[]string{opsPruneClassicType}, func(op chstore.ClassicOp) error {
				classicKeys = append(classicKeys, opsPruneKey{op.Ledger, op.TxHash, op.OpIndex})
				return nil
			})
	})
	assertOpStreamBounded(t, ctx, raw, f, "classic", classicSQL, classicRead, classicKeys, f.wantClassic)
}

// assertOpStreamBounded pins, for one shipped op-stream statement: no
// successful-tx set-build (F111/T385), primary-key pruning retained on
// stellar.operation_results (the rejected remediation), the exact rows the
// IN-subquery shape served (no over- or under-count from the join), and a
// read_rows bound. Each assertion is paired with a non-vacuity guard against
// the frozen oracle that is supposed to violate it.
func assertOpStreamBounded(t *testing.T, ctx context.Context, raw driver.Conn, f opsPruneFixture,
	name, shippedSQL string, shippedRead uint64, got []opsPruneKey, want int,
) {
	t.Helper()
	opTypes := opTypeInListFrom(t, name, shippedSQL)
	legacySQL := inSubqueryOpsSQL(opTypes, f.from, f.to)
	rejectedSQL := derivedWindowOpsSQL(opTypes, f.from, f.to)

	// (1) The defect: the successful-tx set must not be materialised.
	plan := explainIndexes(t, ctx, raw, name+" shipped", shippedSQL)
	if planHasStep(plan, "CreatingSet") {
		t.Errorf("%s: the shipped statement still plans a CreatingSet — the window's whole "+
			"successful-tx hash set is materialised before the join (the 10 GiB blowout of "+
			"2026-07-11):\n%s", name, strings.Join(plan, "\n"))
	}
	if !planHasStep(explainIndexes(t, ctx, raw, name+" in-subquery oracle", legacySQL), "CreatingSet") {
		t.Fatalf("%s: the frozen IN-subquery oracle no longer plans a CreatingSet against this "+
			"fixture — the assertion above cannot fail and is vacuous", name)
	}

	// (2) The rejected remediation: pruning must survive the reshape.
	requirePrunedOperationResults(t, name+" shipped", plan)
	rejectedPlan := explainIndexes(t, ctx, raw, name+" derived-window oracle", rejectedSQL)
	if read, total, ok := granulesFor(rejectedPlan, "operation_results"); ok && read < total {
		t.Fatalf("%s: the frozen derived-window oracle still prunes operation_results (%d/%d "+
			"granules) — the pruning assertion above cannot fail and is vacuous", name, read, total)
	}

	// (3) Rows: exactly what the IN-subquery shape served, and no more.
	wantKeys := oracleKeys(t, ctx, raw, name+" in-subquery oracle", legacySQL)
	if len(got) != len(wantKeys) || len(got) != want {
		t.Fatalf("%s: reader served %d rows, IN-subquery oracle %d, fixture expects %d",
			name, len(got), len(wantKeys), want)
	}
	for i := range got {
		if got[i] != wantKeys[i] {
			t.Fatalf("%s row %d differs: reader %+v, IN-subquery oracle %+v", name, i, got[i], wantKeys[i])
		}
	}
	if fanned := oracleKeys(t, ctx, raw, name+" no-dedupe oracle",
		joinNoDedupeOpsSQL(opTypes, f.from, f.to)); len(fanned) <= len(got) {
		t.Fatalf("%s: dropping GROUP BY tx_hash served %d rows, not more than the shipped %d — "+
			"the duplicate transactions part no longer fans out, so the dedupe is unproven",
			name, len(fanned), len(got))
	}

	// (4) Cost: the shipped read stays a window read.
	rejectedRead := readRowsOf(t, ctx, raw, name+" derived-window oracle", rejectedSQL)
	t.Logf("%s read_rows: shipped=%d derived-window=%d (window %d ledgers of %d)",
		name, shippedRead, rejectedRead, opsPruneWindow, opsPruneRows)
	if rejectedRead < opsPruneRows {
		t.Fatalf("%s: the derived-window oracle read only %d rows — it is not the unbounded scan "+
			"this bound is calibrated against", name, rejectedRead)
	}
	if shippedRead*4 > rejectedRead {
		t.Fatalf("%s: the shipped statement read %d rows against the unpruned shape's %d — the read "+
			"must be bounded by the ledger window, not by chain history", name, shippedRead, rejectedRead)
	}
}
