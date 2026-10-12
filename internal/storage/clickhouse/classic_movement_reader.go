package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ClassicOp is one lake-derived classic operation for pre-P23 movement reconstruction (ADR-0047); Source is the already-resolved effective source, wired into OpContext.TxSource with OpSource left empty.
type ClassicOp struct {
	Ledger   uint32
	ClosedAt time.Time
	TxHash   string
	Source   string
	OpIndex  uint32
	Op       xdr.Operation
	OpResult xdr.OperationResult
}

// classicOpTypeInList renders an op-type slice as a SQL IN list. opTypes MUST be compile-time constants (e.g.
// classicmovements.SupportedOpTypes()), never user input: they are interpolated, as in tradeOpTypeInList.
func classicOpTypeInList(opTypes []string) string {
	quoted := make([]string, len(opTypes))
	for i, t := range opTypes {
		quoted[i] = "'" + t + "'"
	}
	return strings.Join(quoted, ",")
}

// StreamClassicOps streams successful-tx operations for [from,to] of the given opTypes in dispatcher order, with NO FINAL, so callers that count MUST dedupe on (ledger_seq, tx_hash, op_index).
func StreamClassicOps(ctx context.Context, addr string, from, to uint32, opTypes []string, fn func(ClassicOp) error) error {
	if len(opTypes) == 0 {
		return fmt.Errorf("clickhouse: StreamClassicOps: opTypes is empty")
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// (from, to) twice: the successful-tx derived table's window FIRST (it is
	// spelled first), then the outer scan's. See classicOpsQuery's bind-order note.
	rows, err := conn.Query(ctx, classicOpsQuery(opTypes), from, to, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query classic ops [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()
	return streamClassicOpRows(rows, fn)
}

// classicOpsQuery is StreamClassicOps' SQL, separate so its text is assertable; shape follows sdexOpsQuery: the successful-tx restriction
// is a grace_hash INNER JOIN over a `GROUP BY tx_hash` derived table, never an IN-subquery (blew the 10 GiB budget), BEFORE the outer WHERE
// so the ledger window still prunes stellar.operation_results. FOUR binds, in order: the derived table's from + to (FIRST), then the outer
// window's from + to. opTypes is INTERPOLATED.
func classicOpsQuery(opTypes []string) string {
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
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`, classicOpTypeInList(opTypes))
}

// streamClassicOpRows decodes an open classicOpsQuery result set and invokes fn per row, in ClickHouse's order; split out so it is
// drivable against a stub driver.Rows. fn's error aborts the stream and is returned VERBATIM.
func streamClassicOpRows(rows driver.Rows, fn func(ClassicOp) error) error {
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
			return fmt.Errorf("clickhouse: scan classic op: %w", err)
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

		if err := fn(ClassicOp{
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
