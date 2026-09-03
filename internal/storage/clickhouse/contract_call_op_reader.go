package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ContractCallOp is one InvokeContract operation reconstructed from the lake —
// the op body + ledger context, enough to feed dispatcher.ExtractContractCallTree
// and a source's ContractCallDecoder. Source is the op's source account (the
// decoder uses it for relayer/observer/taker attribution; it does not affect the
// emitted event COUNT, which is what the projection census needs).
type ContractCallOp struct {
	Ledger   uint32
	ClosedAt time.Time
	TxHash   string
	Source   string
	OpIndex  uint32
	Op       xdr.Operation
}

// StreamContractCallOps re-derives the InvokeContract ops that invoke a given
// contract — the projection input for event-less ContractCall sources (band,
// soroswap-router) which have no soroban_events landing zone.
//
// stellar.operations carries no contract_id column, so we can't filter by
// contract in a plain WHERE. Instead we restrict to InvokeHostFunction ops in
// [from,to] and match the contract's raw 32-byte ID as a substring of the
// base64-DECODED body_xdr (the ContractAddress is embedded in the InvokeContract
// args). `contractHex` is the lowercase hex of the strkey-decoded 32-byte
// contract ID. Measured ~2.5s per 100k-ledger window — the full Soroban range
// (~590M invokes) filters in ~minutes, vs days to decode every invoke in Go.
//
// Successful txs only: a failed tx's invoke never executed, so it produced no
// served event — counting it would over-state the census (mirrors the SDEX
// reader's successful-tx restriction). The match is a cheap SUPERSET filter
// (any op whose body merely contains the 32 bytes); the caller's
// ContractCallDecoder.Matches gives the exact contract+function predicate.
func StreamContractCallOps(ctx context.Context, addr, contractHex string, from, to uint32, fn func(ContractCallOp) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, contractCallOpsQuery, from, to, from, to, contractHex)
	if err != nil {
		return fmt.Errorf("clickhouse: query contract-call ops [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()
	return streamContractCallOpRows(rows, fn)
}

// contractCallOpsQuery is StreamContractCallOps' SQL, at package level so its
// text stays independently assertable (StreamContractCallOps dials its own
// connection via openRead — see sdexOpsQuery's note on the same seam).
//
// The successful-tx restriction is a grace_hash INNER JOIN, not an
// IN-subquery: IN builds the whole window's tx-hash set in memory
// (CreatingSetsTransform blew the 10 GiB query budget on a dense
// 250k-ledger window, 2026-07-11). grace_hash spills join buckets
// to disk — the same rationale as StreamSDEXOps/StreamClassicOps.
//
// The FIVE bind parameters are, in order: the successful-tx SUBQUERY's from +
// to (the joined derived table is written FIRST, so its window binds first),
// then the outer scan's from + to, then contractHex. Note this is the reverse
// nesting of sdexOpsQuery, where the subquery trails — which is exactly why the
// order is pinned by test: swapping the pairs is invisible when from/to are the
// same values and catastrophic when a caller ever windows them apart.
const contractCallOpsQuery = `
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account, o.body_xdr
		FROM stellar.operations AS o FINAL
		INNER JOIN (
		    SELECT tx_hash FROM stellar.transactions FINAL
		    WHERE successful = 1 AND ledger_seq BETWEEN ? AND ?
		) AS t ON o.tx_hash = t.tx_hash
		WHERE o.ledger_seq BETWEEN ? AND ?
		  AND o.op_type = 'OperationTypeInvokeHostFunction'
		  AND position(base64Decode(o.body_xdr), unhex(?)) > 0
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32,
		         max_memory_usage = 8000000000, max_bytes_before_external_sort = 2000000000`

// streamContractCallOpRows decodes an open contractCallOpsQuery result set and
// invokes fn once per row, in the order ClickHouse returned them. Split out of
// StreamContractCallOps so the decode + fan-out half is drivable against a stub
// driver.Rows. fn's error aborts the stream and is returned VERBATIM.
func streamContractCallOpRows(rows driver.Rows, fn func(ContractCallOp) error) error {
	for rows.Next() {
		var (
			ledger    uint32
			closeTime time.Time
			txHash    string
			opIndex   uint32
			source    string
			bodyXDR   string
		)
		if err := rows.Scan(&ledger, &closeTime, &txHash, &opIndex, &source, &bodyXDR); err != nil {
			return fmt.Errorf("clickhouse: scan contract-call op: %w", err)
		}
		var body xdr.OperationBody
		if err := xdr.SafeUnmarshalBase64(bodyXDR, &body); err != nil {
			return fmt.Errorf("clickhouse: unmarshal op body (ledger %d tx %s op %d): %w",
				ledger, txHash, opIndex, err)
		}
		if err := fn(ContractCallOp{
			Ledger:   ledger,
			ClosedAt: closeTime.UTC(),
			TxHash:   txHash,
			Source:   source,
			OpIndex:  opIndex,
			Op:       xdr.Operation{Body: body},
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}
