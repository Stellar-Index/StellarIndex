//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMigration0205_TradesCompressionGivesUpOnALockedChunk pins that the
// trades compression job bounds its wait for a chunk lock: behind a reader
// it fails within seconds and leaves the chunk uncompressed instead of
// queueing every later reader of the chunk behind it.
func TestMigration0205_TradesCompressionGivesUpOnALockedChunk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 198)
	requireSchemaVersion(t, ctx, db, 198)
	quiesceCAGGRefreshPolicies(t, ctx, db)
	before := tradesCompressionJobs(t, ctx, db)
	if before.builtin != 1 || before.bounded != 0 {
		t.Fatalf("before 0205: built-in=%d bounded=%d, want 1 and 0", before.builtin, before.bounded)
	}

	applyMigrationsUpTo(t, dsn, 205)
	requireSchemaVersion(t, ctx, db, 205)
	after := tradesCompressionJobs(t, ctx, db)
	if after.builtin != 0 || after.bounded != 1 {
		t.Fatalf("after 0205: built-in=%d bounded=%d, want 0 and 1", after.builtin, after.bounded)
	}
	if after.job != before.job {
		t.Errorf("after 0205 the job settings changed:\n got %+v\nwant %+v", after.job, before.job)
	}

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer func() { _ = store.Close() }()
	p, err := store.TradesCompressionPolicy(ctx)
	if err != nil {
		t.Fatalf("resolve the trades compression job: %v", err)
	}
	if p.CompressAfter != 15*24*time.Hour || !p.Scheduled {
		t.Fatalf("resolved job = %+v, want scheduled with compress_after 15 days", p)
	}
	// Run it by hand only, so the scheduler cannot race the assertions.
	if err := store.SetJobScheduled(ctx, p.JobID, false); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	insertTxIndexTrade(t, ctx, db, "sdex", 59_100_000, "lt-old", now.Add(-40*24*time.Hour))
	insertTxIndexTrade(t, ctx, db, "sdex", 59_100_001, "lt-new", now.Add(-2*24*time.Hour))
	var oldChunk string
	if err := db.QueryRowContext(ctx, `
		SELECT c::text FROM show_chunks('trades', older_than => now() - interval '30 days') c`).Scan(&oldChunk); err != nil {
		t.Fatalf("find the 40-day-old chunk: %v", err)
	}

	overdue := overdueCompressionProbe(t)
	if got := overdueTradesChunks(t, ctx, db, overdue); got != 1 {
		t.Fatalf("overdue-compression probe counts %d trades chunks before compression, want 1", got)
	}

	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT count(*) FROM `+oldChunk); err != nil {
		t.Fatalf("read %s: %v", oldChunk, err)
	}

	runJob := func(budget time.Duration) (time.Duration, error) {
		rctx, rcancel := context.WithTimeout(ctx, budget)
		defer rcancel()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		defer func() { _ = conn.Close() }()
		start := time.Now()
		_, err = conn.ExecContext(rctx, fmt.Sprintf(`CALL run_job(%d)`, p.JobID))
		elapsed := time.Since(start)
		// A failed run leaves its session lock_timeout set; keep it off the
		// pool. A connection a cancelled run broke is discarded anyway.
		_, _ = conn.ExecContext(ctx, `RESET lock_timeout`)
		return elapsed, err
	}

	elapsed, err := runJob(60 * time.Second)
	// TimescaleDB reports the chunk's lock timeout as "columnstore policy failure".
	if err == nil || !strings.Contains(err.Error(), "policy failure") || elapsed >= 30*time.Second {
		t.Fatalf("compression behind a reader: err=%v after %s, want a policy failure within 30s", err, elapsed)
	}
	if chunkIsCompressed(t, ctx, db, oldChunk) {
		t.Fatal("the failed run compressed the chunk")
	}

	_ = holder.Rollback()
	if _, err := runJob(2 * time.Minute); err != nil {
		t.Fatalf("compression with the reader gone: %v", err)
	}
	if !chunkIsCompressed(t, ctx, db, oldChunk) {
		t.Fatal("the uncontended run left the old chunk uncompressed")
	}
	if got := overdueTradesChunks(t, ctx, db, overdue); got != 0 {
		t.Errorf("overdue-compression probe counts %d trades chunks after compression, want 0", got)
	}

	if err := store.SetJobScheduled(ctx, p.JobID, true); err != nil {
		t.Fatal(err)
	}
	applyMigrationsUpTo(t, dsn, 198)
	requireSchemaVersion(t, ctx, db, 198)
	down := tradesCompressionJobs(t, ctx, db)
	if down.builtin != 1 || down.bounded != 0 {
		t.Fatalf("after 0205 down: built-in=%d bounded=%d, want 1 and 0", down.builtin, down.bounded)
	}
	if down.job != before.job {
		t.Errorf("after 0205 down the job settings changed:\n got %+v\nwant %+v", down.job, before.job)
	}
	var procs int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'trades_compression_policy'`).Scan(&procs); err != nil || procs != 0 {
		t.Errorf("after 0205 down trades_compression_policy procedures = %d (err %v), want 0", procs, err)
	}
}

// TestMigration0205_RefusesWhileARestampHoldsItsLock pins that the swap
// does not delete the built-in job under a live usd-volume-restamp -write.
func TestMigration0205_RefusesWhileARestampHoldsItsLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 198)
	requireSchemaVersion(t, ctx, db, 198)
	quiesceCAGGRefreshPolicies(t, ctx, db)

	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer func() { _ = holder.Close() }()
	var got bool
	if err := holder.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtext($1::text))`,
		timescale.USDVolumeRestampLockName).Scan(&got); err != nil || !got {
		t.Fatalf("take the restamp lock: got=%v err=%v", got, err)
	}

	err = applyMigrationsUpToErr(dsn, 205)
	if err == nil || !strings.Contains(err.Error(), "usd-volume-restamp") {
		t.Fatalf("0205 with the restamp lock held: err=%v, want a refusal naming usd-volume-restamp", err)
	}
	if before := tradesCompressionJobs(t, ctx, db); before.builtin != 1 || before.bounded != 0 {
		t.Fatalf("after the refusal: built-in=%d bounded=%d, want 1 and 0", before.builtin, before.bounded)
	}

	// A refused step leaves schema_migrations dirty; put it back to retry.
	if _, err := db.ExecContext(ctx, `UPDATE schema_migrations SET version = 198, dirty = false`); err != nil {
		t.Fatalf("clear the dirty flag: %v", err)
	}
	var released bool
	if err := holder.QueryRowContext(ctx, `SELECT pg_advisory_unlock(hashtext($1::text))`,
		timescale.USDVolumeRestampLockName).Scan(&released); err != nil || !released {
		t.Fatalf("release the restamp lock: %v %v", released, err)
	}
	applyMigrationsUpTo(t, dsn, 205)
	requireSchemaVersion(t, ctx, db, 205)
	if after := tradesCompressionJobs(t, ctx, db); after.builtin != 0 || after.bounded != 1 {
		t.Fatalf("after 0205: built-in=%d bounded=%d, want 0 and 1", after.builtin, after.bounded)
	}
}

type tradesCompressionJobSettings struct {
	schedule, maxRuntime, retryPeriod string
	maxRetries                        int
	scheduled                         bool
	compressAfter                     string
}

type tradesCompressionJobCensus struct {
	builtin, bounded int
	job              tradesCompressionJobSettings
}

// tradesCompressionJobs counts both job shapes and reads the settings of
// whichever one exists.
func tradesCompressionJobs(t *testing.T, ctx context.Context, db *sql.DB) tradesCompressionJobCensus {
	t.Helper()
	var r tradesCompressionJobCensus
	const builtin = `j.proc_name = 'policy_compression' AND j.hypertable_schema = current_schema() AND j.hypertable_name = 'trades'`
	const bounded = `j.proc_schema = current_schema() AND j.proc_name = 'trades_compression_policy'`
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE `+builtin+`), count(*) FILTER (WHERE `+bounded+`)
		  FROM timescaledb_information.jobs j`).Scan(&r.builtin, &r.bounded); err != nil {
		t.Fatalf("count trades compression jobs: %v", err)
	}
	if r.builtin+r.bounded != 1 {
		return r
	}
	if err := db.QueryRowContext(ctx, `
		SELECT j.schedule_interval::text, j.max_runtime::text, j.retry_period::text,
		       j.max_retries, j.scheduled, j.config->>'compress_after'
		  FROM timescaledb_information.jobs j
		 WHERE (`+builtin+`) OR (`+bounded+`)`).Scan(
		&r.job.schedule, &r.job.maxRuntime, &r.job.retryPeriod,
		&r.job.maxRetries, &r.job.scheduled, &r.job.compressAfter); err != nil {
		t.Fatalf("read the trades compression job: %v", err)
	}
	return r
}

func chunkIsCompressed(t *testing.T, ctx context.Context, db *sql.DB, chunk string) bool {
	t.Helper()
	var compressed bool
	if err := db.QueryRowContext(ctx, `
		SELECT is_compressed FROM timescaledb_information.chunks
		 WHERE format('%I.%I', chunk_schema, chunk_name) = $1`, chunk).Scan(&compressed); err != nil {
		t.Fatalf("read %s compression state: %v", chunk, err)
	}
	return compressed
}

// overdueCompressionProbe is the stellarindex_timescale_chunks_overdue_compression
// query exactly as the node_exporter probe ships it.
func overdueCompressionProbe(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "configs", "ansible", "roles", "archival-node", "tasks", "10-observability.yml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	_, rest, ok := strings.Cut(string(src), `q "SELECT COALESCE(j.hypertable_name, 'trades'),`)
	if !ok {
		t.Fatalf("%s has no overdue-compression query mapping the trades job", path)
	}
	query, _, ok := strings.Cut(rest, `"`)
	if !ok {
		t.Fatalf("unterminated overdue-compression query in %s", path)
	}
	return "SELECT COALESCE(j.hypertable_name, 'trades')," + query
}

func overdueTradesChunks(t *testing.T, ctx context.Context, db *sql.DB, query string) int {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("run the overdue-compression probe: %v", err)
	}
	defer rows.Close()
	n := -1
	for rows.Next() {
		var ht string
		var overdue int
		if err := rows.Scan(&ht, &overdue); err != nil {
			t.Fatalf("scan the overdue-compression probe: %v", err)
		}
		if ht == "trades" {
			n = overdue
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the overdue-compression probe: %v", err)
	}
	if n < 0 {
		t.Fatal("the overdue-compression probe emits no trades row")
	}
	return n
}
