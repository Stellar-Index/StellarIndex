package clickhouse

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// The lake-read streaming adapters (StreamSDEXOps, StreamClassicOps,
// StreamContractCallOps, StreamMintBurnFlows) each dial their OWN ClickHouse
// connection via openRead, so they cannot be driven end-to-end by this
// package's stubConn harness. They are covered in the two halves the code is
// split into, exactly as BackfillTxHashIndex is:
//
//   - the SQL text, asserted against the package-level query builder;
//   - the decode + fan-out loop, driven against a stub driver.Rows.
//
// What that leaves unproven is stated in the deliverable rather than papered
// over: that ClickHouse ANSWERS these queries with the shape claimed, which
// only a real server can show.

// ── fixtures ────────────────────────────────────────────────────────────────

// opResultB64 marshals an OperationResult to the base64 the lake stores in
// stellar.operation_results.result_xdr. OpBadAuth needs no inner union arm,
// which keeps the fixture about the STREAMING contract rather than about any
// one operation's result shape.
func opResultB64(t *testing.T, code xdr.OperationResultCode) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(xdr.OperationResult{Code: code})
	if err != nil {
		t.Fatalf("MarshalBase64(OperationResult): %v", err)
	}
	return b64
}

// sellOfferBody is a real ManageSellOffer body — one of the five trade-bearing
// op types StreamSDEXOps filters on.
func sellOfferBody(t *testing.T) string {
	t.Helper()
	return opBody(t, xdr.OperationTypeManageSellOffer, xdr.ManageSellOfferOp{
		Selling: xdr.MustNewNativeAsset(),
		Buying:  xdr.MustNewCreditAsset("USDC", testIssuer),
		Amount:  xdr.Int64(1_000_0000),
		Price:   xdr.Price{N: 1, D: 2},
		OfferId: 0,
	})
}

// paymentBody is a real Payment body — a classic-movement op type.
func paymentBody(t *testing.T) string {
	t.Helper()
	return opBody(t, xdr.OperationTypePayment, xdr.PaymentOp{
		Destination: xdr.MustMuxedAddress(testDest),
		Asset:       xdr.MustNewNativeAsset(),
		Amount:      xdr.Int64(42_0000000),
	})
}

// nonUTC is a fixed instant expressed in a NON-UTC zone. Every streaming
// reader normalises close_time to UTC on the way out; a fixture already in UTC
// could not tell a working normalisation from a missing one.
func nonUTC(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("UTC+7", 7*3600))
}

// ── SQL shape ───────────────────────────────────────────────────────────────

// requireSuccessfulTxRestriction pins the correctness predicate shared by the
// three op-stream queries: a FAILED transaction's op results still carry
// success codes and claim atoms for the ops that ran BEFORE the failing one,
// but those effects were rolled back and never happened. Losing this clause
// silently over-counts phantom fills / phantom movements in every re-derive —
// a wrong-answer regression, not a perf one.
func requireSuccessfulTxRestriction(t *testing.T, name, q string) {
	t.Helper()
	if !strings.Contains(q, "successful = 1") {
		t.Errorf("%s lost its successful-tx restriction (phantom rolled-back ops would be re-derived):\n%s", name, q)
	}
}

// TestSDEXOpsQuery_Shape pins StreamSDEXOps' SQL.
func TestSDEXOpsQuery_Shape(t *testing.T) {
	q := sdexOpsQuery()

	requireSuccessfulTxRestriction(t, "sdexOpsQuery", q)

	// The join memory bound. grace_hash spills join buckets to disk; without
	// it this join is the sdex-reconcile OOM class (three OOMs, 2026-07).
	for _, s := range []string{
		"SETTINGS join_algorithm = 'grace_hash'",
		"grace_hash_join_initial_buckets = 32",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("sdexOpsQuery missing bounded-join setting %q:\n%s", s, q)
		}
	}

	// NO FINAL — deliberate, and load-bearing: FINAL on a ReplacingMergeTree
	// merges across every overlapping part of each touched PARTITION (~1M
	// ledgers of wide body_xdr rows) even for a 100k-ledger WHERE window,
	// which is precisely what blew the query-memory ceiling. Duplicates are
	// harmless to every caller by construction (see StreamSDEXOps' doc).
	if strings.Contains(q, "FINAL") {
		t.Errorf("sdexOpsQuery must NOT use FINAL — the partition-scale merge is the documented OOM class:\n%s", q)
	}

	// Every trade-bearing op type must be in the filter, and nothing else:
	// a missing arm is silently-lost trades, an extra arm is decode churn.
	for _, ot := range tradeOpTypes {
		if !strings.Contains(q, "'"+ot+"'") {
			t.Errorf("sdexOpsQuery does not filter in trade op type %q:\n%s", ot, q)
		}
	}
	if got, want := strings.Count(q, "OperationType"), len(tradeOpTypes); got != want {
		t.Errorf("sdexOpsQuery names %d op types, want exactly the %d in tradeOpTypes", got, want)
	}

	// Emission order: the dispatcher's canonical (ledger, tx, op) order. A
	// consumer that reconstructs trades positionally depends on it.
	if !strings.Contains(q, "ORDER BY o.ledger_seq, o.tx_hash, o.op_index") {
		t.Errorf("sdexOpsQuery lost its dispatcher emission order:\n%s", q)
	}
}

// TestSDEXOpsQuery_BindOrderMatchesClauseOrder is the args-drift assertion.
// StreamSDEXOps binds (from, to, from, to); the query must therefore spell its
// windows in that order — outer scan first, successful-tx subquery second.
// If a refactor hoists the subquery above the outer WHERE (which is exactly
// how contractCallOpsQuery is written), the same argument list now binds the
// windows to the wrong clauses. Today both pairs are the same values, so this
// is invisible at runtime; it stops being invisible the moment a caller
// windows the two apart.
func TestSDEXOpsQuery_BindOrderMatchesClauseOrder(t *testing.T) {
	q := sdexOpsQuery()

	outer := strings.Index(q, "WHERE o.ledger_seq BETWEEN ? AND ?")
	sub := strings.Index(q, "WHERE successful = 1 AND ledger_seq BETWEEN ? AND ?")
	if outer < 0 {
		t.Fatalf("sdexOpsQuery has no outer ledger-window clause:\n%s", q)
	}
	if sub < 0 {
		t.Fatalf("sdexOpsQuery has no successful-tx window clause:\n%s", q)
	}
	if outer > sub {
		t.Fatalf("sdexOpsQuery binds (from,to,from,to) with the outer window FIRST, "+
			"but the successful-tx window is spelled first (outer@%d, sub@%d) — "+
			"the args now bind to the wrong clauses:\n%s", outer, sub, q)
	}
	// Exactly four placeholders, matching the four bound arguments.
	if got := strings.Count(q, "?"); got != 4 {
		t.Fatalf("sdexOpsQuery has %d placeholders, but StreamSDEXOps binds 4 (from,to,from,to):\n%s", got, q)
	}
}

// TestClassicOpsQuery_Shape pins StreamClassicOps' SQL. It is the SHARED
// ADR-0047 harness every phase of pre-P23 classic-movement reconstruction
// reuses, so its guarantees must not weaken as callers widen opTypes.
func TestClassicOpsQuery_Shape(t *testing.T) {
	opTypes := []string{"OperationTypePayment", "OperationTypeCreateAccount"}
	q := classicOpsQuery(opTypes)

	requireSuccessfulTxRestriction(t, "classicOpsQuery", q)
	for _, s := range []string{
		"SETTINGS join_algorithm = 'grace_hash'",
		"grace_hash_join_initial_buckets = 32",
		"ORDER BY o.ledger_seq, o.tx_hash, o.op_index",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("classicOpsQuery missing %q:\n%s", s, q)
		}
	}
	if strings.Contains(q, "FINAL") {
		t.Errorf("classicOpsQuery must NOT use FINAL (StreamSDEXOps' documented OOM class):\n%s", q)
	}
	for _, ot := range opTypes {
		if !strings.Contains(q, "'"+ot+"'") {
			t.Errorf("classicOpsQuery does not filter in caller op type %q:\n%s", ot, q)
		}
	}
	// The caller's list is the WHOLE filter — the reader must not smuggle in
	// op types the caller did not ask for.
	if got := strings.Count(q, "OperationType"); got != len(opTypes) {
		t.Errorf("classicOpsQuery names %d op types, want exactly the caller's %d", got, len(opTypes))
	}
	// Same bind order as sdexOpsQuery: outer window, then subquery window.
	outer := strings.Index(q, "WHERE o.ledger_seq BETWEEN ? AND ?")
	sub := strings.Index(q, "WHERE successful = 1 AND ledger_seq BETWEEN ? AND ?")
	if outer < 0 || sub < 0 || outer > sub {
		t.Fatalf("classicOpsQuery clause order does not match its (from,to,from,to) bind order "+
			"(outer@%d, sub@%d):\n%s", outer, sub, q)
	}
	if got := strings.Count(q, "?"); got != 4 {
		t.Fatalf("classicOpsQuery has %d placeholders, but StreamClassicOps binds 4:\n%s", got, q)
	}
}

// TestStreamClassicOps_RejectsEmptyOpTypes — an empty op-type list would
// render `IN ()`, a ClickHouse syntax error, after a connection had already
// been dialled. The guard fires FIRST, so no connection is opened; the
// unroutable address proves it.
func TestStreamClassicOps_RejectsEmptyOpTypes(t *testing.T) {
	err := StreamClassicOps(t.Context(), "127.0.0.1:1", 1, 2, nil, func(ClassicOp) error { return nil })
	if err == nil {
		t.Fatal("StreamClassicOps accepted an empty opTypes list")
	}
	if !strings.Contains(err.Error(), "opTypes is empty") {
		t.Fatalf("err = %v, want the opTypes guard (a dial error means the guard fired too late)", err)
	}
}

// TestClassicOpTypeInList_RendersQuotedCSV pins the IN-list rendering both
// op-stream readers depend on. The contract is compile-time constants only —
// there is no escaping here, deliberately (see the doc comment), so the shape
// must stay exactly `'a','b'`.
func TestClassicOpTypeInList_RendersQuotedCSV(t *testing.T) {
	if got, want := classicOpTypeInList([]string{"A", "B", "C"}), "'A','B','C'"; got != want {
		t.Errorf("classicOpTypeInList = %q, want %q", got, want)
	}
	if got, want := classicOpTypeInList([]string{"Solo"}), "'Solo'"; got != want {
		t.Errorf("classicOpTypeInList(single) = %q, want %q", got, want)
	}
	// tradeOpTypeInList is the same rendering over the fixed trade set.
	if got, want := tradeOpTypeInList(), "'"+strings.Join(tradeOpTypes, "','")+"'"; got != want {
		t.Errorf("tradeOpTypeInList = %q, want %q", got, want)
	}
}

// TestContractCallOpsQuery_Shape pins StreamContractCallOps' SQL, including
// the bind order — which is the REVERSE nesting of the other two op streams
// (the joined successful-tx derived table is written first), and is bound
// (from, to, from, to, contractHex) accordingly.
func TestContractCallOpsQuery_Shape(t *testing.T) {
	q := contractCallOpsQuery

	requireSuccessfulTxRestriction(t, "contractCallOpsQuery", q)

	// The memory bounds this query carries in its own right — the 2026-07-11
	// incident was an IN-subquery CreatingSetsTransform blowing a 10 GiB
	// budget on a dense 250k-ledger window.
	for _, s := range []string{
		"join_algorithm = 'grace_hash'",
		"grace_hash_join_initial_buckets = 32",
		"max_memory_usage = 8000000000",
		"max_bytes_before_external_sort = 2000000000",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("contractCallOpsQuery missing bounded setting %q:\n%s", s, q)
		}
	}
	// The successful-tx set MUST be a join, not an IN-subquery — that is the
	// whole fix. `IN (` reappearing here is the regression.
	if strings.Contains(q, "tx_hash IN (") {
		t.Errorf("contractCallOpsQuery reverted to an IN-subquery for the successful-tx set "+
			"(CreatingSetsTransform blew the 10 GiB budget, 2026-07-11):\n%s", q)
	}
	if !strings.Contains(q, "INNER JOIN (") {
		t.Errorf("contractCallOpsQuery lost its grace_hash INNER JOIN:\n%s", q)
	}

	// The contract match: the raw 32-byte id as a substring of the DECODED
	// body. stellar.operations carries no contract_id column, so this is the
	// only available prefilter.
	if !strings.Contains(q, "position(base64Decode(o.body_xdr), unhex(?)) > 0") {
		t.Errorf("contractCallOpsQuery lost its decoded-body contract match:\n%s", q)
	}
	if !strings.Contains(q, "o.op_type = 'OperationTypeInvokeHostFunction'") {
		t.Errorf("contractCallOpsQuery lost its InvokeHostFunction restriction:\n%s", q)
	}
	if !strings.Contains(q, "ORDER BY o.ledger_seq, o.tx_hash, o.op_index") {
		t.Errorf("contractCallOpsQuery lost its dispatcher emission order:\n%s", q)
	}

	// Bind order: subquery window FIRST (it is nested above the outer WHERE),
	// then the outer window, then the contract hex LAST.
	sub := strings.Index(q, "WHERE successful = 1 AND ledger_seq BETWEEN ? AND ?")
	outer := strings.Index(q, "WHERE o.ledger_seq BETWEEN ? AND ?")
	hex := strings.Index(q, "unhex(?)")
	if sub < 0 || outer < 0 || hex < 0 {
		t.Fatalf("contractCallOpsQuery missing a bound clause (sub@%d outer@%d hex@%d):\n%s", sub, outer, hex, q)
	}
	if !(sub < outer && outer < hex) {
		t.Fatalf("contractCallOpsQuery clause order (sub@%d, outer@%d, hex@%d) does not match its "+
			"(from,to,from,to,contractHex) bind order:\n%s", sub, outer, hex, q)
	}
	if got := strings.Count(q, "?"); got != 5 {
		t.Fatalf("contractCallOpsQuery has %d placeholders, but StreamContractCallOps binds 5:\n%s", got, q)
	}
}

// TestMintBurnFlowsQuery_FinalToggle pins the one thing useFinal may change.
// FINAL is a ~40x read cost that buys ReplacingMergeTree dedup; the two
// variants exist so a caller can choose. What they must NOT do is differ in
// predicate or projection — a FINAL variant that also narrowed the topic
// filter, or dropped a column, would make "with FINAL" and "without FINAL"
// answer different QUESTIONS rather than the same question at two accuracies.
func TestMintBurnFlowsQuery_FinalToggle(t *testing.T) {
	plain := mintBurnFlowsQuery(false)
	final := mintBurnFlowsQuery(true)

	if strings.Contains(plain, "FINAL") {
		t.Errorf("mintBurnFlowsQuery(false) must not use FINAL:\n%s", plain)
	}
	if !strings.Contains(final, "stellar.contract_events FINAL") {
		t.Errorf("mintBurnFlowsQuery(true) must read stellar.contract_events FINAL:\n%s", final)
	}
	// Byte-identical once the sole FINAL keyword is removed.
	if normalised := strings.Replace(final, "contract_events FINAL", "contract_events ", 1); normalised != plain {
		t.Errorf("the two mintBurnFlowsQuery variants differ by more than the FINAL keyword:\nfinal:\n%s\nplain:\n%s", final, plain)
	}

	for _, q := range []string{plain, final} {
		// The topic prefilter is what keeps this to the ~570M supply flows
		// instead of the ~12B total contract_events.
		if !strings.Contains(q, "topic_0_sym IN ('mint','burn','clawback')") {
			t.Errorf("mintBurnFlowsQuery lost its supply-flow topic prefilter:\n%s", q)
		}
		if !strings.Contains(q, "WHERE ledger_seq BETWEEN ? AND ?") {
			t.Errorf("mintBurnFlowsQuery lost its ledger-window predicate:\n%s", q)
		}
		if got := strings.Count(q, "?"); got != 2 {
			t.Errorf("mintBurnFlowsQuery has %d placeholders, but StreamMintBurnFlows binds 2:\n%s", got, q)
		}
		// event_index must be projected: (ledger, tx_hash, op_index) alone
		// does not identify one event when a single op emits several (the
		// reason migration 0058 added the column).
		if !strings.Contains(q, "event_index") {
			t.Errorf("mintBurnFlowsQuery lost event_index — the flows are then not uniquely keyed:\n%s", q)
		}
	}
}

// ── streaming semantics ─────────────────────────────────────────────────────

// sdexRow builds one sdexOpsQuery result row.
func sdexRow(ledger uint32, at time.Time, txHash string, opIndex uint32, source, body, result string) []any {
	return []any{ledger, at, txHash, opIndex, source, body, result}
}

// TestStreamSDEXOpRows_StreamsInOrderAndNormalisesTime drives the decode +
// fan-out half against a stub result set.
func TestStreamSDEXOpRows_StreamsInOrderAndNormalisesTime(t *testing.T) {
	at := nonUTC(t)
	body := sellOfferBody(t)
	result := opResultB64(t, xdr.OperationResultCodeOpBadAuth)

	rows := &stubRows{data: [][]any{
		sdexRow(100, at, "aa", 0, testSource, body, result),
		sdexRow(100, at, "aa", 1, testSource, body, result),
		sdexRow(101, at, "bb", 0, testDest, body, result),
	}}

	var got []SDEXOp
	if err := streamSDEXOpRows(rows, func(op SDEXOp) error {
		got = append(got, op)
		return nil
	}); err != nil {
		t.Fatalf("streamSDEXOpRows: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("streamed %d ops, want 3", len(got))
	}
	// Emission order is preserved verbatim — a consumer correlating a swap
	// with its following sync event depends on it.
	wantKeys := [][3]any{{uint32(100), "aa", uint32(0)}, {uint32(100), "aa", uint32(1)}, {uint32(101), "bb", uint32(0)}}
	for i, op := range got {
		if op.Ledger != wantKeys[i][0] || op.TxHash != wantKeys[i][1] || op.OpIndex != wantKeys[i][2] {
			t.Errorf("op %d = (%d,%s,%d), want (%v,%v,%v)", i, op.Ledger, op.TxHash, op.OpIndex,
				wantKeys[i][0], wantKeys[i][1], wantKeys[i][2])
		}
	}
	if got[2].Source != testDest {
		t.Errorf("Source = %q, want %q (the resolved op source feeds the trade Taker)", got[2].Source, testDest)
	}

	// close_time is normalised to UTC, and it is the SAME INSTANT — a
	// normalisation that reinterpreted the wall clock instead of converting
	// the zone would shift every trade by the offset.
	if loc := got[0].ClosedAt.Location(); loc != time.UTC {
		t.Errorf("ClosedAt location = %v, want UTC", loc)
	}
	if !got[0].ClosedAt.Equal(at) {
		t.Errorf("ClosedAt = %s, want the same instant as %s", got[0].ClosedAt, at)
	}

	// The op body really decoded — not merely carried through as bytes.
	if got[0].Op.Body.Type != xdr.OperationTypeManageSellOffer {
		t.Errorf("decoded body type = %v, want ManageSellOffer", got[0].Op.Body.Type)
	}
	if amt := got[0].Op.Body.ManageSellOfferOp.Amount; amt != xdr.Int64(1_000_0000) {
		t.Errorf("decoded ManageSellOffer amount = %d, want 10000000", amt)
	}
	if got[0].OpResult.Code != xdr.OperationResultCodeOpBadAuth {
		t.Errorf("decoded result code = %v, want OpBadAuth", got[0].OpResult.Code)
	}
}

// TestStreamSDEXOpRows_EmptyResultTerminates — an empty window is the normal
// case for a sparse range, not an error. fn must never be called.
func TestStreamSDEXOpRows_EmptyResultTerminates(t *testing.T) {
	calls := 0
	if err := streamSDEXOpRows(&stubRows{}, func(SDEXOp) error { calls++; return nil }); err != nil {
		t.Fatalf("streamSDEXOpRows(empty) = %v, want nil", err)
	}
	if calls != 0 {
		t.Fatalf("fn called %d times on an empty result set, want 0", calls)
	}
}

// TestStreamSDEXOpRows_ConsumerErrorStopsStreamVerbatim — fn's error is the
// caller's stop signal (ch-rebuild aborts a window on a write failure), so it
// must propagate UNWRAPPED and no further row may be decoded.
func TestStreamSDEXOpRows_ConsumerErrorStopsStreamVerbatim(t *testing.T) {
	stop := errors.New("consumer stopped")
	at := nonUTC(t)
	body := sellOfferBody(t)
	result := opResultB64(t, xdr.OperationResultCodeOpBadAuth)

	rows := &stubRows{data: [][]any{
		sdexRow(1, at, "aa", 0, testSource, body, result),
		sdexRow(2, at, "bb", 0, testSource, body, result),
		sdexRow(3, at, "cc", 0, testSource, body, result),
	}}
	seen := 0
	err := streamSDEXOpRows(rows, func(SDEXOp) error {
		seen++
		if seen == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the consumer's error verbatim (%v)", err, stop)
	}
	if seen != 2 {
		t.Fatalf("fn called %d times, want 2 — the stream must stop at the erroring row", seen)
	}
}

// TestStreamSDEXOpRows_TruncatedStreamIsAnError — rows.Err() reports a stream
// that ended early (dropped connection, server-side memory limit). Returning
// nil there hands a caller a SILENTLY PARTIAL window, which for a re-derive is
// an under-count that looks exactly like a genuinely sparse range.
func TestStreamSDEXOpRows_TruncatedStreamIsAnError(t *testing.T) {
	truncated := errors.New("connection reset mid-stream")
	rows := &stubRows{
		data:      [][]any{sdexRow(1, nonUTC(t), "aa", 0, testSource, sellOfferBody(t), opResultB64(t, xdr.OperationResultCodeOpBadAuth))},
		streamErr: truncated,
	}
	delivered := 0
	err := streamSDEXOpRows(rows, func(SDEXOp) error { delivered++; return nil })
	if !errors.Is(err, truncated) {
		t.Fatalf("err = %v, want the truncation error %v — a partial window must not report success", err, truncated)
	}
	if delivered != 1 {
		t.Fatalf("fn called %d times, want 1 (rows before the truncation are still delivered)", delivered)
	}
}

// TestStreamSDEXOpRows_UndecodableBodyIsFatalAndLocatable — unlike the
// participant backfill (which soft-skips a bad body and COUNTS it), the SDEX
// re-derive treats an undecodable op as fatal: a trade it cannot decode is a
// trade it would silently omit from a reconcile whose entire purpose is to
// count. The error must name the row so an operator can find it.
func TestStreamSDEXOpRows_UndecodableBodyIsFatalAndLocatable(t *testing.T) {
	rows := &stubRows{data: [][]any{
		sdexRow(6100, nonUTC(t), "deadbeef", 3, testSource, "!!!not-base64!!!", opResultB64(t, xdr.OperationResultCodeOpBadAuth)),
	}}
	err := streamSDEXOpRows(rows, func(SDEXOp) error {
		t.Fatal("fn must not be called for a row whose body failed to decode")
		return nil
	})
	if err == nil {
		t.Fatal("streamSDEXOpRows accepted an undecodable op body")
	}
	for _, want := range []string{"6100", "deadbeef", "3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to locate the row via %q", err, want)
		}
	}
}

// TestStreamSDEXOpRows_UndecodableResultIsFatal — the OperationResult carries
// the claim atoms. A body that decodes with a result that does not is still an
// unusable row.
func TestStreamSDEXOpRows_UndecodableResultIsFatal(t *testing.T) {
	rows := &stubRows{data: [][]any{
		sdexRow(7, nonUTC(t), "aa", 0, testSource, sellOfferBody(t), "%%%not-base64%%%"),
	}}
	if err := streamSDEXOpRows(rows, func(SDEXOp) error { return nil }); err == nil {
		t.Fatal("streamSDEXOpRows accepted an undecodable op result")
	}
}

// TestStreamClassicOpRows_DecodesAndNormalises mirrors the SDEX coverage for
// the ADR-0047 classic-movement harness.
func TestStreamClassicOpRows_DecodesAndNormalises(t *testing.T) {
	at := nonUTC(t)
	rows := &stubRows{data: [][]any{
		{uint32(55), at, "aa", uint32(0), testSource, paymentBody(t), opResultB64(t, xdr.OperationResultCodeOpBadAuth)},
	}}
	var got []ClassicOp
	if err := streamClassicOpRows(rows, func(op ClassicOp) error { got = append(got, op); return nil }); err != nil {
		t.Fatalf("streamClassicOpRows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("streamed %d ops, want 1", len(got))
	}
	if got[0].Op.Body.Type != xdr.OperationTypePayment {
		t.Errorf("decoded body type = %v, want Payment", got[0].Op.Body.Type)
	}
	if amt := got[0].Op.Body.PaymentOp.Amount; amt != xdr.Int64(42_0000000) {
		t.Errorf("decoded Payment amount = %d, want 420000000", amt)
	}
	if got[0].ClosedAt.Location() != time.UTC || !got[0].ClosedAt.Equal(at) {
		t.Errorf("ClosedAt = %s (%v), want the same instant as %s in UTC", got[0].ClosedAt, got[0].ClosedAt.Location(), at)
	}
	// Source is the RESOLVED effective source account, wired straight into
	// OpContext.TxSource by the caller.
	if got[0].Source != testSource {
		t.Errorf("Source = %q, want %q", got[0].Source, testSource)
	}
}

// TestStreamClassicOpRows_EmptyAndErrorPaths.
func TestStreamClassicOpRows_EmptyAndErrorPaths(t *testing.T) {
	calls := 0
	if err := streamClassicOpRows(&stubRows{}, func(ClassicOp) error { calls++; return nil }); err != nil || calls != 0 {
		t.Fatalf("empty result: err=%v calls=%d, want nil/0", err, calls)
	}
	truncated := errors.New("truncated")
	if err := streamClassicOpRows(&stubRows{streamErr: truncated}, func(ClassicOp) error { return nil }); !errors.Is(err, truncated) {
		t.Fatalf("truncated stream err = %v, want %v", err, truncated)
	}
	stop := errors.New("stop")
	rows := &stubRows{data: [][]any{
		{uint32(1), nonUTC(t), "aa", uint32(0), testSource, paymentBody(t), opResultB64(t, xdr.OperationResultCodeOpBadAuth)},
		{uint32(2), nonUTC(t), "bb", uint32(0), testSource, paymentBody(t), opResultB64(t, xdr.OperationResultCodeOpBadAuth)},
	}}
	seen := 0
	if err := streamClassicOpRows(rows, func(ClassicOp) error { seen++; return stop }); !errors.Is(err, stop) || seen != 1 {
		t.Fatalf("consumer stop: err=%v seen=%d, want %v/1", err, seen, stop)
	}
}

// TestStreamContractCallOpRows_DecodesInvokeBody — the ContractCall stream
// carries no result column (the census needs the CALL, not its outcome).
func TestStreamContractCallOpRows_DecodesInvokeBody(t *testing.T) {
	at := nonUTC(t)
	var cid xdr.ContractId
	for i := range cid {
		cid[i] = byte(i)
	}
	invoke := opBody(t, xdr.OperationTypeInvokeHostFunction, xdr.InvokeHostFunctionOp{
		HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
			InvokeContract: &xdr.InvokeContractArgs{
				ContractAddress: xdr.ScAddress{
					Type:       xdr.ScAddressTypeScAddressTypeContract,
					ContractId: &cid,
				},
				FunctionName: xdr.ScSymbol("relay"),
			},
		},
	})
	rows := &stubRows{data: [][]any{
		{uint32(900), at, "cc", uint32(2), testSource, invoke},
	}}
	var got []ContractCallOp
	if err := streamContractCallOpRows(rows, func(op ContractCallOp) error { got = append(got, op); return nil }); err != nil {
		t.Fatalf("streamContractCallOpRows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("streamed %d ops, want 1", len(got))
	}
	if got[0].Op.Body.Type != xdr.OperationTypeInvokeHostFunction {
		t.Errorf("decoded body type = %v, want InvokeHostFunction", got[0].Op.Body.Type)
	}
	if got[0].Ledger != 900 || got[0].OpIndex != 2 || got[0].TxHash != "cc" {
		t.Errorf("row key = (%d,%s,%d), want (900,cc,2)", got[0].Ledger, got[0].TxHash, got[0].OpIndex)
	}
	if got[0].ClosedAt.Location() != time.UTC || !got[0].ClosedAt.Equal(at) {
		t.Errorf("ClosedAt = %s (%v), want the same instant as %s in UTC", got[0].ClosedAt, got[0].ClosedAt.Location(), at)
	}
}

// TestStreamContractCallOpRows_EmptyAndErrorPaths.
func TestStreamContractCallOpRows_EmptyAndErrorPaths(t *testing.T) {
	calls := 0
	if err := streamContractCallOpRows(&stubRows{}, func(ContractCallOp) error { calls++; return nil }); err != nil || calls != 0 {
		t.Fatalf("empty result: err=%v calls=%d, want nil/0", err, calls)
	}
	truncated := errors.New("truncated")
	if err := streamContractCallOpRows(&stubRows{streamErr: truncated}, func(ContractCallOp) error { return nil }); !errors.Is(err, truncated) {
		t.Fatalf("truncated stream err = %v, want %v", err, truncated)
	}
	bad := &stubRows{data: [][]any{{uint32(4242), nonUTC(t), "beefcafe", uint32(1), testSource, "@@@not-base64@@@"}}}
	err := streamContractCallOpRows(bad, func(ContractCallOp) error {
		t.Fatal("fn must not be called for an undecodable body")
		return nil
	})
	if err == nil {
		t.Fatal("streamContractCallOpRows accepted an undecodable op body")
	}
	if !strings.Contains(err.Error(), "4242") || !strings.Contains(err.Error(), "beefcafe") {
		t.Errorf("err = %q, want it to locate the offending row", err)
	}
}

// TestStreamMintBurnFlowRows_CarriesRawDataXDR — the supply reader deliberately
// does NOT decode the amount: it hands the caller the raw base64 scval, because
// the i128 in there is exactly the value ADR-0003 forbids truncating, and some
// SEP-41 variants ship a MAP rather than a bare i128. This test pins that the
// blob crosses the seam byte-for-byte.
func TestStreamMintBurnFlowRows_CarriesRawDataXDR(t *testing.T) {
	at := nonUTC(t)
	// An i128 far beyond int64 — 2^100. If anything on this path ever parsed
	// the amount into an int64, this value could not survive.
	big := xdr.Int128Parts{Hi: xdr.Int64(1) << 36, Lo: xdr.Uint64(0)}
	dataXDR, err := xdr.MarshalBase64(xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &big})
	if err != nil {
		t.Fatalf("MarshalBase64(i128): %v", err)
	}

	rows := &stubRows{data: [][]any{
		{uint32(50), at, "CDEADBEEF", "aa", uint32(0), uint32(2), "mint", dataXDR},
		{uint32(51), at, "CDEADBEEF", "bb", uint32(1), uint32(0), "burn", dataXDR},
		{uint32(52), at, "CDEADBEEF", "cc", uint32(0), uint32(0), "clawback", dataXDR},
	}}
	var got []MintBurnFlow
	if err := streamMintBurnFlowRows(rows, func(f MintBurnFlow) error { got = append(got, f); return nil }); err != nil {
		t.Fatalf("streamMintBurnFlowRows: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("streamed %d flows, want 3", len(got))
	}
	for i, want := range []string{"mint", "burn", "clawback"} {
		if got[i].Kind != want {
			t.Errorf("flow %d Kind = %q, want %q", i, got[i].Kind, want)
		}
		if got[i].DataXDR != dataXDR {
			t.Errorf("flow %d DataXDR was altered in transit:\n got %q\nwant %q", i, got[i].DataXDR, dataXDR)
		}
	}
	// event_index must survive: two flows from one op are distinguishable
	// only by it (see mintBurnFlowsQuery).
	if got[0].EventIndex != 2 {
		t.Errorf("EventIndex = %d, want 2", got[0].EventIndex)
	}
	// And the round trip really is lossless for an above-int64 amount.
	var back xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(got[0].DataXDR, &back); err != nil {
		t.Fatalf("re-decoding the carried DataXDR: %v", err)
	}
	if back.Type != xdr.ScValTypeScvI128 || back.I128.Hi != big.Hi || back.I128.Lo != big.Lo {
		t.Errorf("carried i128 = %+v, want %+v (ADR-0003: no truncation on the supply path)", back.I128, big)
	}

	// CloseTime is carried as stored — this reader, unlike the op streams,
	// makes no UTC claim, so pin what it actually does rather than inventing
	// a guarantee the callers do not have.
	if !got[0].CloseTime.Equal(at) {
		t.Errorf("CloseTime = %s, want the same instant as %s", got[0].CloseTime, at)
	}
}

// TestStreamMintBurnFlowRows_EmptyAndErrorPaths.
func TestStreamMintBurnFlowRows_EmptyAndErrorPaths(t *testing.T) {
	calls := 0
	if err := streamMintBurnFlowRows(&stubRows{}, func(MintBurnFlow) error { calls++; return nil }); err != nil || calls != 0 {
		t.Fatalf("empty result: err=%v calls=%d, want nil/0", err, calls)
	}
	truncated := errors.New("truncated")
	if err := streamMintBurnFlowRows(&stubRows{streamErr: truncated}, func(MintBurnFlow) error { return nil }); !errors.Is(err, truncated) {
		t.Fatalf("truncated stream err = %v, want %v", err, truncated)
	}
	stop := errors.New("stop")
	rows := &stubRows{data: [][]any{
		{uint32(1), nonUTC(t), "C1", "aa", uint32(0), uint32(0), "mint", "AAAA"},
		{uint32(2), nonUTC(t), "C1", "bb", uint32(0), uint32(0), "burn", "AAAA"},
	}}
	seen := 0
	if err := streamMintBurnFlowRows(rows, func(MintBurnFlow) error { seen++; return stop }); !errors.Is(err, stop) || seen != 1 {
		t.Fatalf("consumer stop: err=%v seen=%d, want %v/1", err, seen, stop)
	}
}
