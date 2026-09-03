package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// contractInstanceBackfillQuery fills one ledger window of
// stellar.contract_instance_changes from ledger_entry_changes history —
// the same fixed-offset extraction as contract_instance_changes_mv
// (deploy/clickhouse/contract_instance_changes.sql; keep the two in
// lockstep). Windowed on ledger_seq (the source's primary-key prefix,
// so each window prunes) and bounded like the sibling backfills; the
// target RMT collapses re-runs of overlapping windows, so the backfill
// is idempotent and resumable.
const contractInstanceBackfillQuery = `
	INSERT INTO stellar.contract_instance_changes
		(contract_hash, ledger_seq, change_index, close_time, is_sac, wasm_hash)
	SELECT
		lower(hex(substring(tryBase64Decode(key_xdr), 9, 32))),
		ledger_seq,
		change_index,
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
	SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_execution_time = 1800`

// BackfillContractInstanceChanges fills stellar.contract_instance_changes
// (the per-contract instance-executable timeline behind the explorer's
// code-history + wasm-hash reads) from ledger_entry_changes history in
// windowed, resumable INSERT…SELECT chunks. Same operator contract as
// BackfillContractActiveLedgers: serialize on r1, run under
// run-heavy-job.sh, resume with the printed -from on interrupt.
func BackfillContractInstanceChanges(ctx context.Context, addr string, from, to, window uint32, logf func(format string, args ...any)) error {
	if from == 0 || to < from || window == 0 {
		return fmt.Errorf("clickhouse: instance-changes backfill: need 0 < from <= to and window > 0 (got from=%d to=%d window=%d)", from, to, window)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	start := time.Now()
	for lo := from; ; {
		hi := ledgerWindowHi(lo, to, window)
		wStart := time.Now()
		if err := conn.Exec(ctx, contractInstanceBackfillQuery, lo, hi); err != nil {
			return fmt.Errorf("clickhouse: instance-changes window [%d,%d]: %w — resume with -from %d", lo, hi, err, lo)
		}
		logf("window [%d,%d] done in %s (total %s; resume point -from %d)",
			lo, hi, time.Since(wStart).Round(time.Second), time.Since(start).Round(time.Second), hi+1)
		if hi >= to {
			return nil
		}
		lo = hi + 1
	}
}
