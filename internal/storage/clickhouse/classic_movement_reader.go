package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// ClassicOp is one lake-derived classic operation eligible for
// pre-P23 movement reconstruction (ADR-0047): op body + result +
// tx-level context, enough to feed a classicmovements-phase decoder
// via dispatcher.OpContext. Source is the op's resolved effective
// source account (op override else tx source — stellar.operations.
// source_account is already resolved this way at extract time, see
// internal/storage/clickhouse/extract.go::extractOps) — the caller
// wires it straight into OpContext.TxSource, exactly as ch-rebuild's
// SDEX pass does with StreamSDEXOps's equivalent field, and leaves
// OpContext.OpSource empty.
type ClassicOp struct {
	Ledger   uint32
	ClosedAt time.Time
	TxHash   string
	Source   string
	OpIndex  uint32
	Op       xdr.Operation
	OpResult xdr.OperationResult
}

// classicOpTypeInList renders an op-type slice as a SQL IN list:
// 'a','b',.... opTypes MUST be compile-time constants (e.g.
// classicmovements.SupportedOpTypes()), never derived from user
// input — interpolated directly into the query, mirroring
// tradeOpTypeInList's identical contract in sdex_op_reader.go.
func classicOpTypeInList(opTypes []string) string {
	quoted := make([]string, len(opTypes))
	for i, t := range opTypes {
		quoted[i] = "'" + t + "'"
	}
	return strings.Join(quoted, ",")
}

// StreamClassicOps reads stellar.operations JOIN operation_results
// for [from,to] inclusive, restricted to opTypes
// (xdr.OperationType.String() values, e.g. "OperationTypePayment")
// AND successful transactions, reconstructs op.Body + the
// OperationResult from the retained XDR blobs, and invokes fn for
// each in dispatcher emission order.
//
// This is the SHARED ADR-0047 lake-read harness every phase of
// pre-P23 classic-movement reconstruction reuses: Phase 1
// (internal/sources/classicmovements) calls it with
// classicmovements.SupportedOpTypes(); Phases 2-4 extend the
// CALLER's opTypes list, not this function — no reader change
// needed as the reconstruction's op-type scope grows.
//
// Mirrors StreamSDEXOps's shape and its NO-FINAL / grace_hash-join
// rationale (see that function's doc comment for the full incident
// history): duplicate rows from unmerged ReplacingMergeTree parts
// are harmless here too — every classic_movements writer is
// ON CONFLICT DO NOTHING idempotent (migration 0105's PK). The
// successful-tx restriction matters for the same reason it does for
// SDEX: a failed tx's op results can still carry stale/partial data
// for ops that ran before the failing one, but those movements were
// rolled back and never happened.
func StreamClassicOps(ctx context.Context, addr string, from, to uint32, opTypes []string, fn func(ClassicOp) error) error {
	if len(opTypes) == 0 {
		return fmt.Errorf("clickhouse: StreamClassicOps: opTypes is empty")
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	query := fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM stellar.operations AS o
		INNER JOIN stellar.operation_results AS r
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		WHERE o.ledger_seq BETWEEN ? AND ?
		  AND o.op_type IN (%s)
		  AND o.tx_hash IN (
		      SELECT tx_hash FROM stellar.transactions
		      WHERE successful = 1 AND ledger_seq BETWEEN ? AND ?
		  )
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`, classicOpTypeInList(opTypes))
	rows, err := conn.Query(ctx, query, from, to, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query classic ops [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()

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
