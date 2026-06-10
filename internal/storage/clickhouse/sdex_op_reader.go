package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

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
// are op-derived (operations + operation_results), NOT event-derived, so they
// don't flow through StreamContractEvents. It reads stellar.operations joined
// to stellar.operation_results on (ledger_seq, tx_hash, op_index) for
// [from,to] inclusive, restricted to the trade-bearing op types AND to
// SUCCESSFUL transactions, reconstructs op.Body + the OperationResult from the
// retained XDR blobs, and invokes fn for each in dispatcher emission order.
//
// The successful-tx restriction matters: a failed tx's op results can still
// carry success codes + claim atoms for ops that ran before the failing op,
// but those trades were rolled back and never happened. dispatcher.CensusLedger
// and internal/sources/sdex both count trades only in successful txs, so the
// re-derivation must too — otherwise it over-counts phantom fills.
//
// FINAL dedups ReplacingMergeTree parts; join_algorithm=full_sorting_merge
// keeps the operations⋈operation_results join (two large co-sorted inputs)
// memory-bounded. Callers re-deriving all history should window [from,to] per
// partition so the result set + the successful-tx IN-set stay bounded.
func StreamSDEXOps(ctx context.Context, addr string, from, to uint32, fn func(SDEXOp) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	query := fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM stellar.operations AS o FINAL
		INNER JOIN stellar.operation_results AS r FINAL
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		WHERE o.ledger_seq BETWEEN ? AND ?
		  AND o.op_type IN (%s)
		  AND o.tx_hash IN (
		      SELECT tx_hash FROM stellar.transactions FINAL
		      WHERE successful = 1 AND ledger_seq BETWEEN ? AND ?
		  )
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'full_sorting_merge'`, tradeOpTypeInList())
	rows, err := conn.Query(ctx, query, from, to, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query sdex ops [%d,%d]: %w", from, to, err)
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
