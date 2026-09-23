package clickhouse

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Backfill targets for the instance timeline. The v2 name exists only while
// deploy/clickhouse/contract_instance_changes_tx_key.sql rebuilds an
// old-key table beside the live one.
const (
	ContractInstanceChangesTable   = "contract_instance_changes"
	ContractInstanceChangesV2Table = "contract_instance_changes_v2"
)

// contractInstanceBackfillQuery fills one ledger window of the named
// instance-timeline table from ledger_entry_changes history — the same
// fixed-offset extraction as contract_instance_changes_mv
// (deploy/clickhouse/contract_instance_changes.sql; keep the two in
// lockstep). Windowed on ledger_seq (the source's primary-key prefix,
// so each window prunes) and bounded like the sibling backfills; the
// target RMT collapses re-runs of overlapping windows, so the backfill
// is idempotent and resumable. table is one of the two constants above,
// never caller text (BackfillContractInstanceChangesInto checks).
func contractInstanceBackfillQuery(table string) string {
	return fmt.Sprintf(`
	INSERT INTO stellar.%s
		(contract_hash, ledger_seq, tx_hash, change_index, intra_ledger_seq, close_time, is_sac, wasm_hash)
	SELECT
		lower(hex(substring(tryBase64Decode(key_xdr), 9, 32))),
		ledger_seq,
		tx_hash,
		change_index,
		intra_ledger_seq,
		close_time,
		toUInt8(substring(tryBase64Decode(entry_xdr), 61, 4) = unhex('00000001')),
		if(substring(tryBase64Decode(entry_xdr), 61, 4) = unhex('00000000'),
		   lower(hex(substring(tryBase64Decode(entry_xdr), 65, 32))), '')
	FROM stellar.ledger_entry_changes
	WHERE ledger_seq BETWEEN ? AND ?
	  AND entry_type = 'contract_data'
	  AND length(key_xdr) = 64
	  AND substring(tryBase64Decode(key_xdr), 1, 8) = unhex('0000000600000001')
	  AND substring(tryBase64Decode(key_xdr), 41, 4) = unhex('00000014')
	  AND entry_xdr != ''
	  AND substring(tryBase64Decode(entry_xdr), 57, 4) = unhex('00000013')
	SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_execution_time = 1800`, table)
}

// BackfillContractInstanceChanges fills stellar.contract_instance_changes
// (the per-contract instance-executable timeline behind the explorer's
// code-history + wasm-hash reads) from ledger_entry_changes history in
// windowed, resumable INSERT…SELECT chunks. Same operator contract as
// BackfillContractActiveLedgers: serialize on r1, run under
// run-heavy-job.sh, resume with the printed -from on interrupt.
func BackfillContractInstanceChanges(ctx context.Context, addr string, from, to, window uint32, logf func(format string, args ...any)) error {
	return BackfillContractInstanceChangesInto(ctx, addr, ContractInstanceChangesTable, from, to, window, logf)
}

// BackfillContractInstanceChangesInto is BackfillContractInstanceChanges
// against an explicit target: ContractInstanceChangesTable, or
// ContractInstanceChangesV2Table during the key-rebuild cut-over.
func BackfillContractInstanceChangesInto(ctx context.Context, addr, table string, from, to, window uint32, logf func(format string, args ...any)) error {
	if table != ContractInstanceChangesTable && table != ContractInstanceChangesV2Table {
		return fmt.Errorf("clickhouse: instance-changes backfill: table %q is not %s or %s",
			table, ContractInstanceChangesTable, ContractInstanceChangesV2Table)
	}
	if from == 0 || to < from || window == 0 {
		return fmt.Errorf("clickhouse: instance-changes backfill: need 0 < from <= to and window > 0 (got from=%d to=%d window=%d)", from, to, window)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	q := contractInstanceBackfillQuery(table)
	return runWindowedBackfill(ctx, conn, from, to, window, "instance-changes",
		func(ctx context.Context, conn driver.Conn, lo, hi uint32) error {
			return conn.Exec(ctx, q, lo, hi)
		}, logf)
}
