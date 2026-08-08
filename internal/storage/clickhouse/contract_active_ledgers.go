package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// contractActiveLedgersBackfillQuery fills one ledger window of
// stellar.contract_active_ledgers from contract_events history. GROUP BY
// (not DISTINCT) so the collapse can SPILL: a dense 5M-ledger window's
// distinct set blew the 8 GiB limit on the first r1 run (Code 241 at
// window [40M,45M]) — DISTINCT can't use external aggregation, GROUP BY
// with max_bytes_before_external_group_by can. The target's
// ReplacingMergeTree collapses re-runs of overlapping windows, so the
// backfill is idempotent and resumable (see the DDL's replay-safety note —
// this index deliberately carries no counts).
const contractActiveLedgersBackfillQuery = `
	INSERT INTO stellar.contract_active_ledgers (contract_id, ledger_seq, close_time)
	SELECT contract_id, ledger_seq, any(close_time)
	FROM stellar.contract_events
	WHERE ledger_seq BETWEEN ? AND ?
	GROUP BY contract_id, ledger_seq
	SETTINGS max_threads = 4, max_memory_usage = 8589934592,
	         max_bytes_before_external_group_by = 4000000000, max_execution_time = 1800`

// BackfillContractActiveLedgers fills stellar.contract_active_ledgers (the
// per-(contract, ledger) activity index behind ContractEventsRecent's
// quiet-contract bound, deploy/clickhouse/contract_active_ledgers.sql)
// from contract_events history in windowed, resumable INSERT…SELECT
// chunks. Same operator contract as BackfillTxHashIndex: serialize on r1,
// run under run-heavy-job.sh, resume with the printed -from on interrupt.
func BackfillContractActiveLedgers(ctx context.Context, addr string, from, to, window uint32, logf func(format string, args ...any)) error {
	if from == 0 || to < from || window == 0 {
		return fmt.Errorf("clickhouse: contract-ledgers backfill: need 0 < from <= to and window > 0 (got from=%d to=%d window=%d)", from, to, window)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	start := time.Now()
	for lo := from; ; {
		hi := to
		if rem := to - lo; rem >= window {
			hi = lo + window - 1
		}
		wStart := time.Now()
		if err := conn.Exec(ctx, contractActiveLedgersBackfillQuery, lo, hi); err != nil {
			return fmt.Errorf("clickhouse: contract-ledgers window [%d,%d]: %w — resume with -from %d", lo, hi, err, lo)
		}
		logf("window [%d,%d] done in %s (total %s; resume point -from %d)",
			lo, hi, time.Since(wStart).Round(time.Second), time.Since(start).Round(time.Second), hi+1)
		if hi >= to {
			return nil
		}
		lo = hi + 1
	}
}
