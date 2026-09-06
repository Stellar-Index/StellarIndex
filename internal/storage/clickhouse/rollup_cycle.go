package clickhouse

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// rollupLedgerWindow is the ledger span ONE walked statement covers: a
// single lake partition. Both archives a rollup cycle reads are
// PARTITION BY intDiv(<their ledger column>, 1000000)
// (deploy/clickhouse/tier1_schema.sql), so a window predicate prunes to
// exactly one partition's parts. Same constant and same discipline as
// the recognition shape scan (recognitionScanWindow).
const rollupLedgerWindow = 1_000_000

// rollupWalkSettings is the per-statement settings clause for a walked
// step. The memory shape is the house full-history scan class
// (boundedScanSettings: two concurrent part streams, an 8 GiB tracked
// ceiling, spill at 4 GiB) plus a per-WINDOW execution cap.
//
// The cap is per window, not per cycle: the widest window measured on r1
// 2026-09-06 took 24.7 s (stellar.operations partition 62, 819.5 M rows,
// 54.86 GiB read, max_threads=2), so 600 s leaves better than 20x
// headroom for a contended box while still failing one wedged window
// fast instead of spending the unit's whole TimeoutStartSec budget on it.
const rollupWalkSettings = boundedScanSettings + ", max_execution_time = 600"

// rollupStep is one statement of a recompute cycle.
//
// A step with walk set is a TEMPLATE rather than a statement: it carries
// two bind parameters — the inclusive ledger bounds of ONE window — and
// runRollupCycle executes it once per window instead of once. Every
// other step runs exactly once, in order.
type rollupStep struct {
	sql  string
	walk bool
}

// runRollupCycle executes a recompute cycle: each step in order, walked
// steps repeated over consecutive rollupLedgerWindow-wide ledger windows
// from ledger 1 up to the lake tip.
//
// WHY THE WALK IS THE SHAPE AND NOT A BIGGER CEILING. A single statement
// that aggregates a whole archive holds two things at once whose sizes
// are set by different populations: the dedupe hash table, one state per
// surviving source row over ALL of history, and — where the statement
// also joins — the join's build side, one row per account that exists.
// Neither shrinks on its own, so the pair outgrows any fixed ceiling as
// the chain grows, and the ceiling only decides which cycle is the one
// that fails. Measured on r1 2026-09-06, the creator board's single
// statement read all 10,309,146,441 movement rows (315.97 GiB) and died
// at 8.12 GiB in FillingRightJoinSide with its dedupe already spilled to
// 32 external parts — the two consumers summed past the budget.
//
// Walking makes the first population a per-partition one: the widest
// creation window measured holds 1,027,707 rows at 701.17 MiB, and a
// window would need roughly twelve times that to reach the same ceiling.
// The second population is taken out of the walk entirely — the join
// runs ONCE, against the narrow working table the walk wrote, so it is
// paid once per cycle rather than once per window.
//
// The atomic-swap guarantee is unchanged and is why the walk writes to a
// working table rather than to staging: nothing touches a staging arm
// until the walk has finished, so a cycle interrupted mid-walk leaves
// the live board exactly as the previous cycle left it, and the single
// EXCHANGE stays the one moment anything becomes visible.
func runRollupCycle(ctx context.Context, addr, label string, steps []rollupStep, logf func(format string, args ...any)) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	tip, err := lakeTipLedger(ctx, conn)
	if err != nil {
		return fmt.Errorf("clickhouse: %s: %w", label, err)
	}
	return runRollupSteps(ctx, conn, tip, label, steps, logf)
}

// rollupExecer is the slice of driver.Conn a cycle uses. Naming it keeps
// the step loop — which is where the walk lives — drivable without a
// ClickHouse connection.
type rollupExecer interface {
	Exec(ctx context.Context, query string, args ...any) error
}

// runRollupSteps runs the cycle's steps against an already-resolved tip.
func runRollupSteps(ctx context.Context, conn rollupExecer, tip uint32, label string, steps []rollupStep, logf func(format string, args ...any)) error {
	// A zero tip means the lake reported no ledgers. Walking it yields
	// no windows, so the cycle would build an empty board and EXCHANGE
	// it over a populated one. Refusing leaves the previous board served.
	if tip == 0 {
		return fmt.Errorf("clickhouse: %s: lake tip is 0, refusing to swap an empty board over the live one", label)
	}
	for i, step := range steps {
		if !step.walk {
			if err := conn.Exec(ctx, step.sql); err != nil {
				return fmt.Errorf("clickhouse: %s step %d/%d: %w", label, i+1, len(steps), err)
			}
			logf("step %d/%d done", i+1, len(steps))
			continue
		}
		// Count the windows the same way they are then walked, so the
		// completion line cannot claim a coverage the loop did not run.
		want := 0
		_ = forEachLedgerWindow(1, tip, rollupLedgerWindow, func(uint32, uint32) error {
			want++
			return nil
		})
		done := 0
		if err := forEachLedgerWindow(1, tip, rollupLedgerWindow, func(lo, hi uint32) error {
			if xerr := conn.Exec(ctx, step.sql, lo, hi); xerr != nil {
				return fmt.Errorf("clickhouse: %s step %d/%d window [%d,%d]: %w",
					label, i+1, len(steps), lo, hi, xerr)
			}
			done++
			logf("step %d/%d window %d/%d [%d,%d] done", i+1, len(steps), done, want, lo, hi)
			return nil
		}); err != nil {
			return err
		}
		logf("step %d/%d done: %d of %d ledger windows through tip %d", i+1, len(steps), done, want, tip)
	}
	return nil
}

// lakeTipLedger reads the highest ledger_seq in the lake's ledgers table
// on an already-open connection. The walk's upper bound is the tip
// itself rather than each source table's own max: a source that lags the
// tip simply yields empty windows, which partition pruning answers
// without reading a part.
func lakeTipLedger(ctx context.Context, conn driver.Conn) (uint32, error) {
	var hi uint64
	if err := conn.QueryRow(ctx, `SELECT toUInt64(max(ledger_seq)) FROM stellar.ledgers`).Scan(&hi); err != nil {
		return 0, fmt.Errorf("clickhouse: max ledger: %w", err)
	}
	return uint32(hi), nil
}
