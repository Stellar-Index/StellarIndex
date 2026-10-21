package clickhouse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Op-type counts rollup (deploy/clickhouse/account_op_types.sql Step 6): per lake partition, count() FINAL of account_op_types
// grouped by (account, op_type), swapped in with REPLACE PARTITION. Each partition also holds one sentinel row (account '') whose
// `through` is the ledger the build stopped before and whose `n` is the source partition's active row count at build start, so the
// reader serves counts only where they still match the source and reads everything else from account_op_types.

// opTypePartitionWidth is account_op_types' PARTITION BY intDiv(ledger_seq, 1000000).
const opTypePartitionWidth = 1_000_000

// opTypeRollupTipMargin keeps the open partition's build this many ledgers (~1 h) behind the contiguous tip: an account_op_types row
// can land after its ledger row, and a row below the built-through mark is not re-read until the next build.
const opTypeRollupTipMargin = 720

// ErrOpTypeCountsShrink marks a partition recompute that counted fewer ops than the live partition; the partition is left untouched.
var ErrOpTypeCountsShrink = errors.New("op-type counts recompute is smaller than the live partition")

// opTypeBuilt is one partition's sentinel: counts cover ledger_seq < through, built from srcRows active source rows.
type opTypeBuilt struct {
	through uint32
	srcRows uint64
}

func opTypePartitionEnd(p uint32) uint32 { return (p + 1) * opTypePartitionWidth }

// opTypeCountsPlan says how the reader splits one account's counts: rollup rows for the valid partitions, account_op_types FINAL for
// ledger_seq >= tailFrom and for the stale partitions below it.
type opTypeCountsPlan struct {
	valid    []uint32
	stale    []uint32
	tailFrom uint32
}

// planOpTypeCounts builds the read plan from the sentinels and the source's active row count per partition; ok=false (no usable
// rollup) sends the reader to the plain indexed query. The newest built partition is served below its through mark without a row-count
// match while it is still open: live inserts move its count on every ledger.
func planOpTypeCounts(built map[uint32]opTypeBuilt, src map[uint32]uint64) (opTypeCountsPlan, bool) {
	if len(built) == 0 {
		return opTypeCountsPlan{}, false
	}
	newest := slices.Max(slicesKeys(built))
	head := built[newest]
	var plan opTypeCountsPlan
	headComplete := head.through == opTypePartitionEnd(newest)
	switch {
	case headComplete && head.srcRows != src[newest]:
		plan.stale = append(plan.stale, newest)
		plan.tailFrom = opTypePartitionEnd(newest)
	case head.through > newest*opTypePartitionWidth:
		plan.valid = append(plan.valid, newest)
		plan.tailFrom = head.through
	default:
		plan.tailFrom = newest * opTypePartitionWidth
	}
	for _, p := range slicesKeys(src) {
		if p >= newest {
			continue
		}
		if b, ok := built[p]; ok && b.through == opTypePartitionEnd(p) && b.srcRows == src[p] {
			plan.valid = append(plan.valid, p)
		} else {
			plan.stale = append(plan.stale, p)
		}
	}
	if len(plan.valid) == 0 {
		return opTypeCountsPlan{}, false
	}
	slices.Sort(plan.valid)
	slices.Sort(plan.stale)
	return plan, true
}

func slicesKeys[V any](m map[uint32]V) []uint32 {
	out := make([]uint32, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func joinUint32(ps []uint32) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = strconv.FormatUint(uint64(p), 10)
	}
	return strings.Join(s, ", ")
}

// opTypeCountsRollupQuery renders the plan's split read for one account (bound to both ?). Every literal is a partition number or
// ledger from the plan, never user input. The two arms cover disjoint ledgers, so the outer sum counts each op once.
func opTypeCountsRollupQuery(plan opTypeCountsPlan) string {
	ranges := []string{"ledger_seq >= " + strconv.FormatUint(uint64(plan.tailFrom), 10)}
	for _, p := range plan.stale {
		ranges = append(ranges, fmt.Sprintf("(ledger_seq >= %d AND ledger_seq < %d)", p*opTypePartitionWidth, opTypePartitionEnd(p)))
	}
	return `SELECT op_type, toInt64(sum(c)) AS n FROM (
		SELECT op_type, n AS c FROM stellar.account_op_type_counts
		 WHERE account = ? AND part IN (` + joinUint32(plan.valid) + `)
		UNION ALL
		SELECT op_type, count() AS c FROM stellar.account_op_types FINAL
		 WHERE account = ? AND (` + strings.Join(ranges, " OR ") + `)
		 GROUP BY op_type
	) GROUP BY op_type ORDER BY n DESC` + explorerScanSettings + `, do_not_merge_across_partitions_select_final = 1`
}

// readOpTypeRollupState reads every partition's sentinel and the source's active row count per partition.
func readOpTypeRollupState(ctx context.Context, conn driver.Conn) (map[uint32]opTypeBuilt, map[uint32]uint64, error) {
	built := map[uint32]opTypeBuilt{}
	rows, err := conn.Query(ctx, `SELECT part, through, n FROM stellar.account_op_type_counts WHERE account = ''`)
	if err != nil {
		return nil, nil, fmt.Errorf("clickhouse: op-type counts sentinels: %w", err)
	}
	for rows.Next() {
		var p uint32
		var b opTypeBuilt
		if err := rows.Scan(&p, &b.through, &b.srcRows); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		built[p] = b
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	src, err := opTypeSourceRows(ctx, conn, "")
	return built, src, err
}

// opTypeSourceRows sums active account_op_types rows per partition, or for the one partition id given. Inserts and dedup merges move
// it; a plain merge does not.
func opTypeSourceRows(ctx context.Context, conn driver.Conn, partitionID string) (map[uint32]uint64, error) {
	q := `SELECT toUInt32(partition_id), toUInt64(sum(rows)) FROM system.parts
	       WHERE database = 'stellar' AND table = 'account_op_types' AND active`
	var args []any
	if partitionID != "" {
		q += ` AND partition_id = ?`
		args = append(args, partitionID)
	}
	rows, err := conn.Query(ctx, q+` GROUP BY partition_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: account_op_types partition rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	src := map[uint32]uint64{}
	for rows.Next() {
		var p uint32
		var n uint64
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		src[p] = n
	}
	return src, rows.Err()
}

// opTypeRollupPlanTTL is how long the reader reuses a plan: a backfill into a closed partition is served from stale counts for at
// most this long before its row count stops matching and the partition is read from source.
const opTypeRollupPlanTTL = time.Minute

// opTypeRollupCache holds the reader's current plan.
type opTypeRollupCache struct {
	mu        sync.Mutex
	plan      opTypeCountsPlan
	ok        bool
	fetchedAt time.Time
}

// opTypeCountsPlanFor returns the cached plan, refreshing it once the TTL lapses; an error (table not yet created) means no plan
// until the next refresh, so the caller takes the indexed query.
func (r *ExplorerReader) opTypeCountsPlanFor(ctx context.Context) (opTypeCountsPlan, bool) {
	c := &r.opTypeRollup
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < opTypeRollupPlanTTL {
		return c.plan, c.ok
	}
	built, src, err := readOpTypeRollupState(ctx, r.conn)
	c.fetchedAt = time.Now()
	if err != nil {
		c.plan, c.ok = opTypeCountsPlan{}, false
		return c.plan, c.ok
	}
	c.plan, c.ok = planOpTypeCounts(built, src)
	return c.plan, c.ok
}

// OpTypeRollupPartition is one partition RunOpTypeCountsRollup would (re)build, counting ledger_seq < Through.
type OpTypeRollupPartition struct {
	Part    uint32
	Through uint32
	Reason  string
}

// PlanOpTypeCountsRollup lists the partitions a run rebuilds: closed partitions whose counts are missing, incomplete or no longer match
// the source (newest first, at most maxClosed, 0 = all), then the open partition through the contiguous tip less a margin.
func PlanOpTypeCountsRollup(ctx context.Context, addr string, maxClosed int) ([]OpTypeRollupPartition, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	built, src, err := readOpTypeRollupState(ctx, conn)
	if err != nil {
		return nil, err
	}
	if len(src) == 0 {
		return nil, nil
	}
	head := slices.Max(slicesKeys(src))
	tip, err := contiguousWatermarkOn(ctx, conn, head*opTypePartitionWidth)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: op-type counts contiguous tip: %w", err)
	}
	return opTypeRollupWork(built, src, head, tip, maxClosed), nil
}

// opTypeRollupWork is PlanOpTypeCountsRollup's decision, split out for tests. tip is the contiguous watermark from head's first
// ledger (below it when that ledger is missing).
func opTypeRollupWork(built map[uint32]opTypeBuilt, src map[uint32]uint64, head, tip uint32, maxClosed int) []OpTypeRollupPartition {
	var work []OpTypeRollupPartition
	parts := slicesKeys(src)
	slices.Sort(parts)
	slices.Reverse(parts)
	for _, p := range parts {
		if p >= head {
			continue
		}
		b, ok := built[p]
		var reason string
		switch {
		case !ok:
			reason = "not built"
		case b.through != opTypePartitionEnd(p):
			reason = "built through " + strconv.FormatUint(uint64(b.through), 10)
		case b.srcRows != src[p]:
			reason = fmt.Sprintf("source rows %d, built from %d", src[p], b.srcRows)
		default:
			continue
		}
		if maxClosed > 0 && len(work) == maxClosed {
			break
		}
		work = append(work, OpTypeRollupPartition{Part: p, Through: opTypePartitionEnd(p), Reason: reason})
	}
	start := head * opTypePartitionWidth
	if tip >= start+opTypeRollupTipMargin {
		through := tip + 1 - opTypeRollupTipMargin
		if b, ok := built[head]; !ok || through > b.through {
			work = append(work, OpTypeRollupPartition{Part: head, Through: through, Reason: "open partition"})
		}
	}
	return work
}

// RunOpTypeCountsPartition recomputes one partition below through and swaps it in atomically (private staging table, idempotent); a
// recompute counting fewer ops than the live partition is refused with ErrOpTypeCountsShrink unless shrinkOK.
func RunOpTypeCountsPartition(ctx context.Context, addr string, w OpTypeRollupPartition, shrinkOK bool, logf func(string, ...any)) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return runOpTypeCountsPartitionConn(ctx, conn, w, shrinkOK, logf)
}

func runOpTypeCountsPartitionConn(ctx context.Context, conn driver.Conn, w OpTypeRollupPartition, shrinkOK bool, logf func(string, ...any)) error {
	start := time.Now()
	pid := strconv.FormatUint(uint64(w.Part), 10)
	// Read the fingerprint BEFORE the compute: a row landing mid-build then shows as a mismatch and the next run rebuilds.
	src, err := opTypeSourceRows(ctx, conn, pid)
	if err != nil {
		return err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("clickhouse: op-type counts staging name: %w", err)
	}
	staging := "account_op_type_counts_staging_" + hex.EncodeToString(b[:])
	if err := conn.Exec(ctx, "CREATE TABLE stellar."+staging+" AS stellar.account_op_type_counts"); err != nil {
		return fmt.Errorf("clickhouse: op-type counts staging create %s: %w", staging, err)
	}
	defer func() { //nolint:contextcheck // detached cleanup ctx: must fire even when the parent ctx is already cancelled
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if derr := conn.Exec(dropCtx, "DROP TABLE IF EXISTS stellar."+staging); derr != nil {
			logf("partition %s staging cleanup %s failed: %v", pid, staging, derr)
		}
	}()

	if err := conn.Exec(ctx, `INSERT INTO stellar.`+staging+` (part, account, op_type, n, through)
		SELECT toUInt32(?), account, op_type, count(), toUInt32(?)
		  FROM stellar.account_op_types FINAL
		 WHERE ledger_seq >= ? AND ledger_seq < ?
		 GROUP BY account, op_type
		SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_bytes_before_external_group_by = 4000000000,
		         max_execution_time = 1800, do_not_merge_across_partitions_select_final = 1`,
		w.Part, w.Through, w.Part*opTypePartitionWidth, w.Through); err != nil {
		return fmt.Errorf("clickhouse: op-type counts partition %s compute: %w", pid, err)
	}
	if err := conn.Exec(ctx, `INSERT INTO stellar.`+staging+` (part, account, op_type, n, through) VALUES (?, '', '', ?, ?)`,
		w.Part, src[w.Part], w.Through); err != nil {
		return fmt.Errorf("clickhouse: op-type counts partition %s sentinel: %w", pid, err)
	}
	if !shrinkOK {
		var newOps, liveOps uint64
		if err := conn.QueryRow(ctx, `SELECT
			(SELECT toUInt64(sum(n)) FROM stellar.`+staging+` WHERE account != ''),
			(SELECT toUInt64(sum(n)) FROM stellar.account_op_type_counts WHERE part = ? AND account != '')`, w.Part).
			Scan(&newOps, &liveOps); err != nil {
			return fmt.Errorf("clickhouse: op-type counts partition %s size check: %w", pid, err)
		}
		if newOps < liveOps {
			return fmt.Errorf("%w: partition %s recomputed %d op(s), live has %d — the lake lost rows under it; pass -shrink-ok only if intended",
				ErrOpTypeCountsShrink, pid, newOps, liveOps)
		}
	}
	if err := conn.Exec(ctx, "ALTER TABLE stellar.account_op_type_counts REPLACE PARTITION ID '"+pid+"' FROM stellar."+staging); err != nil {
		return fmt.Errorf("clickhouse: op-type counts partition %s replace: %w", pid, err)
	}
	logf("partition %s through %d (%s) done in %s", pid, w.Through, w.Reason, time.Since(start).Round(time.Second))
	return nil
}

// accountPartitionsClause narrows an account-keyed read of a table partitioned like account_op_types to the partitions the account has
// ops in: one granule per part is otherwise read from all ~200 parts, ~1.5M rows for an account with a handful of ops. The clause keeps
// every stale partition and everything at or past the plan's tail. "" (marker or plan missing, read error) means read unfiltered.
// It is only as complete as account_op_types: re-deriving operation_participants must re-run its participant arm (account_op_types.sql).
func (r *ExplorerReader) accountPartitionsClause(ctx context.Context, account string) string {
	if !r.accountOpTypesCovered(ctx) {
		return ""
	}
	plan, ok := r.opTypeCountsPlanFor(ctx)
	if !ok {
		return ""
	}
	parts, err := queryKeys(ctx, r.conn, `SELECT DISTINCT part FROM stellar.account_op_type_counts
		WHERE account = ? AND part IN (`+joinUint32(plan.valid)+`)`+explorerScanSettings,
		[]any{account}, func(rows driver.Rows) (uint32, error) {
			var p uint32
			err := rows.Scan(&p)
			return p, err
		})
	if err != nil {
		return ""
	}
	parts = append(parts, plan.stale...)
	clause := " AND (ledger_seq >= " + strconv.FormatUint(uint64(plan.tailFrom), 10)
	if len(parts) > 0 {
		slices.Sort(parts)
		clause += " OR intDiv(ledger_seq, " + strconv.Itoa(opTypePartitionWidth) + ") IN (" + joinUint32(parts) + ")"
	}
	return clause + ")"
}
