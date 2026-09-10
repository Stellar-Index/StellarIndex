//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDecompressTradesChunk_DoesNotConvoyTheDatabase is the DB-backed
// reproduction of the 2026-09-10 r1 incident, on the deployed pair
// (TimescaleDB 2.26.4 / PG 15), and the proof that the fix removes it.
//
// WHAT HAPPENED. A deploy restarted stellarindex-aggregator at 00:12:23
// UTC. Its cold-start VWAP alias-map aggregation spilled to disk
// (wait_event = IO/BufFileRead) and held AccessShareLock on `trades` for
// 18+ minutes. A `usd-volume-restamp -chunks` run was mid-window; its
// decompress_chunk asked for AccessExclusiveLock on a chunk of that
// hypertable and could not have it, so it QUEUED. A pending exclusive
// request is not a private wait — PostgreSQL puts every LATER request for
// that object behind it, however trivial:
//
//	decompress_chunk (restamp)      blocked 1,984 s
//	UPDATE trades  x2 (restamp)     blocked 1,164 s
//	postgres_exporter scrapes x3    blocked   917 s
//	chunks_detailed_size (watcher)  blocked   904 s
//
// The exporter being in that list is what made it dangerous: alerting went
// cascade-blind for the whole window while /v1/status read `degraded` and
// both systemd units read `active`.
//
// THE SHAPE OF THIS TEST is those three sessions, in that order:
//
//	A — holds AccessShareLock on `trades` in an open transaction (the
//	    aggregator's spilled read);
//	B — DecompressTradesChunk on the compressed chunk (the restamp);
//	C — one trivial `SELECT count(*) FROM trades` issued once B's request
//	    is observably queued (the postgres_exporter scrape).
//
// C is the assertion. Against an UNBOUNDED decompress it never returns: it
// is queued behind B's pending exclusive request and dies at its own
// lock_timeout. Against the bounded one B withdraws after
// tradesChunkDecompressLock.wait (5 s) and spends the next 15 s not
// asking, so C gets its AccessShareLock and answers.
//
// The second half is the crash-safety half: withdrawing must not mean
// giving up. Once A commits, B's next attempt takes the lock and the chunk
// ends DECOMPRESSED — the state the caller asked for — rather than the
// walk failing and leaving a human to finish it.
func TestDecompressTradesChunk_DoesNotConvoyTheDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// ── a compressed `trades` chunk, as the 7-day policy leaves them ──
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		tr := mkIntegrationTrade("sdex", 20+i, day.Add(time.Duration(i)*time.Minute), pair, 1_000_000_000, 500_000_000)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `SELECT compress_chunk(ch, true) FROM show_chunks('trades') ch`); err != nil {
		t.Fatalf("compress the fixture chunk: %v", err)
	}
	chunks, err := store.TradesChunksInRange(ctx, day.Add(-24*time.Hour), day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("TradesChunksInRange: %v", err)
	}
	if len(chunks) != 1 || !chunks[0].Compressed {
		t.Fatalf("fixture: want exactly one COMPRESSED chunk, got %+v", chunks)
	}
	chunk := chunks[0]

	// ── session A: the aggregator's long cold-start read ──────────────
	//
	// Its own pool, so nothing here can be served by a connection the
	// store is also using. The transaction stays open — that is what
	// holds AccessShareLock on the hypertable and its chunk.
	reader, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	readerTx, err := reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("session A begin: %v", err)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = readerTx.Rollback()
		}
	})
	var seen int
	if err := readerTx.QueryRowContext(ctx, `SELECT count(*) FROM trades`).Scan(&seen); err != nil {
		t.Fatalf("session A read: %v", err)
	}
	if seen != 5 {
		t.Fatalf("session A saw %d trades, want 5", seen)
	}

	// ── session B: the restamp's decompress ───────────────────────────
	type decompressResult struct{ err error }
	done := make(chan decompressResult, 1)
	go func() { done <- decompressResult{store.DecompressTradesChunk(ctx, chunk)} }()

	// ── wait until B's request is observably QUEUED ───────────────────
	//
	// Both with and without the fix there is a window in which the
	// decompress is waiting on the lock; without it, that window never
	// ends. Starting C from inside the window is what makes the two
	// outcomes differ by behaviour rather than by timing luck.
	probe, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = probe.Close() })
	waitForQueuedDecompress(t, ctx, probe)

	// ── session C: the postgres_exporter scrape ───────────────────────
	//
	// 9 s is deliberately just past the 5 s the bounded decompress may
	// hold a request pending, and far short of the 904–1,984 s the real
	// convoy inflicted. A failure here IS the incident.
	scrapeCtx, scrapeCancel := context.WithTimeout(ctx, 60*time.Second)
	defer scrapeCancel()
	conn, err := probe.Conn(scrapeCtx)
	if err != nil {
		t.Fatalf("session C conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(scrapeCtx, `SET lock_timeout = '9s'`); err != nil {
		t.Fatalf("session C set lock_timeout: %v", err)
	}
	start := time.Now()
	var n int
	err = conn.QueryRowContext(scrapeCtx, `SELECT count(*) FROM trades`).Scan(&n)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a trivial read was CONVOYED behind the decompress's pending exclusive lock "+
			"(%v after %s) — this is the 2026-09-10 pile-up: the decompress is parking an "+
			"unbounded AccessExclusiveLock request and PostgreSQL is queueing everything behind it", err, elapsed)
	}
	if n != 5 {
		t.Errorf("session C read %d trades, want 5", n)
	}
	t.Logf("trivial read completed in %s while the decompress was contending", elapsed)

	// ── the decompress must not have GIVEN UP ─────────────────────────
	//
	// Withdrawing is only safe because it comes back. Release the reader
	// and the next attempt takes the lock.
	if err := readerTx.Commit(); err != nil {
		t.Fatalf("session A commit: %v", err)
	}
	committed = true

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("DecompressTradesChunk = %v, want it to retry past the contention and succeed", res.err)
		}
	case <-time.After(3 * time.Minute):
		t.Fatal("DecompressTradesChunk never returned after the conflicting lock was released")
	}

	var compressed bool
	if err := store.DB().QueryRowContext(ctx,
		`SELECT is_compressed FROM timescaledb_information.chunks WHERE chunk_schema = $1 AND chunk_name = $2`,
		chunk.Schema, chunk.Name).Scan(&compressed); err != nil {
		t.Fatalf("read the chunk's state: %v", err)
	}
	if compressed {
		t.Error("the chunk is still compressed: the bounded wait turned into a silent no-op")
	}
}

// TestDecompressTradesChunk_WithdrawsItsRequestBetweenAttempts pins the
// mechanism rather than one of its effects: a decompress that cannot have
// its lock must spend most of its time NOT asking.
//
// That is the whole cure. PostgreSQL convoys on a PENDING exclusive
// request — every later request for the object queues behind it — so the
// damage is a function of how long ours is outstanding, not of how long we
// would like the lock. The bounded loop asks for 5 s, withdraws, and stays
// quiet for 15 s; during that quiet the queue behind it drains, which is
// what lets a trivial read through. Sampling pg_stat_activity across a
// full cycle turns that into an assertion: with an UNBOUNDED decompress
// every sample finds it waiting, because the one request it made never
// goes away.
//
// It carries the crash-safety half too, which is the objection the bound
// has to answer — a decompress that fails mid-chunk is the state this
// project fears most. It cannot arise: the statement runs in its own
// transaction, so a `lock_timeout` expiry (SQLSTATE 55P03) rolls back
// atomically, the chunk is left exactly as compressed as it was with every
// row readable, and [timescale.Store.RestampTradesChunk]'s existing
// contract ("a failed decompress runs nothing") is untouched.
func TestDecompressTradesChunk_WithdrawsItsRequestBetweenAttempts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		tr := mkIntegrationTrade("sdex", 30+i, day.Add(time.Duration(i)*time.Minute), pair, 1_000_000_000, 500_000_000)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `SELECT compress_chunk(ch, true) FROM show_chunks('trades') ch`); err != nil {
		t.Fatalf("compress the fixture chunk: %v", err)
	}
	chunks, err := store.TradesChunksInRange(ctx, day.Add(-24*time.Hour), day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("TradesChunksInRange: %v", err)
	}
	if len(chunks) != 1 || !chunks[0].Compressed {
		t.Fatalf("fixture: want exactly one COMPRESSED chunk, got %+v", chunks)
	}
	chunk := chunks[0]

	reader, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	readerTx, err := reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("blocker begin: %v", err)
	}
	defer func() { _ = readerTx.Rollback() }()
	var seen int
	if err := readerTx.QueryRowContext(ctx, `SELECT count(*) FROM trades`).Scan(&seen); err != nil {
		t.Fatalf("blocker read: %v", err)
	}

	// A caller-imposed deadline shorter than the retry budget is how the
	// stop is reached without waiting 35 minutes for it. It is longer than
	// one full ask+drain cycle (5 s + 15 s), so a loop that withdraws has
	// to be caught doing it.
	shortCtx, shortCancel := context.WithTimeout(ctx, 28*time.Second)
	defer shortCancel()
	var decompressErr error
	finished := make(chan struct{})
	go func() {
		decompressErr = store.DecompressTradesChunk(shortCtx, chunk)
		close(finished)
	}()

	sampler, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sampler.Close() })
	asking, quiet := sampleDecompressLockWait(t, ctx, sampler, finished)
	t.Logf("decompress had a lock request outstanding in %d sample(s), none in %d", asking, quiet)
	if asking == 0 {
		t.Fatal("never observed the decompress waiting on the lock; the fixture did not reproduce the contention")
	}
	if quiet == 0 {
		t.Fatalf("the decompress had a lock request outstanding in ALL %d samples across a full ask+drain cycle — "+
			"it is parking a pending AccessExclusiveLock, and PostgreSQL queues every later request for that chunk behind it", asking)
	}

	<-finished
	if decompressErr == nil {
		t.Fatal("DecompressTradesChunk = nil while a conflicting lock was held for its whole budget")
	}

	var compressed bool
	if err := store.DB().QueryRowContext(ctx,
		`SELECT is_compressed FROM timescaledb_information.chunks WHERE chunk_schema = $1 AND chunk_name = $2`,
		chunk.Schema, chunk.Name).Scan(&compressed); err != nil {
		t.Fatalf("read the chunk's state: %v", err)
	}
	if !compressed {
		t.Fatal("the chunk was left DECOMPRESSED by a decompress that never got its lock — " +
			"the bounded wait must roll back atomically, not half-open a 160 GB chunk")
	}
	// And the rows are still there and readable through the compressed
	// chunk: "still compressed" must mean intact, not merely flagged.
	var rows int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM trades`).Scan(&rows); err != nil {
		t.Fatalf("read trades after the refused decompress: %v", err)
	}
	if rows != 5 {
		t.Errorf("trades holds %d rows after the refused decompress, want 5", rows)
	}
}

// decompressAskingSQL is true while some backend running decompress_chunk
// has a heavyweight-lock request outstanding. Reading pg_stat_activity
// costs no lock on `trades`, so this stays answerable from inside a convoy
// — the same property the stellarindex_pg_lock_convoy probe depends on.
const decompressAskingSQL = `
	SELECT EXISTS (
	  SELECT 1 FROM pg_stat_activity
	   WHERE query ILIKE '%decompress_chunk%'
	     AND query NOT ILIKE '%pg_stat_activity%'
	     AND wait_event_type = 'Lock')`

// sampleDecompressLockWait polls until it has seen the decompress both
// ASKING for the lock and, AFTERWARDS, not asking — or until the
// decompress returns. A bounded loop produces both counts within one
// cycle; an unbounded one produces only the first, because its single
// request never goes away.
//
// Quiet samples before the first asking one are DISCARDED, deliberately.
// They are the window before the goroutine's statement reached the
// server, and counting them would let the test pass on a race rather
// than on a withdrawal: a quiet sample only means something once we have
// watched the request exist.
func sampleDecompressLockWait(t *testing.T, ctx context.Context, db *sql.DB, finished <-chan struct{}) (asking, quiet int) {
	t.Helper()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-finished:
			return asking, quiet
		case <-tick.C:
		}
		var pending bool
		if err := db.QueryRowContext(ctx, decompressAskingSQL).Scan(&pending); err != nil {
			t.Fatalf("sample pg_stat_activity: %v", err)
		}
		switch {
		case pending:
			asking++
		case asking > 0:
			quiet++
			return asking, quiet
		}
	}
}

// waitForQueuedDecompress blocks until a backend running decompress_chunk
// is waiting on a heavyweight lock — i.e. the restamp's exclusive request
// is queued. Polling the server beats sleeping a guessed interval: the
// window is what the test needs to be inside, and on an unfixed build it
// opens once and never closes.
func waitForQueuedDecompress(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var queued bool
		err := db.QueryRowContext(ctx, decompressAskingSQL).Scan(&queued)
		if err != nil {
			t.Fatalf("poll for the queued decompress: %v", err)
		}
		if queued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no decompress_chunk ever queued on a lock; the fixture did not reproduce the contention")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
