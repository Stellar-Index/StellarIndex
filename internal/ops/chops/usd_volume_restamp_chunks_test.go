// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The walk that drives the chunk bracket. The bracket itself is pinned on the
// scripted driver in internal/storage/timescale/trades_chunks_test.go.

// fakeChunkStore holds DIRTY rows by timestamp; a plan returns the unapplied
// ones in its window and an apply marks them done, which is the real
// planner's idempotence and makes "a rerun skips finished chunks" observable.
type fakeChunkStore struct {
	chunks []timescale.TradeChunk
	path   string
	dirty  []time.Time
	done   map[time.Time]bool

	failApplyAt   int // the n-th Apply (1-based) fails
	applies       int
	cancelInApply context.CancelFunc // SIGTERM arriving mid-chunk

	policy         timescale.TradesCompressionPolicy
	policyErr      error
	scheduleCtxErr []error // ctx.Err() seen by each SetJobScheduled
	// relistCompressed overrides Compressed in listings taken while paused.
	paused           bool
	relistCompressed map[string]bool
	errw             *bytes.Buffer
	errAtPause       string // errw's content when the pause was issued

	lockHeld             bool
	unlockCtxErr         error
	runningPolls         int // JobRunning answers true this many times
	recompressUnderneath string

	// bmu guards only the byte-poll group: the poll runs on its own goroutine,
	// so nothing here touches log.
	bmu sync.Mutex
	// byteScript's last entry repeats, so the observed movement is a fixed
	// total however often the poll fires. Empty = the listed size, unchanging.
	byteScript []int64
	byteReads  int
	// RestampTradesChunk holds the "decompress" open until this closes.
	byteScriptDrained chan struct{}
	onWork            func() // runs after the decompress, before any row

	log []string
}

func (f *fakeChunkStore) TradesChunkBytes(_ context.Context, c timescale.TradeChunk) (int64, error) {
	f.bmu.Lock()
	defer f.bmu.Unlock()
	if len(f.byteScript) == 0 {
		return c.UncompressedBytes, nil
	}
	i := f.byteReads
	if i >= len(f.byteScript) {
		i = len(f.byteScript) - 1
	}
	f.byteReads++
	if f.byteReads == len(f.byteScript) && f.byteScriptDrained != nil {
		close(f.byteScriptDrained)
	}
	return f.byteScript[i], nil
}

func (f *fakeChunkStore) reads() int {
	f.bmu.Lock()
	defer f.bmu.Unlock()
	return f.byteReads
}

func newFakeChunkStore(chunks []timescale.TradeChunk, dirty ...time.Time) *fakeChunkStore {
	return &fakeChunkStore{
		chunks: chunks, path: "/var/lib/postgresql/data", dirty: dirty, done: map[time.Time]bool{},
		policy: timescale.TradesCompressionPolicy{JobID: 1000, Scheduled: true, CompressAfter: 7 * 24 * time.Hour},
	}
}

func (f *fakeChunkStore) TradesCompressionPolicy(context.Context) (timescale.TradesCompressionPolicy, error) {
	f.log = append(f.log, "policy")
	if f.policyErr != nil {
		return timescale.TradesCompressionPolicy{}, f.policyErr
	}
	return f.policy, nil
}

func (f *fakeChunkStore) SetJobScheduled(ctx context.Context, jobID int, scheduled bool) error {
	verb := "pause"
	if scheduled {
		verb = "resume"
	}
	f.log = append(f.log, fmt.Sprintf("%s job %d", verb, jobID))
	f.scheduleCtxErr = append(f.scheduleCtxErr, ctx.Err())
	f.paused = !scheduled
	if !scheduled && f.errw != nil {
		f.errAtPause = f.errw.String()
	}
	return nil
}

func (f *fakeChunkStore) JobRunning(context.Context, int) (bool, error) {
	f.log = append(f.log, "job-status")
	if f.runningPolls > 0 {
		f.runningPolls--
		return true, nil
	}
	return false, nil
}

func (f *fakeChunkStore) TryUSDVolumeRestampLock(context.Context) (func(context.Context) error, error) {
	if f.lockHeld {
		f.log = append(f.log, "lock held")
		return nil, timescale.ErrUSDVolumeRestampLockHeld
	}
	f.log = append(f.log, "lock")
	return func(ctx context.Context) error {
		f.log = append(f.log, "unlock")
		f.unlockCtxErr = ctx.Err()
		return nil
	}, nil
}

func (f *fakeChunkStore) ApplyXLMBaseUSDVolumeRestampInChunk(ctx context.Context, c timescale.TradeChunk, plan *timescale.XLMBaseRestampPlan, generation int64, batch int) (int64, error) {
	if c.Name == f.recompressUnderneath {
		f.log = append(f.log, "apply refused in-chunk="+c.Name)
		return 0, fmt.Errorf("%w: %s reads is_compressed = true", timescale.ErrTradesChunkRecompressed, c)
	}
	n, err := f.ApplyXLMBaseUSDVolumeRestamp(ctx, plan, generation, batch)
	f.log[len(f.log)-1] += " in-chunk=" + c.Name
	return n, err
}

func (f *fakeChunkStore) TradesChunksInRange(_ context.Context, from, to time.Time) ([]timescale.TradeChunk, error) {
	f.log = append(f.log, fmt.Sprintf("list [%s, %s)", from.Format(time.DateOnly), to.Format(time.DateOnly)))
	var out []timescale.TradeChunk
	for _, c := range f.chunks {
		if !(c.RangeStart.Before(to) && c.RangeEnd.After(from)) {
			continue
		}
		if v, ok := f.relistCompressed[c.Name]; ok && f.paused {
			c.Compressed = v
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeChunkStore) TradesDataVolumePath(context.Context) (string, error) {
	if f.path == "" {
		return "", errors.New("permission denied to read data_directory")
	}
	return f.path, nil
}

// RestampTradesChunk mirrors the store contract: a chunk compressed at
// listing is bracketed (hook before each half); one that was not is left as is.
func (f *fakeChunkStore) RestampTradesChunk(ctx context.Context, c timescale.TradeChunk, work func(context.Context) error, before func(timescale.ChunkRestampStep)) (timescale.TradeChunkRestampResult, error) {
	if before == nil {
		before = func(timescale.ChunkRestampStep) {}
	}
	if !c.Compressed {
		f.log = append(f.log, "work-in-place "+c.Name)
		werr := work(ctx)
		res := timescale.TradeChunkRestampResult{Chunk: c, BytesBefore: c.UncompressedBytes, BytesDecompressed: c.UncompressedBytes, BytesAfter: c.UncompressedBytes}
		if werr != nil {
			return res, fmt.Errorf("chunk %s restamp failed (left uncompressed): %w", c, werr)
		}
		return res, nil
	}
	before(timescale.ChunkRestampDecompress)
	f.log = append(f.log, "decompress "+c.Name)
	// Bounded, so a walk that never polls fails its assertions instead of
	// hanging the suite.
	if f.byteScriptDrained != nil {
		timeout := time.NewTimer(3 * time.Second)
		select {
		case <-f.byteScriptDrained:
		case <-timeout.C:
		case <-ctx.Done():
		}
		timeout.Stop()
	}
	if f.onWork != nil {
		f.onWork()
	}
	werr := work(ctx)
	before(timescale.ChunkRestampCompress)
	f.log = append(f.log, "compress "+c.Name)
	res := timescale.TradeChunkRestampResult{Chunk: c, BytesBefore: c.CompressedBytes, BytesDecompressed: c.UncompressedBytes, BytesAfter: c.CompressedBytes}
	if werr != nil {
		return res, fmt.Errorf("chunk %s restamp failed (re-compressed): %w", c, werr)
	}
	return res, nil
}

func (f *fakeChunkStore) PlanXLMBaseUSDVolumeRestamp(_ context.Context, p timescale.XLMBaseRestampParams) (*timescale.XLMBaseRestampPlan, error) {
	f.log = append(f.log, fmt.Sprintf("plan [%s, %s)", p.From.Format("01-02T15"), p.To.Format("01-02T15")))
	plan := &timescale.XLMBaseRestampPlan{Stats: timescale.NewXLMBaseRestampStats()}
	for _, ts := range f.dirty {
		if ts.Before(p.From) || !ts.Before(p.To) {
			continue
		}
		stored := "0.10000000"
		row := timescale.XLMBaseRestampRow{Source: "sdex", Ledger: 1, TxHash: "ab", TS: ts, Stored: &stored, Want: "5.00000000"}
		if f.done[ts] {
			plan.Record(row, 1) // unchanged: already holds the anchor's value
			continue
		}
		plan.Record(row, 0) // write
	}
	return plan, nil
}

func (f *fakeChunkStore) ApplyXLMBaseUSDVolumeRestamp(_ context.Context, plan *timescale.XLMBaseRestampPlan, generation int64, batch int) (int64, error) {
	f.applies++
	f.log = append(f.log, fmt.Sprintf("apply %d rows gen=%d batch=%d", len(plan.Rows), generation, batch))
	if f.failApplyAt > 0 && f.applies == f.failApplyAt {
		if f.cancelInApply != nil {
			f.cancelInApply()
		}
		return 0, errors.New("update: deadlock detected")
	}
	for _, r := range plan.Rows {
		f.done[r.TS] = true
	}
	return int64(len(plan.Rows)), nil
}

// index is the position of the first log line with the prefix, or -1.
func (f *fakeChunkStore) index(prefix string) int {
	for i, l := range f.log {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

func (f *fakeChunkStore) count(prefix string) int {
	n := 0
	for _, l := range f.log {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func chunkFixture(name string, start, end time.Time, uncompressed, compressed int64) timescale.TradeChunk {
	return timescale.TradeChunk{
		Schema: "_timescaledb_internal", Name: name, RangeStart: start, RangeEnd: end,
		Compressed: compressed > 0, UncompressedBytes: uncompressed, CompressedBytes: compressed,
	}
}

// Three weekly chunks; the window [Jan 5, Jan 18] starts inside the first and
// ends inside the last, so the clamping is observable.
func threeChunks() ([]timescale.TradeChunk, time.Time, time.Time) {
	d := func(day int) time.Time { return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC) }
	chunks := []timescale.TradeChunk{
		chunkFixture("_hyper_1_1_chunk", d(1), d(8), 4<<30, 300<<20),
		chunkFixture("_hyper_1_2_chunk", d(8), d(15), 160<<30, 10<<30),
		chunkFixture("_hyper_1_3_chunk", d(15), d(22), 2<<30, 200<<20),
	}
	return chunks, d(5), d(18)
}

func chunkTestOptions(write bool) (xlmBaseRestampOptions, chunkRestampOptions, *bytes.Buffer) {
	var out bytes.Buffer
	opts := xlmBaseRestampOptions{
		Slice: 24 * time.Hour, Batch: 2000, Write: write, SampleSize: 3,
		MaxGeneration: 1_756_800_000, Generation: 1_756_800_000,
	}
	copts := chunkRestampOptions{
		Batch:      20_000,
		FreeBytes:  func(string) (uint64, error) { return 4_690 << 30, nil },
		Now:        time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), // far past the policy's 7-day lag
		Out:        &out,
		Err:        io.Discard,
		PolicyPoll: time.Millisecond,
	}
	return opts, copts, &out
}

// wantTeardown asserts the policy is re-enabled THEN the lock released, so a
// run waiting on the lock never inherits a paused policy.
func wantTeardown(t *testing.T, store *fakeChunkStore) {
	t.Helper()
	n := len(store.log)
	if n < 2 || store.log[n-2] != "resume job 1000" || store.log[n-1] != "unlock" {
		t.Errorf("store log does not end with the policy re-enable then the unlock:\n%s", strings.Join(store.log, "\n"))
	}
}

func TestXLMBaseChunkRestamp_DryRunPrintsThePlanAndTouchesNoChunk(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 6))
	opts, copts, out := chunkTestOptions(false)

	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"chunk plan: 3 trades chunk(s) intersect [2026-01-05, 2026-01-18]",
		"3 compressed",
		"uncompressed 166.0 GB total",
		"largest 160.0 GB",
		"_timescaledb_internal._hyper_1_2_chunk",
		"compressed 10.5 GB total",
		"pre-flight: free 4.6 TB on /var/lib/postgresql/data (measured; re-measured before every decompress)",
		"need > 620.0 GB (the disk-watchdog floor plus 2.0x the largest chunk's uncompressed size; a guard, not a bound) — OK",
		"would restamp 2 row(s)",
		"DRY RUN: would take session advisory lock hashtext('usd-volume-restamp:trades')",
		"pause compression policy job 1000 on trades (scheduled=true, compress_after=168h0m0s)",
		"DRY RUN: nothing is decompressed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, got)
		}
	}
	if n := store.count("decompress "); n != 0 {
		t.Errorf("dry run decompressed %d chunk(s):\n%s", n, strings.Join(store.log, "\n"))
	}
	if n := store.count("apply "); n != 0 {
		t.Errorf("dry run applied %d plan(s)", n)
	}
	if store.count("plan ") == 0 {
		t.Error("dry run planned nothing — it must still report the rows it would change")
	}
	if len(store.done) != 0 {
		t.Error("dry run wrote rows")
	}
	if store.count("pause job") != 0 || store.count("resume job") != 0 {
		t.Errorf("the dry run touched the compression policy:\n%s", strings.Join(store.log, "\n"))
	}
}

func TestXLMBaseChunkRestamp_BracketsEachChunkAndScopesEveryScanToIt(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
	opts, copts, out := chunkTestOptions(true)

	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(store.done) != 3 {
		t.Fatalf("applied %d row(s), want 3", len(store.done))
	}
	log := strings.Join(store.log, "\n")
	var brackets []string
	open := ""
	for _, l := range store.log {
		switch {
		case strings.HasPrefix(l, "decompress "):
			if open != "" {
				t.Fatalf("chunk %s decompressed while %s was still open", l, open)
			}
			open = strings.TrimPrefix(l, "decompress ")
			brackets = append(brackets, open)
		case strings.HasPrefix(l, "compress "):
			if name := strings.TrimPrefix(l, "compress "); name != open {
				t.Fatalf("compress %s while %q was open", name, open)
			}
			open = ""
		case strings.HasPrefix(l, "apply "):
			if open == "" {
				t.Fatalf("apply outside a bracket: %s\n%s", l, log)
			}
			if !strings.Contains(l, "batch=20000") {
				t.Errorf("apply inside a decompressed chunk used %s, want the chunk batch (20000)", l)
			}
			if !strings.HasSuffix(l, " in-chunk="+open) {
				t.Errorf("apply inside %s bypassed the guarded in-chunk apply: %s", open, l)
			}
		}
	}
	// Lock, then a single policy pause, both before the first decompress.
	lock, pause, first := store.index("lock"), store.index("pause job 1000"), store.index("decompress ")
	if lock < 0 || pause < 0 || first < 0 || lock > pause || pause > first {
		t.Errorf("order of lock (%d), pause (%d), first decompress (%d):\n%s", lock, pause, first, log)
	}
	if n := store.count("pause job"); n != 1 {
		t.Errorf("policy paused %d time(s), want exactly once", n)
	}
	wantTeardown(t, store)
	if want := []string{"_hyper_1_1_chunk", "_hyper_1_2_chunk", "_hyper_1_3_chunk"}; strings.Join(brackets, ",") != strings.Join(want, ",") {
		t.Errorf("brackets = %v, want %v", brackets, want)
	}
	// The policy is read again under the lock and the chunks listed again
	// after the pause, so the state restored is one nothing else is changing.
	if n := store.count("policy"); n != 2 || store.log[lock+1] != "policy" {
		t.Errorf("policy read %d time(s); want a second read right after the lock:\n%s", n, log)
	}
	lists := []int{}
	for i, l := range store.log {
		if strings.HasPrefix(l, "list ") {
			lists = append(lists, i)
		}
	}
	if len(lists) != 2 || lists[1] < pause {
		t.Errorf("chunk listings at %v, want two with the second after the pause (%d):\n%s", lists, pause, log)
	}

	// Every scan is bounded by the open chunk AND the run window; the first
	// and last chunks overhang the window by 4 days and neither is scanned.
	windowEnd := to.AddDate(0, 0, 1)
	open = ""
	for _, l := range store.log {
		if strings.HasPrefix(l, "decompress ") {
			open = strings.TrimPrefix(l, "decompress ")
			continue
		}
		if strings.HasPrefix(l, "compress ") {
			open = ""
			continue
		}
		if !strings.HasPrefix(l, "plan ") || open == "" {
			continue
		}
		lo, hi := parsePlanWindow(t, l)
		var chunk timescale.TradeChunk
		for _, c := range chunks {
			if c.Name == open {
				chunk = c
			}
		}
		if lo.Before(chunk.RangeStart) || hi.After(chunk.RangeEnd) {
			t.Errorf("scan %s escapes open chunk %s [%s, %s)", l, open, chunk.RangeStart.Format(time.DateOnly), chunk.RangeEnd.Format(time.DateOnly))
		}
		if lo.Before(from) || hi.After(windowEnd) {
			t.Errorf("scan %s escapes the run window [%s, %s)", l, from.Format(time.DateOnly), windowEnd.Format(time.DateOnly))
		}
	}
	got := out.String()
	for _, want := range []string{
		"chunk 1/3 _timescaledb_internal._hyper_1_1_chunk",
		"chunk 3/3 _timescaledb_internal._hyper_1_3_chunk",
		"changed 1 row(s)",
		"300.0 MB -> 4.0 GB -> 300.0 MB",
		"restamped 3 row(s)",
		"refresh_continuous_aggregate('prices_1m'",
		// -day is the LAST day and -days counts back: exactly [01-05, 01-18].
		"acceptance: stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -day 2026-01-18 -days 14",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, got)
		}
	}
}

func brackets0(store *fakeChunkStore) int { return store.index("decompress ") }

// parsePlanWindow reads the window back out of a fake "plan [a, b)" line.
func parsePlanWindow(t *testing.T, l string) (time.Time, time.Time) {
	t.Helper()
	inner := strings.TrimSuffix(strings.TrimPrefix(l, "plan ["), ")")
	parts := strings.Split(inner, ", ")
	if len(parts) != 2 {
		t.Fatalf("unparseable plan line %q", l)
	}
	parse := func(s string) time.Time {
		ts, err := time.Parse("2006-01-02T15", "2026-"+s)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	return parse(parts[0]), parse(parts[1])
}

func TestXLMBaseChunkRestamp_RerunSkipsChunksAlreadyAtGeneration(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), to.Add(2*time.Hour)) // chunks 1 and 3 dirty, 2 clean
	opts, copts, out := chunkTestOptions(true)
	ctx := context.Background()

	if err := runXLMBaseChunkRestamp(ctx, store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatal(err)
	}
	if got := store.count("decompress "); got != 2 {
		t.Fatalf("first run decompressed %d chunk(s), want 2 (the clean middle chunk is probed and skipped):\n%s",
			got, strings.Join(store.log, "\n"))
	}
	if !strings.Contains(out.String(), "chunk 2/3 _timescaledb_internal._hyper_1_2_chunk") ||
		!strings.Contains(out.String(), "nothing to change — skipped, chunk left compressed") {
		t.Errorf("the clean chunk was not reported as skipped:\n%s", out.String())
	}

	store.log = nil
	out.Reset()
	if err := runXLMBaseChunkRestamp(ctx, store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatal(err)
	}
	if got := store.count("decompress "); got != 0 {
		t.Errorf("rerun decompressed %d chunk(s), want 0:\n%s", got, strings.Join(store.log, "\n"))
	}
	if got := store.count("apply "); got != 0 {
		t.Errorf("rerun applied %d plan(s), want 0", got)
	}
	if n := strings.Count(out.String(), "skipped, chunk left compressed"); n != 3 {
		t.Errorf("rerun reported %d skipped chunk(s), want 3:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "restamped 0 row(s)") {
		t.Errorf("rerun summary:\n%s", out.String())
	}
}

// A chunk skipped as clean is still inside the reported window, so its rows
// are counted, as the day walk and the dry run count them.
func TestXLMBaseChunkRestamp_ReportCountsTheRowsOfASkippedChunk(t *testing.T) {
	chunks, from, to := threeChunks()
	clean := from.AddDate(0, 0, 4) // Jan 9, inside chunk 2
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), clean)
	store.done[clean] = true
	opts, copts, out := chunkTestOptions(true)
	run := newXLMBaseRestampRun(store, opts)
	run.batch = copts.Batch
	inChunk := func(ctx context.Context, c timescale.TradeChunk, plan *timescale.RestampPlan, generation int64, batch int) (int64, error) {
		return store.ApplyXLMBaseUSDVolumeRestampInChunk(ctx, c, plan, generation, batch)
	}
	tier := &estimatedChunkTier{run: run, opts: opts, inChunk: inChunk}
	if err := runChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, copts, tier); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "chunk 2/3 _timescaledb_internal._hyper_1_2_chunk") ||
		!strings.Contains(out.String(), "nothing to change — skipped") {
		t.Fatalf("chunk 2 was not skipped as clean:\n%s", out.String())
	}
	if s := run.totals; s.Scanned != 2 || s.Changed != 1 || s.Unchanged != 1 || s.Residual() != 0 {
		t.Errorf("report totals: scanned %d changed %d already-correct %d residual %d; want 2/1/1/0 (the skipped chunk's row counted)",
			s.Scanned, s.Changed, s.Unchanged, s.Residual())
	}
}

func TestXLMBaseChunkRestamp_FailureStopsAfterTheFailingChunk(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
	store.failApplyAt = 2 // the middle chunk's apply
	opts, copts, out := chunkTestOptions(true)

	err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
	if err == nil || !strings.Contains(err.Error(), "deadlock detected") {
		t.Fatalf("err = %v, want the apply failure", err)
	}
	log := strings.Join(store.log, "\n")
	if store.index("pause job 1000") < 0 {
		t.Errorf("the policy was never paused:\n%s", log)
	}
	if !strings.Contains(log, "decompress _hyper_1_2_chunk\n") || !strings.HasSuffix(log, "compress _hyper_1_2_chunk\nresume job 1000\nunlock") {
		t.Errorf("log does not end with the failing chunk's re-compress, the policy re-enable and the unlock:\n%s", log)
	}
	if strings.Contains(log, "_hyper_1_3_chunk") {
		t.Errorf("the walk continued past the failure:\n%s", log)
	}
	if !strings.Contains(out.String(), "RESUME:") || !strings.Contains(out.String(), "-chunks") ||
		!strings.Contains(out.String(), "-generation 1756800000") {
		t.Errorf("no resume hint carrying -chunks and the run's generation:\n%s", out.String())
	}
}

func TestXLMBaseChunkRestamp_PreflightRefusesAWriteRunBeforeAnyDecompress(t *testing.T) {
	chunks, from, to := threeChunks()
	statfsErr := errors.New("statfs: no such file or directory")
	cases := []struct {
		name    string
		free    uint64
		freeErr error
		path    string
		minFree int64
		wantErr string
		wantOut string
	}{
		{
			name: "measured, too little", free: 200 << 30, path: "/var/lib/postgresql/data",
			wantErr: "free space 200.0 GB on /var/lib/postgresql/data is not more than 620.0 GB",
		},
		{
			// The watchdog's 300 GiB floor plus this chunk's 320 GiB headroom.
			name: "exactly floor+headroom is not more than", free: 620 << 30, path: "/var/lib/postgresql/data",
			wantErr: "is not more than 620.0 GB",
		},
		{name: "unmeasurable and no override", freeErr: statfsErr, path: "/var/lib/postgresql/data", wantErr: "-min-free-bytes"},
		{name: "data directory unreadable and no override", path: "", wantErr: "-min-free-bytes"},
		{
			name: "override too small", freeErr: statfsErr, path: "/var/lib/postgresql/data", minFree: 100 << 30,
			wantErr: "free space 100.0 GB (-min-free-bytes, NOT measured)",
		},
		{
			// Clears max(floor, headroom)=320 GiB but not floor+headroom=620 GiB.
			name: "override just under floor+headroom", freeErr: statfsErr, path: "/var/lib/postgresql/data", minFree: 600 << 30,
			wantErr: "free space 600.0 GB (-min-free-bytes, NOT measured) is not more than 620.0 GB",
		},
		{
			name: "override large enough", freeErr: statfsErr, path: "/var/lib/postgresql/data", minFree: 700 << 30,
			wantOut: "WARNING: trusting -min-free-bytes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeChunkStore(chunks, from.Add(3*time.Hour))
			store.path = tc.path
			opts, copts, out := chunkTestOptions(true)
			copts.MinFreeBytes = tc.minFree
			copts.FreeBytes = func(string) (uint64, error) { return tc.free, tc.freeErr }

			err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				if store.count("decompress ") != 0 || store.count("plan ") != 0 || store.count("pause job") != 0 {
					t.Errorf("a refused run touched the store:\n%s", strings.Join(store.log, "\n"))
				}
				return
			}
			if err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output lacks %q:\n%s", tc.wantOut, out.String())
			}
		})
	}

	// A dry run prints the verdict it would refuse on and carries on read-only.
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour))
	opts, copts, out := chunkTestOptions(false)
	copts.FreeBytes = func(string) (uint64, error) { return 1 << 30, nil }
	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatalf("dry run refused: %v", err)
	}
	if !strings.Contains(out.String(), "PRE-FLIGHT WOULD REFUSE -write") || store.count("plan ") == 0 {
		t.Errorf("dry run under a failing pre-flight:\n%s", out.String())
	}
}

func TestClampTradesChunk(t *testing.T) {
	t.Parallel()
	d := func(day int) time.Time { return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC) }
	c := chunkFixture("_hyper_1_2_chunk", d(8), d(15), 1, 1)
	for _, tc := range []struct {
		name                     string
		from, to, wantLo, wantHi int
	}{
		{"chunk inside window", 5, 19, 8, 15},
		{"window inside chunk", 10, 12, 10, 12},
		{"window overlaps the chunk's start", 1, 9, 8, 9},
	} {
		if lo, hi := clampTradesChunk(c, d(tc.from), d(tc.to)); !lo.Equal(d(tc.wantLo)) || !hi.Equal(d(tc.wantHi)) {
			t.Errorf("%s: [%s, %s)", tc.name, lo, hi)
		}
	}
}

func caseValidateRestampChunkFlags(t *testing.T) {
	t.Parallel()
	if err := validateRestampChunkFlags(true, map[string]bool{"chunk-batch": true, "min-free-bytes": true, "generation": true, "allow-live-adjacent": true}); err != nil {
		t.Fatalf("-chunks with its own flags: %v", err)
	}
	if err := validateRestampChunkFlags(false, nil); err != nil {
		t.Fatalf("no chunk flags at all: %v", err)
	}
	for _, f := range restampChunkOnlyFlags {
		err := validateRestampChunkFlags(false, map[string]bool{f: true})
		if err == nil || !strings.Contains(err.Error(), "-"+f) {
			t.Errorf("-%s without -chunks: err = %v, want a refusal naming the flag", f, err)
		}
	}
	err := validateRestampChunkFlags(true, map[string]bool{"batch": true})
	if err == nil || !strings.Contains(err.Error(), "-chunk-batch") {
		t.Errorf("-batch with -chunks: err = %v, want a redirect to -chunk-batch", err)
	}
}

func TestFmtBytes(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]string{
		0:           "0 B",
		999:         "999 B",
		300 << 20:   "300.0 MB",
		4 << 30:     "4.0 GB",
		160 << 30:   "160.0 GB",
		4_690 << 30: "4.6 TB",
		25 << 30:    "25.0 GB",
		1536 << 30:  "1.5 TB",
		2_000_000:   "1.9 MB",
		-1:          "-1 B",
	} {
		if got := fmtBytes(n); got != want {
			t.Errorf("fmtBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// The trades compression policy would re-compress an open chunk mid-run and
// silently push later batches onto the per-row path, so it is paused for the
// run and re-enabled on every exit, on a context that survives cancellation.
func TestXLMBaseChunkRestamp_ReenablesTheCompressionPolicyOnACancelledContext(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.failApplyAt, store.cancelInApply = 2, cancel // SIGTERM lands mid-chunk
	opts, copts, _ := chunkTestOptions(true)

	err := runXLMBaseChunkRestamp(ctx, store, "/etc/stellarindex.toml", from, to, opts, copts)
	if err == nil {
		t.Fatal("a cancelled run returned nil")
	}
	wantTeardown(t, store)
	if n := len(store.scheduleCtxErr); n != 2 {
		t.Fatalf("SetJobScheduled called %d time(s), want 2 (pause, re-enable)", n)
	}
	if got := store.scheduleCtxErr[1]; got != nil {
		t.Errorf("the re-enable saw ctx.Err() = %v; it must run on a context that survives the cancellation", got)
	}
	if store.unlockCtxErr != nil {
		t.Errorf("the unlock saw ctx.Err() = %v; it must run on a context that survives the cancellation", store.unlockCtxErr)
	}
}

func TestXLMBaseChunkRestamp_RefusesToStartWithoutACompressionPolicy(t *testing.T) {
	chunks, from, to := threeChunks()
	for _, write := range []bool{true, false} {
		store := newFakeChunkStore(chunks, from.Add(3*time.Hour))
		store.policyErr = timescale.ErrNoTradesCompressionPolicy
		opts, copts, _ := chunkTestOptions(write)

		err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
		if err == nil || !errors.Is(err, timescale.ErrNoTradesCompressionPolicy) || !strings.Contains(err.Error(), "refuses to start") {
			t.Fatalf("write=%v: err = %v, want a refusal naming the missing policy", write, err)
		}
		if got := strings.Join(store.log, "\n"); got != "policy" {
			t.Errorf("write=%v: a refused run went on to touch the store:\n%s", write, got)
		}
	}
}

func TestCheckRestampLiveAdjacent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	const lag = 7 * 24 * time.Hour
	edge := now.Add(-lag) // 2026-08-28T12:00Z
	// -to 2026-08-27: the window ends 2026-08-28T00:00Z, before the edge.
	if adj, err := checkRestampLiveAdjacent(time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC), now, lag, false); adj || err != nil {
		t.Errorf("window ending before the edge: adjacent=%v err=%v", adj, err)
	}
	// -to 2026-08-28: the window ends 2026-08-29T00:00Z, past the edge.
	adj, err := checkRestampLiveAdjacent(time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC), now, lag, false)
	if !adj || err == nil {
		t.Fatalf("window ending past the edge: adjacent=%v err=%v", adj, err)
	}
	for _, want := range []string{"-to 2026-08-28", edge.Format(time.RFC3339), "-allow-live-adjacent", "in-place walk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want containing %q", err, want)
		}
	}
	if adj, err := checkRestampLiveAdjacent(time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC), now, lag, true); !adj || err != nil {
		t.Errorf("override: adjacent=%v err=%v", adj, err)
	}
	if adj, err := checkRestampLiveAdjacent(edge.Truncate(24*time.Hour).AddDate(0, 0, -1), edge.Truncate(24*time.Hour).Add(lag), lag, false); adj || err != nil {
		t.Errorf("window ending exactly on the edge: adjacent=%v err=%v", adj, err)
	}
}

func TestXLMBaseChunkRestamp_RefusesALiveAdjacentWindow(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour))
	opts, copts, _ := chunkTestOptions(true)
	copts.Now = to.AddDate(0, 0, 3) // inside the 7-day lag

	err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
	if err == nil || !strings.Contains(err.Error(), "-allow-live-adjacent") {
		t.Fatalf("err = %v, want the live-adjacent refusal", err)
	}
	if got := strings.Join(store.log, "\n"); got != "policy" {
		t.Errorf("a refused run went on to touch the store:\n%s", got)
	}

	var errw bytes.Buffer
	store = newFakeChunkStore(chunks, from.Add(3*time.Hour))
	copts.AllowLiveAdjacent, copts.Err = true, &errw
	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errw.String(), "WARNING: -allow-live-adjacent") {
		t.Errorf("no warning for the override:\n%s", errw.String())
	}
	if store.count("decompress ") != 1 || store.index("pause job") < 0 {
		t.Errorf("override run:\n%s", strings.Join(store.log, "\n"))
	}
}

func TestXLMBaseChunkRestamp_LeavesAnUncompressedChunkUncompressed(t *testing.T) {
	chunks, from, to := threeChunks()
	chunks[2] = chunkFixture("_hyper_1_3_chunk", chunks[2].RangeStart, chunks[2].RangeEnd, 2<<30, 0) // not compressed at listing
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), to.Add(2*time.Hour))                   // chunks 1 and 3 dirty
	opts, copts, out := chunkTestOptions(true)

	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	log := strings.Join(store.log, "\n")
	if strings.Contains(log, "decompress _hyper_1_3_chunk") || strings.Contains(log, "compress _hyper_1_3_chunk") {
		t.Errorf("the uncompressed chunk was (de)compressed:\n%s", log)
	}
	if !strings.Contains(log, "work-in-place _hyper_1_3_chunk") || len(store.done) != 2 {
		t.Errorf("the uncompressed chunk was not restamped in place (%d rows done):\n%s", len(store.done), log)
	}
	for _, want := range []string{
		"2 compressed, 1 not",
		"1 chunk(s) not compressed at listing are restamped in place and LEFT uncompressed",
		"chunk 3/3 _timescaledb_internal._hyper_1_3_chunk",
		"chunk left uncompressed (not compressed at listing)",
		"chunk 1/3 _timescaledb_internal._hyper_1_1_chunk",
		"300.0 MB -> 4.0 GB -> 300.0 MB",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestXLMBaseChunkRestamp_RechecksFreeSpaceBeforeEachDecompress(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5)) // chunks 1 and 2 dirty
	opts, copts, out := chunkTestOptions(true)
	calls := 0
	copts.FreeBytes = func(string) (uint64, error) {
		calls++
		if calls <= 2 { // the run pre-flight and chunk 1's re-check
			return 4_690 << 30, nil
		}
		return 100 << 30, nil // the pool filled up while chunk 1 ran
	}

	err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
	if err == nil || !strings.Contains(err.Error(), "chunk 2/3") || !strings.Contains(err.Error(), "pre-flight refused before the decompress") ||
		!strings.Contains(err.Error(), "free space 100.0 GB on /var/lib/postgresql/data is not more than 620.0 GB") {
		t.Fatalf("err = %v, want the per-chunk refusal on chunk 2", err)
	}
	log := strings.Join(store.log, "\n")
	if store.count("decompress ") != 1 || strings.Contains(log, "_hyper_1_2_chunk") && strings.Contains(log, "decompress _hyper_1_2_chunk") {
		t.Errorf("chunk 2 was decompressed despite the refusal:\n%s", log)
	}
	if calls != 3 {
		t.Errorf("free space measured %d time(s), want 3 (run pre-flight, chunk 1, chunk 2)", calls)
	}
	wantTeardown(t, store)
	if !strings.Contains(out.String(), "RESUME:") {
		t.Errorf("no resume hint after the refusal:\n%s", out.String())
	}
}

func TestXLMBaseChunkRestamp_PrintsTheByHandRepairBeforeEachDecompressAndRecompress(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
	opts, copts, out := chunkTestOptions(true)
	var errw bytes.Buffer
	copts.Err = &errw
	store.errw = &errw

	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	got := errw.String()
	const reenable = "SELECT alter_job(1000, scheduled => true);"
	pausing, firstDecompress := strings.Index(got, "PAUSING"), strings.Index(got, "decompressing")
	if pausing < 0 || firstDecompress < 0 || pausing > firstDecompress {
		t.Errorf("the policy notice is not before the first decompress (at %d vs %d):\n%s", pausing, firstDecompress, got)
	}
	if !strings.Contains(got[:firstDecompress], reenable) {
		t.Errorf("the re-enable SQL is not printed up front:\n%s", got)
	}
	// Already on stderr when the pause is issued: a SIGKILL between the two
	// would otherwise leave the policy paused with no trace of the repair.
	if !strings.Contains(store.errAtPause, reenable) {
		t.Errorf("the re-enable SQL was not on stderr when the pause was issued; stderr at that moment:\n%s", store.errAtPause)
	}
	for _, name := range []string{"_hyper_1_1_chunk", "_hyper_1_2_chunk", "_hyper_1_3_chunk"} {
		byHand := "SELECT compress_chunk('_timescaledb_internal." + name + "');"
		if n := strings.Count(got, byHand); n != 2 {
			t.Errorf("%s: the by-hand compress_chunk appears %d time(s) on stderr, want 2:\n%s", name, n, got)
		}
	}
	if n := strings.Count(got, reenable); n != 1+2*3 {
		t.Errorf("the re-enable SQL appears %d time(s) on stderr, want 7 (up front + twice per chunk)", n)
	}
	reenabled, unlocked := strings.Index(got, "compression policy job 1000 re-enabled"), strings.Index(got, "run lock released")
	if !strings.Contains(got, "STAYS DECOMPRESSED") || reenabled < 0 || unlocked < reenabled || !strings.HasSuffix(strings.TrimSpace(got), "run lock released") {
		t.Errorf("stderr lacks the re-compress warning, or does not end with the re-enable notice then the unlock notice:\n%s", got)
	}
}

func TestChunkRestampResumeHint_CarriesEveryPopulationFlag(t *testing.T) {
	t.Parallel()
	from, to := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	opts := xlmBaseRestampOptions{
		Allow: map[string]bool{"soroswap": true, "sdex": true}, FillNull: true, Slice: 30 * time.Minute,
		Write: true, Generation: 1_756_800_000, MaxGeneration: 0,
	}
	copts := chunkRestampOptions{Batch: 5000, MinFreeBytes: 1 << 40, AllowLiveAdjacent: true}
	run := newXLMBaseRestampRun(newFakeChunkStore(nil), opts)
	got := chunkRestampResumeHint("/etc/stellarindex.toml", from, to, run, opts, copts)
	want := "RESUME: stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml -tier xlm-base -chunks -from 2026-01-01 -to 2026-07-19 -generation 1756800000" +
		" -fill-null -slice 30m0s -sources sdex,soroswap -max-generation 0 -chunk-batch 5000 -min-free-bytes 1099511627776 -allow-live-adjacent -write"
	if !strings.Contains(got, want) {
		t.Errorf("resume hint:\n%s\nwant containing:\n%s", got, want)
	}
	// Defaults are not repeated.
	opts = xlmBaseRestampOptions{Slice: time.Hour, Generation: 7, MaxGeneration: 7}
	copts = chunkRestampOptions{Batch: defaultChunkBatch}
	got = chunkRestampResumeHint("/etc/x.toml", from, to, newXLMBaseRestampRun(newFakeChunkStore(nil), opts), opts, copts)
	for _, stray := range []string{"-max-generation", "-sources", "-slice", "-chunk-batch", "-min-free-bytes", "-allow-live-adjacent", "-write", "-fill-null"} {
		if strings.Contains(got, stray) {
			t.Errorf("default run's resume hint carries %s:\n%s", stray, got)
		}
	}
}

func TestValidateRestampGeneration(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	for _, ok := range []int64{0, 1, now.Unix() - 1, now.Unix()} {
		if err := validateRestampGeneration(ok, now); err != nil {
			t.Errorf("generation %d: %v", ok, err)
		}
	}
	if err := validateRestampGeneration(-1, now); err == nil || !strings.Contains(err.Error(), ">= 0") {
		t.Errorf("negative: err = %v", err)
	}
	// 1756800000 typed with one extra digit: five centuries out.
	err := validateRestampGeneration(17_568_000_000, now)
	if err == nil || !strings.Contains(err.Error(), "in the future") || !strings.Contains(err.Error(), "never be re-derived") {
		t.Errorf("future: err = %v", err)
	}
	if err := validateRestampGeneration(now.Unix()+1, now); err == nil {
		t.Error("one second in the future was accepted")
	}
}

func TestResolveRestampMaxGeneration(t *testing.T) {
	t.Parallel()
	const gen = int64(1_758_000_000)
	for _, tc := range []struct{ flag, want int64 }{
		{-1, gen}, {0, 0}, {gen - 1, gen - 1}, {gen, gen},
	} {
		got, err := resolveRestampMaxGeneration(tc.flag, gen)
		if err != nil || got != tc.want {
			t.Errorf("-max-generation %d: got (%d, %v), want (%d, nil)", tc.flag, got, err, tc.want)
		}
	}
	for _, bad := range []int64{gen + 1, 17_580_000_000} {
		_, err := resolveRestampMaxGeneration(bad, gen)
		if err == nil || !strings.Contains(err.Error(), "above the run's generation") {
			t.Errorf("-max-generation %d: err = %v, want a refusal", bad, err)
		}
	}
}

// run-heavy-job.sh locks per job NAME, so only the session advisory lock stops
// a second -write launched under another name.
func TestXLMBaseChunkRestamp_RefusesToStartWhileAnotherRunHoldsTheLock(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour))
	store.lockHeld = true
	opts, copts, _ := chunkTestOptions(true)

	err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
	if err == nil || !errors.Is(err, timescale.ErrUSDVolumeRestampLockHeld) {
		t.Fatalf("err = %v, want the held-lock refusal", err)
	}
	for _, want := range []string{"refuses to start", "usd-volume-restamp:trades", "pg_locks", "per job NAME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err lacks %q: %v", want, err)
		}
	}
	if store.count("pause job") != 0 || store.count("decompress ") != 0 || store.count("unlock") != 0 || store.count("resume job") != 0 {
		t.Errorf("a refused run touched the store:\n%s", strings.Join(store.log, "\n"))
	}
	if last := store.log[len(store.log)-1]; last != "lock held" {
		t.Errorf("last store call = %q, want the refused lock attempt", last)
	}
	// The dry run does not take the lock at all.
	store = newFakeChunkStore(chunks, from.Add(3*time.Hour))
	store.lockHeld = true
	opts, copts, out := chunkTestOptions(false)
	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatalf("dry run under a held lock: %v", err)
	}
	if store.count("lock") != 0 || !strings.Contains(out.String(), "would take session advisory lock hashtext('usd-volume-restamp:trades')") {
		t.Errorf("dry run and the lock:\n%s\n%s", strings.Join(store.log, "\n"), out.String())
	}
}

func TestXLMBaseChunkRestamp_RefusesAnAlreadyPausedPolicyWithoutTheFlag(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour))
	store.policy.Scheduled = false
	opts, copts, _ := chunkTestOptions(true)

	err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
	if err == nil {
		t.Fatal("a run against an already-unscheduled policy started")
	}
	for _, want := range []string{"refuses to start", "ALREADY unscheduled", "-resume-paused-policy", "SELECT alter_job(1000, scheduled => true);"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err lacks %q: %v", want, err)
		}
	}
	// The policy is the operator's to re-enable: untouched, lock released.
	if store.count("pause job") != 0 || store.count("resume job") != 0 || store.count("decompress ") != 0 {
		t.Errorf("a refused run touched the policy or a chunk:\n%s", strings.Join(store.log, "\n"))
	}
	if last := store.log[len(store.log)-1]; last != "unlock" {
		t.Errorf("last store call = %q, want the lock released after the refusal:\n%s", last, strings.Join(store.log, "\n"))
	}

	var errw bytes.Buffer
	store = newFakeChunkStore(chunks, from.Add(3*time.Hour))
	store.policy.Scheduled = false
	copts.ResumePausedPolicy, copts.Err = true, &errw
	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatalf("with -resume-paused-policy: %v", err)
	}
	if store.count("decompress ") != 1 || store.count("pause job") != 1 {
		t.Errorf("override run:\n%s", strings.Join(store.log, "\n"))
	}
	wantTeardown(t, store)
	if !strings.Contains(errw.String(), "NOTE: -resume-paused-policy") {
		t.Errorf("no note about taking over the paused policy:\n%s", errw.String())
	}
}

func TestXLMBaseChunkRestamp_WaitsForAPolicyRunInFlightBeforeTheFirstDecompress(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour))
	store.runningPolls = 3
	opts, copts, _ := chunkTestOptions(true)
	var errw bytes.Buffer
	copts.Err = &errw

	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatal(err)
	}
	log := strings.Join(store.log, "\n")
	pause, first := store.index("pause job"), store.index("decompress ")
	polls := 0
	for i, l := range store.log {
		if l == "job-status" {
			polls++
			if i < pause || i > first {
				t.Errorf("job-status poll at %d is outside (pause %d, first decompress %d):\n%s", i, pause, first, log)
			}
		}
	}
	if polls != 4 {
		t.Errorf("polled %d time(s), want 4 (three running, one idle)", polls)
	}
	if n := strings.Count(errw.String(), "waiting for it to finish before the first decompress"); n != 3 {
		t.Errorf("%d progress line(s), want one per running poll:\n%s", n, errw.String())
	}
	wantTeardown(t, store)

	// Bounded: a job that never goes idle is a refusal with a full teardown.
	store = newFakeChunkStore(chunks, from.Add(3*time.Hour))
	store.runningPolls = 1 << 30
	copts.PolicyIdleTimeout = 20 * time.Millisecond
	err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
	if err == nil || !strings.Contains(err.Error(), "still RUNNING") {
		t.Fatalf("err = %v, want the bounded-wait refusal", err)
	}
	if store.count("decompress ") != 0 {
		t.Errorf("decompressed beside a running policy:\n%s", strings.Join(store.log, "\n"))
	}
	wantTeardown(t, store)
}

func TestXLMBaseChunkRestamp_WalksTheListingTakenAfterThePause(t *testing.T) {
	chunks, from, to := threeChunks()
	chunks[2] = chunkFixture("_hyper_1_3_chunk", chunks[2].RangeStart, chunks[2].RangeEnd, 2<<30, 0) // uncompressed when the plan is listed
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), to.Add(2*time.Hour))
	store.relistCompressed = map[string]bool{"_hyper_1_3_chunk": true} // the policy compressed it before the pause landed
	opts, copts, out := chunkTestOptions(true)

	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	log := strings.Join(store.log, "\n")
	if !strings.Contains(out.String(), "2 compressed, 1 not") {
		t.Errorf("the printed plan is not the pre-pause listing:\n%s", out.String())
	}
	if !strings.Contains(log, "decompress _hyper_1_3_chunk") || strings.Contains(log, "work-in-place _hyper_1_3_chunk") {
		t.Errorf("chunk 3 was walked on the pre-pause listing:\n%s", log)
	}
	if !strings.Contains(out.String(), "chunk plan re-read after the pause: 3 chunk(s) (plan had 3); changed: _timescaledb_internal._hyper_1_3_chunk (compressed, was uncompressed)") {
		t.Errorf("no drift line for the re-listing:\n%s", out.String())
	}
}

func TestXLMBaseChunkRestamp_StopsWhenTheChunkIsRecompressedUnderneath(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
	store.recompressUnderneath = "_hyper_1_2_chunk"
	opts, copts, out := chunkTestOptions(true)

	err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
	if err == nil || !errors.Is(err, timescale.ErrTradesChunkRecompressed) {
		t.Fatalf("err = %v, want the recompressed-underneath stop", err)
	}
	for _, want := range []string{"STOPPED", "_timescaledb_internal._hyper_1_2_chunk", "re-compressed underneath", "per-row path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err lacks %q: %v", want, err)
		}
	}
	log := strings.Join(store.log, "\n")
	if !strings.Contains(log, "apply refused in-chunk=_hyper_1_2_chunk") || strings.Contains(log, "_hyper_1_3_chunk") || len(store.done) != 1 {
		t.Errorf("after the guard tripped (%d rows done):\n%s", len(store.done), log)
	}
	if !strings.Contains(out.String(), "RESUME:") || !strings.Contains(out.String(), "-generation 1756800000") {
		t.Errorf("no RESUME line after the stop:\n%s", out.String())
	}
	wantTeardown(t, store)
}

// -day is the LAST day and -days counts back, so the runbook window is 202
// days ending on 07-21.
func TestXLMBaseRestampSummary_AcceptanceLineForTheRunbookWindow(t *testing.T) {
	t.Parallel()
	opts, _, _ := chunkTestOptions(true)
	run := newXLMBaseRestampRun(newFakeChunkStore(nil), opts)
	got := run.summary("/etc/stellarindex.toml", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC))
	const want = "acceptance: stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -day 2026-07-21 -days 202\n"
	if !strings.Contains(got, want) {
		t.Errorf("summary lacks %q:\n%s", want, got)
	}
}

// The disk watchdog kills any heavy job under its own floor regardless of this
// CLI's headroom math, so Required is floor PLUS headroom, never max().
func TestChunkRestampPreflight_WatchdogFloorAddsToHeadroom(t *testing.T) {
	for _, tc := range []struct {
		name    string
		floorKB uint64
		largest int64
		free    func(floor, headroom uint64) uint64
	}{
		{
			// 512 MiB clears the 20 MiB headroom but not the 1 GiB floor.
			name: "under the floor", floorKB: 1 << 20, largest: 10 << 20,
			free: func(uint64, uint64) uint64 { return 512 << 20 },
		},
		{
			// 10 GiB above max(floor, headroom), still short of their sum.
			name: "watchdog gap band", floorKB: 300 << 20, largest: 17_000_000_000,
			free: func(floor, headroom uint64) uint64 { return max(floor, headroom) + 10<<30 },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HEAVY_MIN_DATA_KB", fmt.Sprint(tc.floorKB))
			floor := tc.floorKB * 1024
			if got := heavyMinDataFloorBytes(); got != floor {
				t.Fatalf("heavyMinDataFloorBytes() = %d, want %d", got, floor)
			}
			headroom := uint64(float64(tc.largest) * chunkFreeSpaceHeadroom)
			free := tc.free(floor, headroom)
			copts := chunkRestampOptions{FreeBytes: func(string) (uint64, error) { return free, nil }}

			p := chunkRestampPreflight(context.Background(), &fakeChunkStore{path: "/data"}, tc.largest, copts)
			if p.Required != floor+headroom {
				t.Errorf("Required = %d bytes, want floor+headroom %d bytes (floor alone is %d)", p.Required, floor+headroom, floor)
			}
			if p.Err == nil {
				t.Errorf("pre-flight passed with %s free against floor %s + headroom %s; want a refusal",
					fmtBytes(int64(free)), fmtBytes(int64(floor)), fmtBytes(int64(headroom)))
			}
		})
	}
}

// Only a missing path means the wrong host; a permission or I/O error must not
// send the operator chasing a host that was never wrong.
func TestStatfsHostMismatchErr_ClassifiesErrno(t *testing.T) {
	for errno, want := range map[syscall.Errno]string{
		syscall.ENOENT:  "this host is not the database host",
		syscall.ENOTDIR: "this host is not the database host",
		syscall.EACCES:  "cannot stat the path",
		syscall.EPERM:   "cannot stat the path",
		syscall.EIO:     "transient statfs failure",
	} {
		err := fmt.Errorf("statfs /data: %w", errno)
		if got := statfsHostMismatchErr("/data", err); !strings.Contains(got.Error(), want) {
			t.Errorf("statfsHostMismatchErr(%v) = %q, want it to contain %q", err, got, want)
		}
	}
}

// postgresql-client creates /var/lib/postgresql on ops hosts, so a local
// statfs must not stand in for a remote database host's free space.
func TestChunkRestampPreflight_RefusesLocalStatfsForRemoteDSN(t *testing.T) {
	t.Setenv("HEAVY_MIN_DATA_KB", "0")
	store := &fakeChunkStore{path: "/var/lib/postgresql/16/main"}
	measured := false
	copts := chunkRestampOptions{
		RemoteDBHost: "db.internal",
		FreeBytes: func(string) (uint64, error) {
			measured = true
			return 5 << 40, nil
		},
	}

	p := chunkRestampPreflight(context.Background(), store, 1<<30, copts)
	if p.Err == nil || !strings.Contains(p.Err.Error(), "db.internal") {
		t.Fatalf("pre-flight err = %v, want a refusal naming the remote DSN host", p.Err)
	}
	if measured || p.MeasuredOK {
		t.Errorf("statfs ran on this host for a remote DSN (MeasuredOK=%v); want it skipped", p.MeasuredOK)
	}
	if got := p.render(); !strings.Contains(got, "free space UNKNOWN") || !strings.Contains(got, "REFUSED") {
		t.Errorf("render() = %q, want UNKNOWN free space and REFUSED", got)
	}

	copts.MinFreeBytes = 4 << 30
	if p := chunkRestampPreflight(context.Background(), store, 1<<30, copts); p.Err != nil {
		t.Errorf("-min-free-bytes 4 GiB for a 1 GiB chunk refused for a remote DSN: %v", p.Err)
	}
}

func TestRemoteDSNHost(t *testing.T) {
	for dsn, want := range map[string]string{
		"postgres://u:p@localhost:5432/db":                  "",
		"postgres://u:p@127.0.0.1:5432/db":                  "",
		"postgres://u:p@[::1]:5432/db":                      "",
		"host=/var/run/postgresql dbname=db":                "",
		"host=10.0.0.5 dbname=db":                           "10.0.0.5",
		"postgres://u:p@db.internal:5432/db":                "db.internal",
		"postgres://u:p@localhost:5432,db.internal:5433/db": "db.internal",
		"postgres://u:p@local host/db":                      "(unparseable DSN)",
	} {
		if got := remoteDSNHost(dsn); got != want {
			t.Errorf("remoteDSNHost(%q) = %q, want %q", dsn, got, want)
		}
	}
}
