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
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── `usd-volume-restamp -tier xlm-base -chunks` — the chunk walk ────────
//
// The store-side bracket (decompress → work → compress, compress-on-failure
// included) is pinned on the scripted driver in
// internal/storage/timescale/trades_chunks_test.go. What is pinned HERE is
// the walk that drives it: the dry run prints the chunk plan and touches
// no chunk; a write run brackets every chunk it changes and scopes every
// scan to that chunk's slice of the window; a chunk with nothing to change
// is probed read-only and skipped; a failure stops the walk after the
// failing chunk; the free-space pre-flight refuses a write run before any
// chunk is decompressed and again before each later decompress; the
// compression policy is paused for the run and re-enabled on every exit;
// a live-adjacent window is refused; the by-hand repair is printed before
// each statement a SIGKILL could interrupt; the run lock is taken before
// the policy is touched and released after it is put back; a policy
// already unscheduled at start is refused without -resume-paused-policy;
// a policy run in flight is waited out; the chunks are listed again after
// the pause; and a chunk re-compressed underneath the run stops the walk.

// fakeChunkStore is the chunk walk's seam double. It holds a set of DIRTY
// rows (by timestamp); a plan over a window returns the dirty rows in it
// that have not been applied yet, and an apply marks them applied — the
// same idempotence the real planner has (a row already holding the
// anchor's value is not a candidate), which is what makes "a rerun skips
// the finished chunks" observable here without a database.
type fakeChunkStore struct {
	chunks []timescale.TradeChunk
	path   string
	dirty  []time.Time
	done   map[time.Time]bool

	// failApplyAt makes the n-th Apply call (1-based) fail.
	failApplyAt int
	applies     int
	// cancelInApply cancels this context from inside the n-th Apply —
	// the operator's SIGTERM arriving mid-chunk.
	cancelInApply context.CancelFunc

	// policy is what TradesCompressionPolicy answers; policyErr replaces
	// it when set.
	policy    timescale.TradesCompressionPolicy
	policyErr error
	// scheduleCtxErr records ctx.Err() as seen by each SetJobScheduled
	// call, in order — the re-enable must run on a context that survives
	// the run's cancellation.
	scheduleCtxErr []error
	// paused mirrors the policy's scheduled flag as the fake's own state;
	// relistCompressed, when set, overrides chunks' Compressed in listings
	// taken while paused — the policy compressed something between the
	// plan's listing and the pause.
	paused           bool
	relistCompressed map[string]bool
	// errw is the run's stderr; errAtPause is what it held at the moment
	// the pause was issued.
	errw       *bytes.Buffer
	errAtPause string

	// lockHeld makes TryUSDVolumeRestampLock answer that another session
	// holds the lock. unlockCtxErr records ctx.Err() at release.
	lockHeld     bool
	unlockCtxErr error
	// runningPolls is how many JobRunning polls answer true before the
	// policy reads idle.
	runningPolls int
	// recompressUnderneath names a chunk whose in-chunk apply finds it
	// compressed again.
	recompressUnderneath string

	log []string
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

// ApplyXLMBaseUSDVolumeRestampInChunk is the guarded apply: the named
// chunk reads compressed again and the batch is refused; otherwise it is
// the plain apply, logged with the chunk it ran inside.
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

// RestampTradesChunk mirrors the store's contract: a chunk compressed at
// listing is bracketed (and the hook fires before each half); one that
// was not is neither decompressed nor compressed.
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

// index returns the position of the first log line with the prefix, -1
// when there is none.
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

// Three weekly chunks; the run window [Jan 5, Jan 18] starts INSIDE the
// first and ends INSIDE the last, so the clamping is observable.
func threeChunks() ([]timescale.TradeChunk, time.Time, time.Time) {
	d := func(day int) time.Time { return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC) }
	chunks := []timescale.TradeChunk{
		chunkFixture("_hyper_1_1_chunk", d(1), d(8), 4<<30, 300<<20),
		chunkFixture("_hyper_1_2_chunk", d(8), d(15), 160<<30, 10<<30),
		chunkFixture("_hyper_1_3_chunk", d(15), d(22), 2<<30, 200<<20),
	}
	return chunks, d(5), d(18)
}

func chunkTestOptions(write bool) (xlmBaseRestampOptions, xlmBaseChunkOptions, *bytes.Buffer) {
	var out bytes.Buffer
	opts := xlmBaseRestampOptions{
		Slice: 24 * time.Hour, Batch: 2000, Write: write, SampleSize: 3,
		MaxGeneration: 1_756_800_000, Generation: 1_756_800_000,
	}
	copts := xlmBaseChunkOptions{
		Batch:     20_000,
		FreeBytes: func(string) (uint64, error) { return 4_690 << 30, nil }, // 4.69 TB free
		// Months after the window: nowhere near the policy's 7-day lag.
		Now:        time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Out:        &out,
		Err:        io.Discard,
		PolicyPoll: time.Millisecond,
	}
	return opts, copts, &out
}

// wantTeardown asserts a write run's last two store calls: the policy
// re-enabled, THEN the run lock released — in that order, so a run waiting
// on the lock never inherits a paused policy.
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
		"need > 320.0 GB (2.0 x the largest chunk's uncompressed size; a guard, not a bound) — OK",
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
	// One dirty row per chunk, inside the window.
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
	opts, copts, out := chunkTestOptions(true)

	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(store.done) != 3 {
		t.Fatalf("applied %d row(s), want 3", len(store.done))
	}
	// Every chunk was bracketed exactly once, in range order, and every
	// apply happened INSIDE its chunk's bracket.
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
				t.Fatalf("apply outside a bracket: %s\n%s", l, strings.Join(store.log, "\n"))
			}
			if !strings.Contains(l, "batch=20000") {
				t.Errorf("apply inside a decompressed chunk used %s, want the chunk batch (20000)", l)
			}
			if !strings.HasSuffix(l, " in-chunk="+open) {
				t.Errorf("apply inside %s bypassed the guarded in-chunk apply: %s", open, l)
			}
		}
	}
	// The lock is taken before the policy is paused, and both happen
	// before the first decompress.
	if lock, pause := store.index("lock"), store.index("pause job"); lock < 0 || pause < 0 || lock > pause || pause > brackets0(store) {
		t.Errorf("order of lock (%d), pause (%d), first decompress (%d):\n%s", lock, pause, brackets0(store), strings.Join(store.log, "\n"))
	}
	wantTeardown(t, store)
	if want := []string{"_hyper_1_1_chunk", "_hyper_1_2_chunk", "_hyper_1_3_chunk"}; strings.Join(brackets, ",") != strings.Join(want, ",") {
		t.Errorf("brackets = %v, want %v", brackets, want)
	}

	// Every scan is bounded by the chunk that is open at the time, AND by
	// the run window — the first chunk starts 4 days before -from and the
	// last ends 4 days after -to, and neither overhang is scanned.
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
		// -day is the LAST day and -days counts back from it, so this
		// covers exactly [2026-01-05, 2026-01-18].
		"acceptance: stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -day 2026-01-18 -days 14",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, got)
		}
	}
}

// brackets0 is the index of the first decompress in the fake's log.
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

	// The rerun — same generation, same command.
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
	// The failing chunk was re-compressed (the store contract the fake
	// mirrors), the policy re-enabled right after, and the walk did not
	// go on to the third chunk.
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
			wantErr: "free space 200.0 GB on /var/lib/postgresql/data is not more than 320.0 GB",
		},
		{
			name: "exactly 2x is not more than", free: 320 << 30, path: "/var/lib/postgresql/data",
			wantErr: "is not more than 320.0 GB",
		},
		{
			name: "unmeasurable and no override", freeErr: errors.New("statfs: no such file or directory"), path: "/var/lib/postgresql/data",
			wantErr: "-min-free-bytes",
		},
		{
			name: "data directory unreadable and no override", path: "",
			wantErr: "-min-free-bytes",
		},
		{
			name: "override too small", freeErr: errors.New("statfs: no such file or directory"), path: "/var/lib/postgresql/data", minFree: 100 << 30,
			wantErr: "free space 100.0 GB (-min-free-bytes, NOT measured)",
		},
		{
			name: "override just under 2x", freeErr: errors.New("statfs: no such file or directory"), path: "/var/lib/postgresql/data", minFree: 300 << 30,
			wantErr: "free space 300.0 GB (-min-free-bytes, NOT measured) is not more than 320.0 GB",
		},
		{
			name: "override large enough", freeErr: errors.New("statfs: no such file or directory"), path: "/var/lib/postgresql/data", minFree: 400 << 30,
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

	// A DRY RUN prints the verdict it would refuse on, and carries on
	// read-only: the plan is still the thing the operator came for.
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
	lo, hi := clampTradesChunk(c, d(5), d(19))
	if !lo.Equal(d(8)) || !hi.Equal(d(15)) {
		t.Errorf("chunk inside window: [%s, %s)", lo, hi)
	}
	lo, hi = clampTradesChunk(c, d(10), d(12))
	if !lo.Equal(d(10)) || !hi.Equal(d(12)) {
		t.Errorf("window inside chunk: [%s, %s)", lo, hi)
	}
	lo, hi = clampTradesChunk(c, d(1), d(9))
	if !lo.Equal(d(8)) || !hi.Equal(d(9)) {
		t.Errorf("window overlaps the chunk's start: [%s, %s)", lo, hi)
	}
}

func TestValidateRestampChunkFlags(t *testing.T) {
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
	// And the whole chunk flag set is xlm-base-only.
	for _, f := range append([]string{"chunks"}, restampChunkOnlyFlags...) {
		if err := validateRestampTierFlags(restampTierExact, map[string]bool{f: true}); err == nil {
			t.Errorf("-%s with -tier exact was accepted", f)
		}
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

// ─── the compression policy is paused for the run ────────────────────────
//
// migrations/0001 attaches add_compression_policy('trades', '7 days') on a
// 12-hour schedule; its proc selects every chunk older than the lag that
// is not fully compressed — a chunk this walk has decompressed IS one —
// and compresses it. Over a multi-day run the policy would re-compress
// the open chunk between two batches, and the next batch would crawl
// through the per-row decompression path this mode exists to escape,
// without an error. So: paused before the first decompress, re-enabled
// after the last chunk, re-enabled on failure, re-enabled on a cancelled
// context — on a context that survives the cancellation.

func TestXLMBaseChunkRestamp_PausesTheCompressionPolicyForTheRun(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), to.Add(2*time.Hour))
	opts, copts, out := chunkTestOptions(true)

	if err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	log := strings.Join(store.log, "\n")
	pause, first := store.index("pause job 1000"), store.index("decompress ")
	if pause < 0 || first < 0 || pause > first {
		t.Fatalf("the compression policy was not paused before the first decompress (pause at %d, first decompress at %d):\n%s", pause, first, log)
	}
	wantTeardown(t, store)
	if n := store.count("pause job"); n != 1 {
		t.Errorf("policy paused %d time(s), want exactly once", n)
	}
	// The policy is read again UNDER the lock, and the chunks are listed
	// again AFTER the pause: the state the walk restores is the one nothing
	// else is changing.
	lock := store.index("lock")
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
}

func TestXLMBaseChunkRestamp_ReenablesTheCompressionPolicyOnFailure(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
	store.failApplyAt = 2
	opts, copts, _ := chunkTestOptions(true)

	err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
	if err == nil || !strings.Contains(err.Error(), "deadlock detected") {
		t.Fatalf("err = %v, want the apply failure", err)
	}
	log := strings.Join(store.log, "\n")
	wantTeardown(t, store)
	if store.index("pause job 1000") < 0 {
		t.Errorf("the policy was never paused:\n%s", log)
	}
}

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
	// The re-enable and the unlock ran on a context that had NOT been
	// cancelled — a cancelled one would fail the very statements that put
	// the policy back and let the next run in.
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

// ─── the policy must exist ───────────────────────────────────────────────

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

// ─── live-adjacent windows ───────────────────────────────────────────────

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
	// Exactly on the edge is not past it.
	if adj, err := checkRestampLiveAdjacent(edge.Truncate(24*time.Hour).AddDate(0, 0, -1), edge.Truncate(24*time.Hour).Add(lag), lag, false); adj || err != nil {
		t.Errorf("window ending exactly on the edge: adjacent=%v err=%v", adj, err)
	}
}

func TestXLMBaseChunkRestamp_RefusesALiveAdjacentWindow(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks, from.Add(3*time.Hour))
	opts, copts, _ := chunkTestOptions(true)
	copts.Now = to.AddDate(0, 0, 3) // three days after -to: inside the 7-day lag

	err := runXLMBaseChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
	if err == nil || !strings.Contains(err.Error(), "-allow-live-adjacent") {
		t.Fatalf("err = %v, want the live-adjacent refusal", err)
	}
	if got := strings.Join(store.log, "\n"); got != "policy" {
		t.Errorf("a refused run went on to touch the store:\n%s", got)
	}

	// The override walks it, warns, and the policy dance still happens.
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

// ─── an uncompressed chunk stays uncompressed ────────────────────────────

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

// ─── free space is re-checked before EVERY decompress ────────────────────

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
		!strings.Contains(err.Error(), "free space 100.0 GB on /var/lib/postgresql/data is not more than 320.0 GB") {
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

// ─── the by-hand repair is on stderr before the statement it repairs ─────

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
	// Up front, before anything is decompressed: the policy re-enable.
	pausing, firstDecompress := strings.Index(got, "PAUSING"), strings.Index(got, "decompressing")
	if pausing < 0 || firstDecompress < 0 || pausing > firstDecompress {
		t.Errorf("the policy notice is not before the first decompress (at %d vs %d):\n%s", pausing, firstDecompress, got)
	}
	if !strings.Contains(got[:firstDecompress], reenable) {
		t.Errorf("the re-enable SQL is not printed up front:\n%s", got)
	}
	// And BEFORE the pause is issued — what stderr held at the moment
	// SetJobScheduled(false) ran already carried it. A SIGKILL between the
	// two would otherwise leave the policy paused with no trace of how to
	// put it back.
	if !strings.Contains(store.errAtPause, reenable) {
		t.Errorf("the re-enable SQL was not on stderr when the pause was issued; stderr at that moment:\n%s", store.errAtPause)
	}
	// Per chunk: the exact compress_chunk twice (before the decompress and
	// before the re-compress), each with the re-enable beside it.
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

// ─── the RESUME line carries everything that shaped the population ───────

func TestChunkRestampResumeHint_CarriesEveryPopulationFlag(t *testing.T) {
	t.Parallel()
	from, to := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	opts := xlmBaseRestampOptions{
		Allow: map[string]bool{"soroswap": true, "sdex": true}, FillNull: true, Slice: 30 * time.Minute,
		Write: true, Generation: 1_756_800_000, MaxGeneration: 0,
	}
	copts := xlmBaseChunkOptions{Batch: 5000, MinFreeBytes: 1 << 40, AllowLiveAdjacent: true}
	got := chunkRestampResumeHint("/etc/stellarindex.toml", from, to, opts, copts)
	want := "RESUME: stellarindex-ops usd-volume-restamp -config /etc/stellarindex.toml -tier xlm-base -chunks -from 2026-01-01 -to 2026-07-19 -generation 1756800000" +
		" -fill-null -slice 30m0s -sources sdex,soroswap -max-generation 0 -chunk-batch 5000 -min-free-bytes 1099511627776 -allow-live-adjacent -write"
	if !strings.Contains(got, want) {
		t.Errorf("resume hint:\n%s\nwant containing:\n%s", got, want)
	}
	// Defaults are not repeated: -max-generation equal to the generation
	// is the default, and so are the batch, slice, and no sources.
	opts = xlmBaseRestampOptions{Slice: time.Hour, Generation: 7, MaxGeneration: 7}
	copts = xlmBaseChunkOptions{Batch: defaultChunkBatch}
	got = chunkRestampResumeHint("/etc/x.toml", from, to, opts, copts)
	for _, stray := range []string{"-max-generation", "-sources", "-slice", "-chunk-batch", "-min-free-bytes", "-allow-live-adjacent", "-write", "-fill-null"} {
		if strings.Contains(got, stray) {
			t.Errorf("default run's resume hint carries %s:\n%s", stray, got)
		}
	}
}

// ─── -generation cannot be in the future ─────────────────────────────────

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
	// 1756800000 typed as 17568000000 — one extra digit, five centuries out.
	err := validateRestampGeneration(17_568_000_000, now)
	if err == nil || !strings.Contains(err.Error(), "in the future") || !strings.Contains(err.Error(), "never be re-derived") {
		t.Errorf("future: err = %v", err)
	}
	if err := validateRestampGeneration(now.Unix()+1, now); err == nil {
		t.Error("one second in the future was accepted")
	}
}

// ─── one run at a time: the run lock ─────────────────────────────────────
//
// run-heavy-job.sh's lock is per job NAME and the runbook mandates a
// unique name per attempt, so the wrapper does not stop a second -write
// from starting beside a live one. The session advisory lock does.

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
	// Nothing after the lock ran: no pause, no decompress, no unlock (there
	// is nothing to release), and the policy was not touched.
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

// ─── a policy already unscheduled at start ───────────────────────────────

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
	// Refused under the lock, before the pause: nothing decompressed, the
	// policy NOT touched (it is the operator's to re-enable), the lock
	// released.
	if store.count("pause job") != 0 || store.count("resume job") != 0 || store.count("decompress ") != 0 {
		t.Errorf("a refused run touched the policy or a chunk:\n%s", strings.Join(store.log, "\n"))
	}
	if last := store.log[len(store.log)-1]; last != "unlock" {
		t.Errorf("last store call = %q, want the lock released after the refusal:\n%s", last, strings.Join(store.log, "\n"))
	}

	// With the flag: proceeds, notes it on stderr, and still re-enables.
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

// ─── a policy run in flight is waited out ────────────────────────────────

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

	// Bounded: a job that never goes idle is a refusal, the policy is
	// re-enabled and the lock released, and nothing was decompressed.
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

// ─── the chunks are listed again after the pause ─────────────────────────

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
	// The plan said "1 not compressed"; the walk bracketed the chunk,
	// because the listing it walks is the post-pause one.
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

// ─── a chunk re-compressed underneath the run stops the walk ─────────────

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
	// No row was written into chunk 2, the walk did not go on to chunk 3,
	// and the teardown ran.
	if !strings.Contains(log, "apply refused in-chunk=_hyper_1_2_chunk") || strings.Contains(log, "_hyper_1_3_chunk") || len(store.done) != 1 {
		t.Errorf("after the guard tripped (%d rows done):\n%s", len(store.done), log)
	}
	if !strings.Contains(out.String(), "RESUME:") || !strings.Contains(out.String(), "-generation 1756800000") {
		t.Errorf("no RESUME line after the stop:\n%s", out.String())
	}
	wantTeardown(t, store)
}

// ─── the acceptance line the runbook quotes ──────────────────────────────

// TestXLMBaseRestampSummary_AcceptanceLineForTheRunbookWindow pins the
// exact acceptance command the tool prints for the #372 window the
// runbook recommends (Step 6, chunk mode): -day is the LAST day and -days
// counts back from it, so [2026-01-01, 2026-07-21] is 202 days ending on
// 07-21. The runbook quotes this line byte-for-byte.
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
