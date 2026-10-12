package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ContractCallOp is one InvokeContract operation reconstructed from the lake (op body plus ledger context); Source is for attribution and does not affect the emitted event COUNT.
type ContractCallOp struct {
	Ledger   uint32
	ClosedAt time.Time
	TxHash   string
	Source   string
	OpIndex  uint32
	Op       xdr.Operation
}

// StreamContractCallOps re-derives successful InvokeContract ops for one contract (input for event-less ContractCall sources) via a cheap SUPERSET substring match; ContractCallDecoder.Matches is the exact predicate.
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

// contractCallOpsQuery is StreamContractCallOps' SQL, package-level so its text is assertable (see sdexOpsQuery). The successful-tx
// restriction is a grace_hash INNER JOIN, not an IN-subquery (which blew the 10 GiB budget on a dense window). The FIVE binds are, in
// order: the successful-tx SUBQUERY's from + to (written FIRST), the outer scan's from + to, then contractHex. A test pins the order:
// swapping the pairs is invisible when from/to match and catastrophic when a caller windows them apart.
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

// streamContractCallOpRows decodes an open contractCallOpsQuery result set and invokes fn per row, in ClickHouse's order; split out so it
// is drivable against a stub driver.Rows. fn's error aborts the stream and is returned VERBATIM.
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
