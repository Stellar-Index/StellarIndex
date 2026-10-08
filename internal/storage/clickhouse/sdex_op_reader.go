package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// tradeOpTypes is the set of operation types that can emit ClaimAtoms
// (classic SDEX trades), as op.Body.Type.String() — the exact strings the
// extractor stores in stellar.operations.op_type. Mirrors
// internal/sources/sdex.matchesTradeOp. Used to prefilter the lake scan so
// the op-based re-derivation touches only trade-bearing ops, not all 23 B.
// These are compile-time constants (not user input), so inlining them into
// the IN list below carries no injection risk and avoids driver-specific
// slice-binding behaviour for IN (?).
var tradeOpTypes = []string{
	"OperationTypeManageSellOffer",
	"OperationTypeManageBuyOffer",
	"OperationTypeCreatePassiveSellOffer",
	"OperationTypePathPaymentStrictReceive",
	"OperationTypePathPaymentStrictSend",
}

// tradeOpTypeInList renders tradeOpTypes as a SQL IN list: 'a','b',...
func tradeOpTypeInList() string {
	quoted := make([]string, len(tradeOpTypes))
	for i, t := range tradeOpTypes {
		quoted[i] = "'" + t + "'"
	}
	return strings.Join(quoted, ",")
}

// SDEXOp is one trade-eligible operation reconstructed from the ClickHouse
// lake — the op body + its result + ledger context, enough to feed the SDEX
// OpDecoder (the caller maps it to dispatcher.OpContext). Source is the
// resolved op source account (the op's own if it set one, else the tx
// source), which the decoder uses as the trade Taker.
type SDEXOp struct {
	Ledger   uint32
	ClosedAt time.Time
	TxHash   string
	Source   string
	OpIndex  uint32
	Op       xdr.Operation
	OpResult xdr.OperationResult
}

// StreamSDEXOps is the Phase-4 op-based input adapter (ADR-0034): SDEX trades
// are op-derived, NOT event-derived. It reads stellar.operations joined to
// stellar.operation_results on (ledger_seq, tx_hash, op_index) for [from,to]
// inclusive, restricted to the trade-bearing op types AND to SUCCESSFUL
// transactions, reconstructs op.Body + the OperationResult from the retained
// XDR blobs, and invokes fn for each in dispatcher emission order.
//
// Failed transactions are excluded: their op results can still carry success
// codes + claim atoms for ops that ran before the failing op, but those
// trades were rolled back. dispatcher.CensusLedger and internal/sources/sdex
// both count successful txs only, so the re-derivation must too.
//
// NO FINAL — deliberately: FINAL on a ReplacingMergeTree merges across ALL
// overlapping parts of each touched PARTITION (~1M ledgers of wide body_xdr
// rows) even for a 100k-ledger WHERE window, which repeatedly blew the
// query-memory ceiling. Duplicate rows from un-merged parts are HARMLESS to
// every caller: the census consumer PK-dedups via its `seen` map and
// ch-rebuild's InsertTrade is ON CONFLICT DO NOTHING. join_algorithm=
// grace_hash keeps the join memory-bounded (see sdexOpsQuery). Callers
// re-deriving all history should still window [from,to].
func StreamSDEXOps(ctx context.Context, addr string, from, to uint32, fn func(SDEXOp) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// (from, to) twice: the successful-tx derived table's window FIRST (it is
	// spelled first), then the outer scan's. See sdexOpsQuery's bind-order note.
	rows, err := conn.Query(ctx, sdexOpsQuery(), from, to, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query sdex ops [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()
	return streamSDEXOpRows(rows, fn)
}

// sdexOpsQuery is StreamSDEXOps' SQL, built here so its text stays
// independently assertable (StreamSDEXOps dials its own connection via
// openRead, so stubConn cannot drive it end to end).
//
// The successful-tx restriction is a grace_hash INNER JOIN over a derived
// table, not an IN-subquery: IN materialises the window's tx-hash set in
// memory first and blew the 10 GiB query budget on a dense window. GROUP BY
// tx_hash gives the derived table SET semantics, so an un-merged duplicate
// transactions part cannot fan one op row out into two (NO FINAL).
//
// The join is spelled BEFORE the outer WHERE, as in contractCallOpsQuery;
// that placement is load-bearing: wrapping the outer ledger window in a
// derived table stops ClickHouse propagating it through
// o.ledger_seq = r.ledger_seq, and stellar.operation_results loses
// primary-key pruning entirely.
//
// The FOUR bind parameters are, in order: the successful-tx derived table's
// from + to (written FIRST), then the outer ledger window's from + to. A
// reorder that does not move the argument list is a silent wrong-window
// read, so the order is pinned by test.
//
// The op-type filter is INTERPOLATED, not bound — see tradeOpTypeInList for
// why that carries no injection risk.
// why that carries no injection risk (compile-time constants only).
func sdexOpsQuery() string {
	return fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM stellar.operations AS o
		INNER JOIN (
		    SELECT tx_hash FROM stellar.transactions
		    WHERE successful = 1 AND ledger_seq BETWEEN ? AND ?
		    GROUP BY tx_hash
		) AS t ON o.tx_hash = t.tx_hash
		INNER JOIN stellar.operation_results AS r
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		WHERE o.ledger_seq BETWEEN ? AND ?
		  AND o.op_type IN (%s)
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`, tradeOpTypeInList())
}

// streamSDEXOpRows decodes an open sdexOpsQuery result set and invokes fn once
// per row, in the order ClickHouse returned them. Split out of StreamSDEXOps so
// the decode + fan-out half is drivable against a stub driver.Rows (the
// scanOpParticipants precedent in participant_backfill.go).
//
// fn's error aborts the stream and is returned VERBATIM: callers use a sentinel
// from fn as a stop signal, so it must not be wrapped.
func streamSDEXOpRows(rows driver.Rows, fn func(SDEXOp) error) error {
	for rows.Next() {
		var (
			ledger    uint32
			closeTime time.Time
			txHash    string
			opIndex   uint32
			source    string
			bodyXDR   string
			resultXDR string
		)
		if err := rows.Scan(&ledger, &closeTime, &txHash, &opIndex, &source,
			&bodyXDR, &resultXDR); err != nil {
			return fmt.Errorf("clickhouse: scan sdex op: %w", err)
		}

		var body xdr.OperationBody
		if err := xdr.SafeUnmarshalBase64(bodyXDR, &body); err != nil {
			return fmt.Errorf("clickhouse: unmarshal op body (ledger %d tx %s op %d): %w",
				ledger, txHash, opIndex, err)
		}
		var res xdr.OperationResult
		if err := xdr.SafeUnmarshalBase64(resultXDR, &res); err != nil {
			return fmt.Errorf("clickhouse: unmarshal op result (ledger %d tx %s op %d): %w",
				ledger, txHash, opIndex, err)
		}

		if err := fn(SDEXOp{
			Ledger:   ledger,
			ClosedAt: closeTime.UTC(),
			TxHash:   txHash,
			Source:   source,
			OpIndex:  opIndex,
			Op:       xdr.Operation{Body: body},
			OpResult: res,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}
