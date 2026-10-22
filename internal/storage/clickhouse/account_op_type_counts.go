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

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Per-account counts rollups (deploy/clickhouse/account_op_types.sql Step 6): per lake partition, count() FINAL of a source table
// grouped by (account, group column), swapped in with REPLACE PARTITION. Each partition also holds one sentinel row (account '') whose
// `through` is the ledger the build stopped before and whose `n` is the source partition's active row count at build start, so the
// reader trusts a partition only while it still matches the source and reads everything else from the source.

// CountsRollup names one rollup: target(part, account, <group>, n, through) counts source rows per account and group.
type CountsRollup struct {
	Name    string
	table   string
	source  string
	ledger  string
	account string
	group   string
}

var (
	// OpTypeCountsRollup serves the activity panel's op-type counts and the transactions/operations partition directory.
	OpTypeCountsRollup = &CountsRollup{
		Name: "op-types", table: "account_op_type_counts", source: "account_op_types",
		ledger: "ledger_seq", account: "account", group: "op_type",
	}
	// MovementCountsRollup is the movements listing's partition directory. account_op_types cannot be: CAP-67 movements land on
	// accounts that are no operation's participant.
	MovementCountsRollup = &CountsRollup{
		Name: "movements", table: "account_movement_counts", source: "account_movements",
		ledger: "ledger", account: "address", group: "movement_kind",
	}
	// CountsRollups is every rollup ch-optype-rollup maintains.
	CountsRollups = []*CountsRollup{OpTypeCountsRollup, MovementCountsRollup}
)

// opTypePartitionWidth is every source's PARTITION BY intDiv(<ledger>, 1000000).
const opTypePartitionWidth = 1_000_000

// opTypeRollupTipMargin keeps the open partition's build this many ledgers (~1 h) behind the contiguous tip: a source row can land
// after its ledger row, and a row below the built-through mark is not re-read until the next build.
const opTypeRollupTipMargin = 720

// ErrCountsShrink marks a partition recompute that counted fewer rows than the live partition; the partition is left untouched.
var ErrCountsShrink = errors.New("counts rollup recompute is smaller than the live partition")

// opTypeBuilt is one partition's sentinel: counts cover ledger_seq < through, built from srcRows active source rows.
type opTypeBuilt struct {
	through uint32
	srcRows uint64
}

func opTypePartitionEnd(p uint32) uint32 { return (p + 1) * opTypePartitionWidth }

// opTypeCountsPlan says how the reader splits one account's counts: rollup rows for the valid partitions, the source FINAL for
// ledger >= tailFrom and for the stale partitions below it.
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

// readState reads every partition's sentinel and the source's active row count per partition.
func (rc *CountsRollup) readState(ctx context.Context, conn driver.Conn) (map[uint32]opTypeBuilt, map[uint32]uint64, error) {
	built := map[uint32]opTypeBuilt{}
	rows, err := conn.Query(ctx, `SELECT part, through, n FROM stellar.`+rc.table+` WHERE account = ''`)
	if err != nil {
		return nil, nil, fmt.Errorf("clickhouse: %s sentinels: %w", rc.table, err)
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
	src, err := rc.sourceRows(ctx, conn, "")
	return built, src, err
}

// sourceRows sums the source's active rows per partition, or for the one partition id given. Inserts and dedup merges move it; a
// plain merge does not.
func (rc *CountsRollup) sourceRows(ctx context.Context, conn driver.Conn, partitionID string) (map[uint32]uint64, error) {
	q := `SELECT toUInt32(partition_id), toUInt64(sum(rows)) FROM system.parts
	       WHERE database = 'stellar' AND table = ? AND active`
	args := []any{rc.source}
	if partitionID != "" {
		q += ` AND partition_id = ?`
		args = append(args, partitionID)
	}
	rows, err := conn.Query(ctx, q+` GROUP BY partition_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %s partition rows: %w", rc.source, err)
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

// planFor returns rc's cached plan, refreshing it once the TTL lapses; an error (table not yet created) means no plan until the next
// refresh, so the caller reads the source. Each refresh exports the plan's health: the fallback is a full scan, which would otherwise be silent.
func (r *ExplorerReader) planFor(ctx context.Context, rc *CountsRollup) (opTypeCountsPlan, bool) {
	c := &r.opTypeRollup
	if rc == MovementCountsRollup {
		c = &r.movementRollup
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < opTypeRollupPlanTTL {
		return c.plan, c.ok
	}
	built, src, err := rc.readState(ctx, r.conn)
	c.fetchedAt = time.Now()
	if err != nil {
		c.plan, c.ok = opTypeCountsPlan{}, false
	} else {
		c.plan, c.ok = planOpTypeCounts(built, src)
	}
	ok := 0.0
	if c.ok {
		ok = 1
	}
	obs.CountsRollupPlanOK.WithLabelValues(rc.Name).Set(ok)
	obs.CountsRollupStalePartitions.WithLabelValues(rc.Name).Set(float64(len(c.plan.stale)))
	return c.plan, c.ok
}

// OpTypeRollupPartition is one partition a rollup run would (re)build, counting ledgers below Through.
type OpTypeRollupPartition struct {
	Part    uint32
	Through uint32
	Reason  string
}

// Plan lists the partitions a run rebuilds: closed partitions whose counts are missing, incomplete or no longer match the source
// (newest first, at most maxClosed, 0 = all), then the open partition through the contiguous tip less a margin.
func (rc *CountsRollup) Plan(ctx context.Context, addr string, maxClosed int) ([]OpTypeRollupPartition, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	built, src, err := rc.readState(ctx, conn)
	if err != nil {
		return nil, err
	}
	if len(src) == 0 {
		return nil, nil
	}
	head := slices.Max(slicesKeys(src))
	tip, err := contiguousWatermarkOn(ctx, conn, head*opTypePartitionWidth)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %s contiguous tip: %w", rc.table, err)
	}
	return opTypeRollupWork(built, src, head, tip, maxClosed), nil
}

// opTypeRollupWork is Plan's decision, split out for tests. tip is the contiguous watermark from head's first
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

// RunPartition recomputes one partition below through and swaps it in atomically (private staging table, idempotent); a recompute
// counting fewer rows than the live partition is refused with ErrCountsShrink unless shrinkOK.
func (rc *CountsRollup) RunPartition(ctx context.Context, addr string, w OpTypeRollupPartition, shrinkOK bool, logf func(string, ...any)) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return rc.runPartitionConn(ctx, conn, w, shrinkOK, logf)
}

func (rc *CountsRollup) runPartitionConn(ctx context.Context, conn driver.Conn, w OpTypeRollupPartition, shrinkOK bool, logf func(string, ...any)) error {
	start := time.Now()
	pid := strconv.FormatUint(uint64(w.Part), 10)
	// Read the fingerprint BEFORE the compute: a row landing mid-build then shows as a mismatch and the next run rebuilds.
	src, err := rc.sourceRows(ctx, conn, pid)
	if err != nil {
		return err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("clickhouse: %s staging name: %w", rc.table, err)
	}
	staging := rc.table + "_staging_" + hex.EncodeToString(b[:])
	if err := conn.Exec(ctx, "CREATE TABLE stellar."+staging+" AS stellar."+rc.table); err != nil {
		return fmt.Errorf("clickhouse: %s staging create %s: %w", rc.table, staging, err)
	}
	defer func() { //nolint:contextcheck // detached cleanup ctx: must fire even when the parent ctx is already cancelled
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if derr := conn.Exec(dropCtx, "DROP TABLE IF EXISTS stellar."+staging); derr != nil {
			logf("partition %s staging cleanup %s failed: %v", pid, staging, derr)
		}
	}()

	if err := conn.Exec(ctx, `INSERT INTO stellar.`+staging+` (part, account, `+rc.group+`, n, through)
		SELECT toUInt32(?), `+rc.account+`, `+rc.group+`, count(), toUInt32(?)
		  FROM stellar.`+rc.source+` FINAL
		 WHERE `+rc.ledger+` >= ? AND `+rc.ledger+` < ?
		 GROUP BY `+rc.account+`, `+rc.group+`
		SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_bytes_before_external_group_by = 4000000000,
		         max_execution_time = 1800, do_not_merge_across_partitions_select_final = 1`,
		w.Part, w.Through, w.Part*opTypePartitionWidth, w.Through); err != nil {
		return fmt.Errorf("clickhouse: %s partition %s compute: %w", rc.table, pid, err)
	}
	if err := conn.Exec(ctx, `INSERT INTO stellar.`+staging+` (part, account, `+rc.group+`, n, through) VALUES (?, '', '', ?, ?)`,
		w.Part, src[w.Part], w.Through); err != nil {
		return fmt.Errorf("clickhouse: %s partition %s sentinel: %w", rc.table, pid, err)
	}
	if !shrinkOK {
		var newOps, liveOps uint64
		if err := conn.QueryRow(ctx, `SELECT
			(SELECT toUInt64(sum(n)) FROM stellar.`+staging+` WHERE account != ''),
			(SELECT toUInt64(sum(n)) FROM stellar.`+rc.table+` WHERE part = ? AND account != '')`, w.Part).
			Scan(&newOps, &liveOps); err != nil {
			return fmt.Errorf("clickhouse: %s partition %s size check: %w", rc.table, pid, err)
		}
		if newOps < liveOps {
			return fmt.Errorf("%w: %s partition %s recomputed %d row(s), live has %d — the lake lost rows under it; pass -shrink-ok only if intended",
				ErrCountsShrink, rc.table, pid, newOps, liveOps)
		}
	}
	if err := conn.Exec(ctx, "ALTER TABLE stellar."+rc.table+" REPLACE PARTITION ID '"+pid+"' FROM stellar."+staging); err != nil {
		return fmt.Errorf("clickhouse: %s partition %s replace: %w", rc.table, pid, err)
	}
	logf("%s partition %s through %d (%s) done in %s", rc.Name, pid, w.Through, w.Reason, time.Since(start).Round(time.Second))
	return nil
}

// accountParticipantPartitions narrows an account-keyed read of operation_participants to the partitions the account has ops in, and
// gives newestPartitionFloor under ceiling. It is only as complete as account_op_types: re-deriving operation_participants must re-run
// its participant arm (account_op_types.sql).
func (r *ExplorerReader) accountParticipantPartitions(ctx context.Context, account string, ceiling uint32) (clause string, floor uint32, hasFloor bool) {
	if !r.accountOpTypesCovered(ctx) {
		return "", 0, false
	}
	parts, tail, ok := r.accountPartitions(ctx, OpTypeCountsRollup, account)
	if !ok {
		return "", 0, false
	}
	floor, hasFloor = newestPartitionFloor(parts, ceiling)
	return partitionsClauseFor(parts, tail, "ledger_seq"), floor, hasFloor
}

// accountPartitions returns the sorted partitions rc has the account in plus every stale one, and the first ledger of the partition
// the plan's tail starts in, so a row landing late below the open partition's built mark is still read. Without it one granule per part
// is read from all ~200 parts. ok=false (plan missing, read error) means the account's partitions are unknown.
func (r *ExplorerReader) accountPartitions(ctx context.Context, rc *CountsRollup, account string) (parts []uint32, tail uint32, ok bool) {
	plan, ok := r.planFor(ctx, rc)
	if !ok {
		return nil, 0, false
	}
	parts, err := queryKeys(ctx, r.conn, `SELECT DISTINCT part FROM stellar.`+rc.table+`
		WHERE account = ? AND part IN (`+joinUint32(plan.valid)+`)`+explorerScanSettings,
		[]any{account}, func(rows driver.Rows) (uint32, error) {
			var p uint32
			err := rows.Scan(&p)
			return p, err
		})
	if err != nil {
		return nil, 0, false
	}
	parts = append(parts, plan.stale...)
	slices.Sort(parts)
	return parts, plan.tailFrom / opTypePartitionWidth * opTypePartitionWidth, true
}

// newestPartitionFloor is the first ledger of the newest of parts at or below ceiling, when an older one exists too. A busy account sits
// in every partition and pays one granule per part in each; a page from its newest one alone is a fraction of that, and since every
// row above the floor is read, a full page from it is the true page.
func newestPartitionFloor(parts []uint32, ceiling uint32) (uint32, bool) {
	for i := len(parts) - 1; i > 0; i-- {
		if floor := parts[i] * opTypePartitionWidth; floor <= ceiling {
			return floor, true
		}
	}
	return 0, false
}

func partitionsClauseFor(parts []uint32, tail uint32, ledgerCol string) string {
	clause := " AND (" + ledgerCol + " >= " + strconv.FormatUint(uint64(tail), 10)
	if len(parts) > 0 {
		clause += " OR intDiv(" + ledgerCol + ", " + strconv.Itoa(opTypePartitionWidth) + ") IN (" + joinUint32(parts) + ")"
	}
	return clause + ")"
}
