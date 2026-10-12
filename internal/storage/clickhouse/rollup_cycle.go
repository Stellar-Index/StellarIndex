package clickhouse

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// rollupLedgerWindow is the ledger span ONE walked statement covers: a single lake partition. Both archives a cycle reads are PARTITION
// BY intDiv(<ledger column>, 1000000) (deploy/clickhouse/tier1_schema.sql), so a window predicate prunes to one partition's parts.
const rollupLedgerWindow = 1_000_000

// rollupWalkSettings is a walked step's settings: boundedScanSettings plus a per-WINDOW (not per-cycle) execution cap; 600 s leaves >20x
// headroom over the widest window yet fails one wedged window fast.
const rollupWalkSettings = boundedScanSettings + ", max_execution_time = 600"

// rollupStep is one statement of a recompute cycle. A step with walk set is a TEMPLATE carrying one window's inclusive ledger bounds as
// bind parameters, run once per window by runRollupCycle; every other step runs once, in order.
type rollupStep struct {
	sql  string
	walk bool
	// windowBinds is how many `BETWEEN ? AND ?` pairs sql carries: one per SOURCE TABLE, since a
	// join condition prunes neither side's partitions and an unbounded side costs the whole
	// archive per window. The same window is bound into every pair. Zero reads as one.
	// TestRollupCyclesDeclareTheirWindowBinds pins the count against the placeholders present.
	windowBinds int
}

// windowBindArgs repeats one window's inclusive bounds once per
// `BETWEEN ? AND ?` pair the template carries.
func windowBindArgs(lo, hi uint32, pairs int) []any {
	if pairs < 1 {
		pairs = 1
	}
	args := make([]any, 0, 2*pairs)
	for range pairs {
		args = append(args, lo, hi)
	}
	return args
}

// runRollupCycle runs each step in order, repeating walked steps over consecutive rollupLedgerWindow windows from ledger 1 to the lake
// tip. One statement over the whole archive holds a dedupe table sized by ALL of history plus a build side sized by every account;
// walking makes the population per-window, and the account join runs ONCE against the narrow working table. The walk writes a working
// table, not staging, so an interrupted cycle leaves the live board untouched; the single EXCHANGE is the one moment anything becomes
// visible.
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

// rollupExecer is the slice of driver.Conn a cycle uses, so the step loop (where the walk lives) is drivable without a ClickHouse
// connection.
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
			if xerr := conn.Exec(ctx, step.sql, windowBindArgs(lo, hi, step.windowBinds)...); xerr != nil {
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

// lakeTipLedger reads the highest ledger_seq in the lake's ledgers table. The walk's bound is the tip, not each source table's own max: a
// lagging source just yields empty windows, which partition pruning answers without reading a part.
func lakeTipLedger(ctx context.Context, conn driver.Conn) (uint32, error) {
	var hi uint64
	if err := conn.QueryRow(ctx, `SELECT toUInt64(max(ledger_seq)) FROM stellar.ledgers`).Scan(&hi); err != nil {
		return 0, fmt.Errorf("clickhouse: max ledger: %w", err)
	}
	return uint32(hi), nil
}
