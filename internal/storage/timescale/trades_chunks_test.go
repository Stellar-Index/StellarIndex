// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ─── the chunk-wise restamp's store primitives, pinned on the scripted driver ──
//
// The chunk mode of `usd-volume-restamp -tier xlm-base` exists because a
// per-row UPDATE into a COMPRESSED `trades` chunk decompresses per row
// (measured 2026-09-03: one 2,000-row batch took over 14 minutes, ~1,574
// rows/min across 28.6M rows). The remedy is mechanical — decompress the
// chunk, restamp inside it, re-compress it — and the whole of its safety
// lives in the ORDER of those statements. These tests pin that order and
// the exact SQL, because a chunk left decompressed on failure is a
// 160 GB object on a pool with 4.69 TB free, and nothing else would see it.

// chunkListCols is the column set [tradesChunksInRangeSelect] returns, in
// its SELECT order.
var chunkListCols = []string{
	"chunk_schema", "chunk_name", "range_start", "range_end", "is_compressed",
	"uncompressed_bytes", "compressed_bytes",
}

func chunkRow(name string, start, end time.Time, compressed bool, uncompressed, compressedBytes int64) []driver.Value {
	return []driver.Value{"_timescaledb_internal", name, start, end, compressed, uncompressed, compressedBytes}
}

// sizeResult scripts one `chunks_detailed_size` lookup.
func sizeResult(total int64) scriptedResult {
	return scriptedResult{cols: []string{"total_bytes"}, rows: [][]driver.Value{{total}}}
}

func TestTradesChunksInRange_ListsIntersectingChunksInRangeOrder(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	c1s, c1e := time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	c2s, c2e := c1e, time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: chunkListCols,
		rows: [][]driver.Value{
			chunkRow("_hyper_1_10_chunk", c1s, c1e, true, 4_200_000_000, 280_000_000),
			chunkRow("_hyper_1_11_chunk", c2s, c2e, false, 900_000_000, 0),
		},
	})

	chunks, err := store.TradesChunksInRange(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	stmt := conn.only(t)
	for _, want := range []string{
		"timescaledb_information.chunks",
		"hypertable_name = 'trades'",
		"chunk_compression_stats('trades')",
		"range_start < $2",
		"range_end > $1",
		"ORDER BY c.range_start",
	} {
		if !strings.Contains(stmt.sql, want) {
			t.Errorf("chunk listing SQL lacks %q:\n%s", want, stmt.sql)
		}
	}
	wantTime(t, stmt.arg(t, 1), from)
	wantTime(t, stmt.arg(t, 2), to)

	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	c := chunks[0]
	if c.Schema != "_timescaledb_internal" || c.Name != "_hyper_1_10_chunk" || c.String() != "_timescaledb_internal._hyper_1_10_chunk" {
		t.Errorf("chunk identity = %q / %q / %q", c.Schema, c.Name, c.String())
	}
	if !c.RangeStart.Equal(c1s) || !c.RangeEnd.Equal(c1e) || c.RangeStart.Location() != time.UTC {
		t.Errorf("chunk range = [%s, %s)", c.RangeStart, c.RangeEnd)
	}
	if !c.Compressed || c.UncompressedBytes != 4_200_000_000 || c.CompressedBytes != 280_000_000 {
		t.Errorf("chunk sizes = compressed=%v %d/%d", c.Compressed, c.UncompressedBytes, c.CompressedBytes)
	}
	if chunks[1].Compressed || chunks[1].UncompressedBytes != 900_000_000 || chunks[1].CompressedBytes != 0 {
		t.Errorf("uncompressed chunk = %+v", chunks[1])
	}
}

// TestRestampTradesChunk_BracketsEachChunk: for EVERY chunk, in order —
// size, decompress_chunk(if_compressed => true), size, the work, then
// compress_chunk(if_not_compressed => true), size. Two chunks make two
// complete brackets; the work of the second never runs inside the first's
// decompressed window.
func TestRestampTradesChunk_BracketsEachChunk(t *testing.T) {
	t.Parallel()
	bracket := func() []scriptedResult {
		return []scriptedResult{
			sizeResult(280_000_000), // before
			{},                      // decompress_chunk
			sizeResult(4_200_000_000),
			{rowsAffected: 7}, // the work's own statement
			{},                // compress_chunk
			sizeResult(275_000_000),
		}
	}
	store, conn := newScriptedStore(t, append(bracket(), bracket()...)...)
	ctx := context.Background()
	chunks := []TradeChunk{
		{Schema: "_timescaledb_internal", Name: "_hyper_1_10_chunk", Compressed: true},
		{Schema: "_timescaledb_internal", Name: "_hyper_1_11_chunk", Compressed: true},
	}
	// The hook fires BEFORE the statement it names is issued: the number
	// of statements on the wire at that moment is the proof.
	var announced []string
	for _, c := range chunks {
		res, err := store.RestampTradesChunk(ctx, c, func(ctx context.Context) error {
			_, err := store.db.ExecContext(ctx, "UPDATE trades SET usd_volume = 1 WHERE ts >= $1", c.RangeStart)
			return err
		}, func(step ChunkRestampStep) {
			announced = append(announced, fmt.Sprintf("%d@%d", step, len(conn.stmts)))
		})
		if err != nil {
			t.Fatalf("chunk %s: %v", c, err)
		}
		if res.BytesBefore != 280_000_000 || res.BytesDecompressed != 4_200_000_000 || res.BytesAfter != 275_000_000 {
			t.Errorf("chunk %s sizes = %d / %d / %d", c, res.BytesBefore, res.BytesDecompressed, res.BytesAfter)
		}
	}
	// decompress is statement 1 of each bracket, compress statement 4.
	if want := []string{"1@1", "2@4", "1@7", "2@10"}; strings.Join(announced, ",") != strings.Join(want, ",") {
		t.Errorf("hook calls (step@statements-issued) = %v, want %v", announced, want)
	}

	got := conn.statements()
	if len(got) != 12 {
		t.Fatalf("issued %d statements, want 12 (two 6-statement brackets):\n%s", len(got), strings.Join(got, "\n"))
	}
	for i, base := range []int{0, 6} {
		chunk := chunks[i]
		if !strings.Contains(got[base], "chunks_detailed_size('trades')") {
			t.Errorf("bracket %d stmt 0 = %q, want the size lookup", i, got[base])
		}
		dec := conn.stmts[base+1]
		if !strings.Contains(dec.sql, "decompress_chunk(") || !strings.Contains(dec.sql, "if_compressed => true") {
			t.Errorf("bracket %d stmt 1 = %q, want decompress_chunk(..., if_compressed => true)", i, dec.sql)
		}
		if dec.arg(t, 1) != chunk.Schema || dec.arg(t, 2) != chunk.Name {
			t.Errorf("bracket %d decompress args = %v, want (%s, %s)", i, dec.args, chunk.Schema, chunk.Name)
		}
		if !strings.Contains(got[base+3], "UPDATE trades") {
			t.Errorf("bracket %d stmt 3 = %q, want the work, i.e. INSIDE the decompressed window", i, got[base+3])
		}
		comp := conn.stmts[base+4]
		if !strings.Contains(comp.sql, "compress_chunk(") || strings.Contains(comp.sql, "decompress_chunk(") ||
			!strings.Contains(comp.sql, "if_not_compressed => true") {
			t.Errorf("bracket %d stmt 4 = %q, want compress_chunk(..., if_not_compressed => true)", i, comp.sql)
		}
		if comp.arg(t, 1) != chunk.Schema || comp.arg(t, 2) != chunk.Name {
			t.Errorf("bracket %d compress args = %v, want (%s, %s)", i, comp.args, chunk.Schema, chunk.Name)
		}
	}
	// The regclass is built server-side from the two identifiers, never
	// spliced into the SQL text.
	for _, s := range got {
		if strings.Contains(s, "_hyper_1_1") {
			t.Errorf("a chunk name reached the SQL text: %q", s)
		}
	}
}

// TestRestampTradesChunk_RecompressesWhenWorkFails is the one that
// matters unattended: the work fails (or is cancelled) and the chunk is
// still re-compressed BEFORE the error propagates. The compress runs on a
// context that survives the caller's cancellation, because the most
// likely failure under run-heavy-job.sh is a SIGTERM — and a cancelled
// context would fail the very statement that puts 160 GB back.
func TestRestampTradesChunk_RecompressesWhenWorkFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("update: deadlock detected")
	store, conn := newScriptedStore(t,
		sizeResult(280_000_000),
		scriptedResult{},
		sizeResult(4_200_000_000),
		scriptedResult{}, // compress_chunk; no size lookup follows a failure
	)
	ctx, cancel := context.WithCancel(context.Background())
	c := TradeChunk{Schema: "_timescaledb_internal", Name: "_hyper_1_10_chunk", Compressed: true}
	var workCtx context.Context
	_, err := store.RestampTradesChunk(ctx, c, func(ctx context.Context) error {
		workCtx = ctx
		cancel() // the operator's SIGTERM arrives mid-chunk
		return boom
	}, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the work's error", err)
	}
	if !strings.Contains(err.Error(), "re-compressed") {
		t.Errorf("err = %v, want it to say the chunk was re-compressed", err)
	}
	if workCtx == nil || workCtx.Err() == nil {
		t.Fatal("the work did not observe the cancelled context")
	}
	got := conn.statements()
	if len(got) != 4 {
		t.Fatalf("issued %d statements, want 4:\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[3], "compress_chunk(") || strings.Contains(got[3], "decompress_chunk(") {
		t.Errorf("statement after the failed work = %q, want compress_chunk", got[3])
	}
}

// TestRestampTradesChunk_ReportsAChunkLeftDecompressed: when BOTH the
// work and the re-compress fail, the error names the chunk and says it is
// still decompressed — the operator's cue to compress it by hand.
func TestRestampTradesChunk_ReportsAChunkLeftDecompressed(t *testing.T) {
	t.Parallel()
	boom := errors.New("update: connection reset")
	store, _ := newScriptedStore(t,
		sizeResult(1),
		scriptedResult{},
		sizeResult(2),
		scriptedResult{err: errors.New("compress_chunk: out of disk")},
	)
	c := TradeChunk{Schema: "_timescaledb_internal", Name: "_hyper_1_77_chunk", Compressed: true}
	_, err := store.RestampTradesChunk(context.Background(), c, func(context.Context) error { return boom }, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the work's error", err)
	}
	for _, want := range []string{"LEFT DECOMPRESSED", "_hyper_1_77_chunk", "out of disk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want containing %q", err, want)
		}
	}
}

// TestRestampTradesChunk_DecompressFailureRunsNothing: if the chunk never
// decompressed, nothing else may run — no work, no compress.
func TestRestampTradesChunk_DecompressFailureRunsNothing(t *testing.T) {
	t.Parallel()
	store, conn := newScriptedStore(t,
		sizeResult(1),
		scriptedResult{err: errors.New("decompress_chunk: lock timeout")},
	)
	c := TradeChunk{Schema: "_timescaledb_internal", Name: "_hyper_1_10_chunk", Compressed: true}
	ran := false
	_, err := store.RestampTradesChunk(context.Background(), c, func(context.Context) error { ran = true; return nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("err = %v", err)
	}
	if ran {
		t.Error("the work ran inside a chunk that never decompressed")
	}
	if n := len(conn.statements()); n != 2 {
		t.Errorf("issued %d statements, want 2 (size + the failed decompress)", n)
	}
}

// TestRestampTradesChunk_RangePredicateIsPerChunk: the restamp scan the
// work issues inside the bracket is bounded by THAT chunk's range, so a
// decompressed chunk is never asked to answer for its neighbours.
func TestRestampTradesChunk_RangePredicateIsPerChunk(t *testing.T) {
	t.Parallel()
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	store, conn := newScriptedStore(t,
		sizeResult(1),
		scriptedResult{},
		sizeResult(2),
		scriptedResult{cols: []string{"source"}}, // the scan: no candidates
		scriptedResult{},
		sizeResult(3),
	)
	if err := InstallUSDVolumeResolution(store, []string{usdc}, nil); err != nil {
		t.Fatal(err)
	}
	c := TradeChunk{
		Schema:     "_timescaledb_internal",
		Name:       "_hyper_1_10_chunk",
		RangeStart: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		RangeEnd:   time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
		Compressed: true,
	}
	_, err := store.RestampTradesChunk(context.Background(), c, func(ctx context.Context) error {
		_, err := store.PlanXLMBaseUSDVolumeRestamp(ctx, XLMBaseRestampParams{
			From: c.RangeStart, To: c.RangeEnd, MaxGeneration: 1,
		})
		return err
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	scan := conn.stmts[3]
	if !strings.Contains(scan.sql, "FROM trades") || !strings.Contains(scan.sql, "ts >= $1 AND ts < $2") {
		t.Fatalf("statement inside the bracket = %q, want the restamp scan", scan.sql)
	}
	wantTime(t, scan.arg(t, 1), c.RangeStart)
	wantTime(t, scan.arg(t, 2), c.RangeEnd)
}

func TestTradesDataVolumePath(t *testing.T) {
	t.Parallel()
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"path"}, rows: [][]driver.Value{{"/var/lib/postgresql/data"}},
	})
	got, err := store.TradesDataVolumePath(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "/var/lib/postgresql/data" {
		t.Errorf("path = %q", got)
	}
	stmt := conn.only(t)
	for _, want := range []string{"pg_tablespace_location", "data_directory", "'trades'::regclass"} {
		if !strings.Contains(stmt.sql, want) {
			t.Errorf("SQL lacks %q:\n%s", want, stmt.sql)
		}
	}
}

// TestRestampTradesChunk_LeavesAnUncompressedChunkUncompressed: the
// bracket restores the state the chunk was LISTED in. A chunk that was not
// compressed at listing — the live-adjacent chunks the ledgerstream
// cursor-regression replay upserts into, or one a killed run left open —
// is neither decompressed nor compressed: size, the work, size.
func TestRestampTradesChunk_LeavesAnUncompressedChunkUncompressed(t *testing.T) {
	t.Parallel()
	store, conn := newScriptedStore(t,
		sizeResult(900_000_000),
		scriptedResult{rowsAffected: 3}, // the work
		sizeResult(910_000_000),
	)
	c := TradeChunk{Schema: "_timescaledb_internal", Name: "_hyper_1_12_chunk", Compressed: false, UncompressedBytes: 900_000_000}
	hooked := false
	res, err := store.RestampTradesChunk(context.Background(), c, func(ctx context.Context) error {
		_, err := store.db.ExecContext(ctx, "UPDATE trades SET usd_volume = 1")
		return err
	}, func(ChunkRestampStep) { hooked = true })
	if hooked {
		t.Error("the hook fired for a chunk that is neither decompressed nor compressed")
	}
	for _, s := range conn.statements() {
		if strings.Contains(s, "compress_chunk(") {
			t.Errorf("an uncompressed chunk was (de)compressed: %q", s)
		}
	}
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := conn.statements(); len(got) != 3 || !strings.Contains(got[1], "UPDATE trades") {
		t.Fatalf("issued %d statements, want 3 (size, the work, size):\n%s", len(got), strings.Join(got, "\n"))
	}
	if res.BytesBefore != 900_000_000 || res.BytesDecompressed != 900_000_000 || res.BytesAfter != 910_000_000 {
		t.Errorf("sizes = %d / %d / %d", res.BytesBefore, res.BytesDecompressed, res.BytesAfter)
	}
}

func TestTradesCompressionPolicy(t *testing.T) {
	t.Parallel()
	cols := []string{"job_id", "scheduled", "compress_after_seconds"}
	store, conn := newScriptedStore(t, scriptedResult{cols: cols, rows: [][]driver.Value{{int64(1000), true, int64(604800)}}})
	p, err := store.TradesCompressionPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.JobID != 1000 || !p.Scheduled || p.CompressAfter != 7*24*time.Hour {
		t.Errorf("policy = %+v", p)
	}
	stmt := conn.only(t)
	for _, want := range []string{
		"timescaledb_information.jobs",
		"proc_name = 'policy_compression'",
		"hypertable_name = 'trades'",
		"config->>'compress_after'",
	} {
		if !strings.Contains(stmt.sql, want) {
			t.Errorf("policy SQL lacks %q:\n%s", want, stmt.sql)
		}
	}

	// No job at all is a named error, never a zero policy.
	store, _ = newScriptedStore(t, scriptedResult{cols: cols})
	if _, err := store.TradesCompressionPolicy(context.Background()); !errors.Is(err, ErrNoTradesCompressionPolicy) {
		t.Errorf("no job: err = %v, want ErrNoTradesCompressionPolicy", err)
	}
	// A job without a lag in its config is refused too: the caller sizes
	// the live-adjacent refusal on it.
	store, _ = newScriptedStore(t, scriptedResult{cols: cols, rows: [][]driver.Value{{int64(1000), true, nil}}})
	if _, err := store.TradesCompressionPolicy(context.Background()); err == nil || !strings.Contains(err.Error(), "compress_after") {
		t.Errorf("no compress_after: err = %v", err)
	}
}

func TestSetJobScheduled(t *testing.T) {
	t.Parallel()
	store, conn := newScriptedStore(t, scriptedResult{}, scriptedResult{})
	ctx := context.Background()
	if err := store.SetJobScheduled(ctx, 1000, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetJobScheduled(ctx, 1000, true); err != nil {
		t.Fatal(err)
	}
	if len(conn.stmts) != 2 {
		t.Fatalf("issued %d statements, want 2", len(conn.stmts))
	}
	for i, want := range []bool{false, true} {
		s := conn.stmts[i]
		if !strings.Contains(s.sql, "alter_job(") || !strings.Contains(s.sql, "scheduled =>") {
			t.Errorf("stmt %d = %q, want alter_job(..., scheduled => ...)", i, s.sql)
		}
		if s.arg(t, 1) != 1000 || s.arg(t, 2) != want {
			t.Errorf("stmt %d args = %v, want (1000, %v)", i, s.args, want)
		}
	}
}

// TestRestampTradesChunk_RecompressesWhenWorkPanics: the re-compress is a
// deferred call, not a tail call, so a Go panic inside the work still puts
// the chunk back before the panic propagates. Without the defer a panic
// would leave 160 GB decompressed with nothing printed.
func TestRestampTradesChunk_RecompressesWhenWorkPanics(t *testing.T) {
	t.Parallel()
	store, conn := newScriptedStore(t,
		sizeResult(280_000_000),
		scriptedResult{}, // decompress_chunk
		sizeResult(4_200_000_000),
		scriptedResult{}, // compress_chunk — issued by the deferred half
		sizeResult(275_000_000),
	)
	c := TradeChunk{Schema: "_timescaledb_internal", Name: "_hyper_1_10_chunk", Compressed: true}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = store.RestampTradesChunk(context.Background(), c, func(context.Context) error {
			panic("nil map write in the planner")
		}, nil)
	}()
	if recovered == nil {
		t.Fatal("the panic was swallowed; it must propagate after the re-compress")
	}
	got := conn.statements()
	if len(got) != 5 || !strings.Contains(got[3], "compress_chunk(") || strings.Contains(got[3], "decompress_chunk(") {
		t.Fatalf("statements after a panicking work:\n%s\nwant the re-compress as statement 4 of 5", strings.Join(got, "\n"))
	}
}

// TestApplyXLMBaseUSDVolumeRestampInChunk_GuardsEveryBatch: ahead of EVERY
// batch the chunk's is_compressed is read; the moment it reads true the
// loop stops with [ErrTradesChunkRecompressed], the rows so far committed,
// and the next UPDATE — the one that would decompress per row — never
// issued.
func TestApplyXLMBaseUSDVolumeRestampInChunk_GuardsEveryBatch(t *testing.T) {
	t.Parallel()
	isCompressed := func(v bool) scriptedResult {
		return scriptedResult{cols: []string{"is_compressed"}, rows: [][]driver.Value{{v}}}
	}
	c := TradeChunk{Schema: "_timescaledb_internal", Name: "_hyper_1_10_chunk", Compressed: true}
	ts := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	plan := &XLMBaseRestampPlan{}
	for i := range 3 {
		plan.Rows = append(plan.Rows, XLMBaseRestampRow{Source: "sdex", Ledger: uint32(i + 1), TxHash: "ab", TS: ts, Want: "5.00000000"}) //nolint:gosec // tiny loop index
	}

	// Happy path: two batches, a guard read ahead of each, both carrying the
	// chunk's own identifiers.
	store, conn := newScriptedStore(t,
		isCompressed(false), scriptedResult{}, scriptedResult{rowsAffected: 2},
		isCompressed(false), scriptedResult{}, scriptedResult{rowsAffected: 1},
	)
	n, err := store.ApplyXLMBaseUSDVolumeRestampInChunk(context.Background(), c, plan, 1_756_800_000, 2)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v, want 3 rows and no error", n, err)
	}
	got := conn.statements()
	if len(got) != 6 {
		t.Fatalf("issued %d statements, want 6 (guard, SET LOCAL, UPDATE) x 2:\n%s", len(got), strings.Join(got, "\n"))
	}
	for _, i := range []int{0, 3} {
		g := conn.stmts[i]
		if !strings.Contains(g.sql, "timescaledb_information.chunks") || !strings.Contains(g.sql, "is_compressed") {
			t.Errorf("statement %d = %q, want the is_compressed read", i, g.sql)
		}
		if g.arg(t, 1) != c.Schema || g.arg(t, 2) != c.Name {
			t.Errorf("guard %d args = %v, want (%s, %s)", i, g.args, c.Schema, c.Name)
		}
		if !strings.Contains(got[i+2], "UPDATE trades") {
			t.Errorf("statement %d = %q, want the batch's UPDATE after its guard", i+2, got[i+2])
		}
	}
	if conn.commits != 2 {
		t.Errorf("commits = %d, want one per batch", conn.commits)
	}

	// The chunk is taken back between batch 1 and batch 2.
	store, conn = newScriptedStore(t,
		isCompressed(false), scriptedResult{}, scriptedResult{rowsAffected: 2},
		isCompressed(true),
	)
	n, err = store.ApplyXLMBaseUSDVolumeRestampInChunk(context.Background(), c, plan, 1_756_800_000, 2)
	if !errors.Is(err, ErrTradesChunkRecompressed) {
		t.Fatalf("err = %v, want ErrTradesChunkRecompressed", err)
	}
	for _, want := range []string{"_timescaledb_internal._hyper_1_10_chunk", "is_compressed = true", "NOT written"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want containing %q", err, want)
		}
	}
	if n != 2 {
		t.Errorf("n = %d, want the 2 rows batch 1 committed", n)
	}
	got = conn.statements()
	if len(got) != 4 || strings.Count(strings.Join(got, "\n"), "UPDATE trades") != 1 {
		t.Fatalf("after the guard tripped:\n%s\nwant exactly 4 statements and ONE update", strings.Join(got, "\n"))
	}
	if conn.commits != 1 {
		t.Errorf("commits = %d, want batch 1's only", conn.commits)
	}
}

// TestTryUSDVolumeRestampLock pins the lock's spelling — a session lock
// keyed on hashtext of the run's name, taken with the non-blocking form —
// and that a held lock is the named error, never a wait.
func TestTryUSDVolumeRestampLock(t *testing.T) {
	t.Parallel()
	lockResult := func(v bool) scriptedResult {
		return scriptedResult{cols: []string{"pg_try_advisory_lock"}, rows: [][]driver.Value{{v}}}
	}
	store, conn := newScriptedStore(t, lockResult(true), lockResult(true))
	release, err := store.TryUSDVolumeRestampLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	got := conn.stmts
	if len(got) != 2 {
		t.Fatalf("issued %d statements, want 2 (try lock, unlock):\n%s", len(got), strings.Join(conn.statements(), "\n"))
	}
	if !strings.Contains(got[0].sql, "pg_try_advisory_lock(hashtext(") || strings.Contains(got[0].sql, "pg_advisory_lock(") {
		t.Errorf("lock statement = %q, want the NON-blocking pg_try_advisory_lock on hashtext", got[0].sql)
	}
	if !strings.Contains(got[1].sql, "pg_advisory_unlock(hashtext(") {
		t.Errorf("unlock statement = %q", got[1].sql)
	}
	for i, s := range got {
		if s.arg(t, 1) != "usd-volume-restamp:trades" {
			t.Errorf("statement %d keyed on %v, want usd-volume-restamp:trades", i, s.arg(t, 1))
		}
	}

	store, conn = newScriptedStore(t, lockResult(false))
	if _, err := store.TryUSDVolumeRestampLock(context.Background()); !errors.Is(err, ErrUSDVolumeRestampLockHeld) {
		t.Fatalf("held: err = %v, want ErrUSDVolumeRestampLockHeld", err)
	}
	if n := len(conn.stmts); n != 1 {
		t.Errorf("a refused lock issued %d statements, want 1 (no unlock, no retry)", n)
	}
}

func TestJobRunning(t *testing.T) {
	t.Parallel()
	cols := []string{"job_status", "last_run_status"}
	cases := []struct {
		row  []driver.Value
		want bool
	}{
		{[]driver.Value{"Running", nil}, true},       // 2.26: a run in flight — job_status says so, last_run_status is NULL
		{[]driver.Value{"Running", "Success"}, true}, // in flight with an earlier outcome on record
		{[]driver.Value{"Paused", "Running"}, true},  // a version that reports the run in last_run_status
		{[]driver.Value{"Scheduled", "Success"}, false},
		{[]driver.Value{"Paused", nil}, false}, // never ran
	}
	for _, tc := range cases {
		store, conn := newScriptedStore(t, scriptedResult{cols: cols, rows: [][]driver.Value{tc.row}})
		got, err := store.JobRunning(context.Background(), 1000)
		if err != nil || got != tc.want {
			t.Errorf("row %v: running=%v err=%v, want %v", tc.row, got, err, tc.want)
		}
		stmt := conn.only(t)
		if !strings.Contains(stmt.sql, "timescaledb_information.job_stats") || stmt.arg(t, 1) != 1000 {
			t.Errorf("SQL = %q args = %v", stmt.sql, stmt.args)
		}
	}
	// No stats row at all: never ran, not running.
	store, _ := newScriptedStore(t, scriptedResult{cols: cols})
	if got, err := store.JobRunning(context.Background(), 1000); err != nil || got {
		t.Errorf("no row: running=%v err=%v", got, err)
	}
}
