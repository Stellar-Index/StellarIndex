//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These pin, on the deployed pair (TimescaleDB 2.26.4 / PG 15), what the
// chunk restamp's 5 s lock bound actually covers and the long-holder check
// in front of it. Each attempt opens with LOCK TABLE on the locks the
// function takes first, so a refusal there costs no work; the function's
// late AccessExclusiveLock request is bounded at the same 5 s; and a
// transaction that has held a lock on the chunk for over a minute (the
// incident shape) is waited out before any work starts.

// openCompressedTradesChunk opens a store on a fresh database holding one
// compressed `trades` chunk and returns both.
func openCompressedTradesChunk(t *testing.T, ctx context.Context) (string, *timescale.Store, timescale.TradeChunk) {
	t.Helper()
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
		if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", 40+i, day.Add(time.Duration(i)*time.Minute), pair, 1_000_000_000, 500_000_000)); err != nil {
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
	return dsn, store, chunks[0]
}

// holdInOpenTx runs stmt in a transaction on its own pool and leaves the
// transaction open; the returned func ends it.
func holdInOpenTx(t *testing.T, ctx context.Context, dsn, stmt string) func() {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("holder %q: %v", stmt, err)
	}
	ended := false
	end := func() {
		if !ended {
			ended = true
			_ = tx.Rollback()
		}
	}
	t.Cleanup(end)
	return end
}

// chunkLocksOfOthersSQL lists the chunk locks held or requested by the
// backend running the restamp's statements ($3 matches its query).
const chunkLocksOfOthersSQL = `
	SELECT l.mode, l.granted
	  FROM pg_locks l
	  JOIN pg_stat_activity a ON a.pid = l.pid
	 WHERE l.relation = to_regclass(format('%I.%I', $1::text, $2::text))
	   AND a.query ILIKE $3
	   AND a.query NOT ILIKE '%pg_locks%'`

type lockSample struct {
	mode    string
	granted bool
}

// lockSnapshot is one poll: when it was taken and what it found.
type lockSnapshot struct {
	at    time.Time
	locks []lockSample
}

// sampleRestampChunkLocks polls the chunk's locks held by a backend whose
// current query matches like, every 100 ms for d.
func sampleRestampChunkLocks(t *testing.T, ctx context.Context, db *sql.DB, chunk timescale.TradeChunk, like string, d time.Duration) []lockSnapshot {
	t.Helper()
	var out []lockSnapshot
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		rows, err := db.QueryContext(ctx, chunkLocksOfOthersSQL, chunk.Schema, chunk.Name, like)
		if err != nil {
			t.Fatalf("sample pg_locks: %v", err)
		}
		s := lockSnapshot{at: time.Now()}
		for rows.Next() {
			var l lockSample
			if err := rows.Scan(&l.mode, &l.granted); err != nil {
				t.Fatal(err)
			}
			s.locks = append(s.locks, l)
		}
		_ = rows.Close()
		out = append(out, s)
	}
	return out
}

// TestDecompressTradesChunk_WaitsOutALongHolderBeforeWorking is the
// incident shape: a reader whose transaction has been open for over a
// minute holds AccessShareLock on the chunk. The decompress must not start
// work whose end-stage lock that reader would refuse; it waits with no
// request pending and starts once the reader is gone.
func TestDecompressTradesChunk_WaitsOutALongHolderBeforeWorking(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn, store, chunk := openCompressedTradesChunk(t, ctx)

	end := holdInOpenTx(t, ctx, dsn, `SELECT count(*) FROM trades`)
	time.Sleep(62 * time.Second) // past the 60 s long-holder age

	done := make(chan error, 1)
	go func() { done <- store.DecompressTradesChunk(ctx, chunk) }()

	sampler, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sampler.Close() })
	// 8 s spans more than one 5 s request: an attempt started against the
	// holder would be seen holding or asking for a chunk lock.
	for i, s := range sampleRestampChunkLocks(t, ctx, sampler, chunk, "%_chunk%", 8*time.Second) {
		if len(s.locks) > 0 {
			t.Fatalf("sample %d: the decompress took or asked for chunk locks %+v while a long-running reader held the chunk", i, s.locks)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("DecompressTradesChunk returned %v while the long holder was still there", err)
	default:
	}

	end()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DecompressTradesChunk = %v, want it to start once the holder was gone", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("DecompressTradesChunk never returned after the long holder ended")
	}
	if chunkIsCompressed(t, ctx, store.DB(), chunk.String()) {
		t.Error("the chunk is still compressed")
	}
}

// TestDecompressTradesChunk_RefusedOpeningLockStartsNoWork: a writer's
// RowExclusiveLock conflicts with the ExclusiveLock both functions open
// with. The attempt's own LOCK TABLE is what waits and is refused, so
// decompress_chunk never runs and the error says no work was lost.
func TestDecompressTradesChunk_RefusedOpeningLockStartsNoWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn, store, chunk := openCompressedTradesChunk(t, ctx)
	holdInOpenTx(t, ctx, dsn, `LOCK TABLE ONLY `+chunk.Schema+`.`+chunk.Name+` IN ROW EXCLUSIVE MODE`)

	shortCtx, shortCancel := context.WithTimeout(ctx, 12*time.Second)
	defer shortCancel()
	done := make(chan error, 1)
	go func() { done <- store.DecompressTradesChunk(shortCtx, chunk) }()

	sampler, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sampler.Close() })
	var asked bool
	for _, s := range sampleRestampChunkLocks(t, ctx, sampler, chunk, "LOCK TABLE ONLY%IN EXCLUSIVE MODE", 4*time.Second) {
		for _, l := range s.locks {
			if l.mode == "ExclusiveLock" && !l.granted {
				asked = true
			}
		}
	}
	if !asked {
		t.Fatal("never saw the attempt's LOCK TABLE waiting for ExclusiveLock on the chunk")
	}
	err = <-done
	if err == nil {
		t.Fatal("DecompressTradesChunk = nil while a writer held the chunk for its whole window")
	}
	for _, want := range []string{"refused before any work", "0 refused inside the statement"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want containing %q", err, want)
		}
	}
	if !chunkIsCompressed(t, ctx, store.DB(), chunk.String()) {
		t.Error("the chunk was left decompressed by a refused attempt")
	}
}

// TestDecompressTradesChunk_EndStageRequestIsBoundedToo: with a young
// reader on the chunk, the attempt gets its ExclusiveLock and then waits
// for AccessExclusiveLock on the same chunk — the late request — and that
// wait ends at the same 5 s bound, inside the statement.
func TestDecompressTradesChunk_EndStageRequestIsBoundedToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn, store, chunk := openCompressedTradesChunk(t, ctx)
	holdInOpenTx(t, ctx, dsn, `SELECT count(*) FROM trades`)

	shortCtx, shortCancel := context.WithTimeout(ctx, 12*time.Second)
	defer shortCancel()
	done := make(chan error, 1)
	go func() { done <- store.DecompressTradesChunk(shortCtx, chunk) }()

	sampler, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sampler.Close() })
	var endStage, sawAsk bool
	var firstAsk, lastAsk time.Time
	start := time.Now()
	for _, s := range sampleRestampChunkLocks(t, ctx, sampler, chunk, "%decompress_chunk%", 9*time.Second) {
		var heldExclusive, askingAccessExclusive bool
		for _, l := range s.locks {
			heldExclusive = heldExclusive || (l.mode == "ExclusiveLock" && l.granted)
			askingAccessExclusive = askingAccessExclusive || (l.mode == "AccessExclusiveLock" && !l.granted)
		}
		if askingAccessExclusive {
			if !sawAsk {
				firstAsk = s.at
			}
			sawAsk, lastAsk = true, s.at
			endStage = endStage || heldExclusive
		}
	}
	if !endStage {
		t.Fatal("never saw decompress_chunk holding ExclusiveLock while asking for AccessExclusiveLock on the chunk")
	}
	if asked := lastAsk.Sub(firstAsk); asked > 6*time.Second {
		t.Errorf("the end-stage request stayed pending %s, want it withdrawn at the 5 s bound", asked)
	}
	t.Logf("end-stage request pending from %s to %s after the call", firstAsk.Sub(start), lastAsk.Sub(start))

	err = <-done
	if err == nil || !strings.Contains(err.Error(), "1 refused inside the statement") {
		t.Fatalf("err = %v, want the refusal counted as inside the statement", err)
	}
	if !chunkIsCompressed(t, ctx, store.DB(), chunk.String()) {
		t.Error("the chunk was left decompressed by a refused attempt")
	}
}
