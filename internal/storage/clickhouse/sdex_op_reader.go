package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// tradeOpTypes are the op types (stellar.operations.op_type) that can emit ClaimAtoms, mirroring internal/sources/sdex.matchesTradeOp;
// they prefilter the lake scan. Compile-time constants, so inlining them into the IN list is injection-safe.
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

// SDEXOp is one trade-eligible operation from the lake (op body, result, ledger context) for the SDEX OpDecoder; Source is the op source if set, else the tx source (the trade Taker).
type SDEXOp struct {
	Ledger   uint32
	ClosedAt time.Time
	TxHash   string
	Source   string
	OpIndex  uint32
	Op       xdr.Operation
	OpResult xdr.OperationResult
}

// StreamSDEXOps streams trade-bearing ops of SUCCESSFUL transactions for [from,to] in order (op-derived trades, ADR-0034); deliberately NO FINAL (it blows the memory ceiling), duplicates are harmless to PK-deduping consumers, so window [from,to].
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

// sdexOpsQuery is StreamSDEXOps' SQL, built here so its text stays assertable. The successful-tx restriction is a grace_hash INNER JOIN
// over a derived table, not an IN-subquery (IN blew the budget on a dense window); GROUP BY tx_hash gives the derived table SET semantics
// so un-merged duplicates cannot fan one op row out into two (NO FINAL). The join is spelled BEFORE the outer WHERE, as in
// contractCallOpsQuery: wrapping the outer ledger window in a derived table stops ClickHouse propagating it through o.ledger_seq =
// r.ledger_seq and operation_results loses PK pruning. The FOUR bind parameters are, in order: the derived table's from + to, then the
// outer window's from + to; reordering without moving the argument list is a silent wrong-window read (pinned by test). The op-type
// filter is INTERPOLATED, not bound: see tradeOpTypeInList.
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

// streamSDEXOpRows decodes an open sdexOpsQuery result set and invokes fn once per row; split out to be drivable against a stub
// driver.Rows. fn's error aborts the stream and is returned VERBATIM (callers use a sentinel as a stop signal).
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
