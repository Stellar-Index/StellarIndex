// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── `usd-volume-restamp -tier exact -chunks` — the exact tier's walk ────
//
// The driver is pinned tier-agnostically by the xlm-base tests beside
// this file (plan, pre-flight, lock, policy dance, tracing, re-listing).
// What is pinned HERE is that the EXACT tier drives that same walk: it
// brackets each compressed chunk, applies its identity through the
// guarded in-chunk apply, scopes every statement to the chunk's slice of
// the run window, probes a finished chunk read-only and leaves it
// compressed, and — in a dry run — counts through the candidate count and
// decompresses nothing.

const exactTestUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// fakeExactChunkStore is the exact tier's seam double. It reuses the
// driver double for the chunk/policy/lock half and models the exact
// tier's own half the same way: a set of DIRTY rows by timestamp, where a
// count returns the ones not yet applied and an apply marks them applied
// — the idempotence the real scope predicate has
// (`usd_volume IS DISTINCT FROM <identity>`), which is what makes "a
// rerun skips the finished chunks" observable without a database.
type fakeExactChunkStore struct {
	*fakeChunkStore

	groups []timescale.TradeValuationGroup
	dirty  []time.Time
	done   map[time.Time]bool

	// failApplyAt makes the n-th exact apply (1-based) fail.
	failApplyAt int
	applies     int
	// recompressUnderneath names a chunk whose in-chunk apply finds it
	// compressed again.
	recompressUnderneath string
}

func newFakeExactChunkStore(chunks []timescale.TradeChunk, dirty ...time.Time) *fakeExactChunkStore {
	return &fakeExactChunkStore{
		fakeChunkStore: newFakeChunkStore(chunks),
		groups: []timescale.TradeValuationGroup{{
			Source: "sdex", BaseAsset: exactTestUSDC, QuoteAsset: "native",
			PricedRows: 3, SumUSDVolume: "0", SumBaseAmount: "0", SumQuoteAmount: "0",
		}},
		dirty: dirty,
		done:  map[time.Time]bool{},
	}
}

func (f *fakeExactChunkStore) TradeValuationByDay(_ context.Context, day time.Time) ([]timescale.TradeValuationGroup, error) {
	f.log = append(f.log, "valuation "+day.Format(time.DateOnly))
	return f.groups, nil
}

// pending is the dirty rows inside the window that no apply has taken yet.
func (f *fakeExactChunkStore) pending(p timescale.USDVolumeRestampParams) []time.Time {
	var out []time.Time
	for _, ts := range f.dirty {
		if ts.Before(p.From) || !ts.Before(p.To) || f.done[ts] {
			continue
		}
		out = append(out, ts)
	}
	return out
}

func (f *fakeExactChunkStore) CountUSDVolumeRestampCandidates(_ context.Context, p timescale.USDVolumeRestampParams) (int64, error) {
	f.log = append(f.log, fmt.Sprintf("count [%s, %s)", p.From.Format("01-02T15"), p.To.Format("01-02T15")))
	return int64(len(f.pending(p))), nil
}

func (f *fakeExactChunkStore) RestampExactTierUSDVolume(_ context.Context, p timescale.USDVolumeRestampParams) (int64, error) {
	f.applies++
	rows := f.pending(p)
	f.log = append(f.log, fmt.Sprintf("restamp %d rows gen=%d fill_null=%v [%s, %s)",
		len(rows), p.Generation, p.FillNull, p.From.Format("01-02T15"), p.To.Format("01-02T15")))
	if f.failApplyAt > 0 && f.applies == f.failApplyAt {
		return 0, errors.New("update: deadlock detected")
	}
	for _, ts := range rows {
		f.done[ts] = true
	}
	return int64(len(rows)), nil
}

func (f *fakeExactChunkStore) RestampExactTierUSDVolumeInChunk(ctx context.Context, c timescale.TradeChunk, p timescale.USDVolumeRestampParams) (int64, error) {
	if c.Name == f.recompressUnderneath {
		f.log = append(f.log, "restamp refused in-chunk="+c.Name)
		return 0, fmt.Errorf("%w: %s reads is_compressed = true", timescale.ErrTradesChunkRecompressed, c)
	}
	n, err := f.RestampExactTierUSDVolume(ctx, p)
	f.log[len(f.log)-1] += " in-chunk=" + c.Name
	return n, err
}

// exactChunkTestRun builds the exact-tier run the chunk walk drives, with
// the REAL classifier behind it (the group above is tier 2b, base-pegged).
func exactChunkTestRun(t *testing.T, store exactChunkStore, write bool) *restampRun {
	t.Helper()
	spec, err := timescale.NewUSDVolumeQuoteSpec([]string{exactTestUSDC}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &restampRun{
		store: store, spec: spec, slice: 24 * time.Hour, write: write,
		generation: 1_756_800_000,
	}
}

func TestExactChunkRestamp_BracketsEachChunkAndRestampsInsideIt(t *testing.T) {
	chunks, from, to := threeChunks()
	// One dirty row per chunk, inside the window.
	store := newFakeExactChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
	_, copts, out := chunkTestOptions(true)
	run := exactChunkTestRun(t, store, true)

	if err := runExactChunkRestamp(context.Background(), store, run, "/etc/stellarindex.toml", from, to, copts); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(store.done) != 3 {
		t.Fatalf("restamped %d row(s), want 3:\n%s", len(store.done), strings.Join(store.log, "\n"))
	}
	// Every chunk bracketed exactly once, in range order, with every
	// UPDATE inside its own bracket and routed through the guarded
	// in-chunk apply.
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
		case strings.HasPrefix(l, "restamp "):
			if open == "" {
				t.Fatalf("apply outside a bracket: %s\n%s", l, strings.Join(store.log, "\n"))
			}
			if !strings.HasSuffix(l, " in-chunk="+open) {
				t.Errorf("apply inside %s bypassed the guarded in-chunk apply: %s", open, l)
			}
			if !strings.Contains(l, "gen=1756800000") {
				t.Errorf("apply did not carry the run's generation (INV-3): %s", l)
			}
		}
	}
	if want := []string{"_hyper_1_1_chunk", "_hyper_1_2_chunk", "_hyper_1_3_chunk"}; strings.Join(brackets, ",") != strings.Join(want, ",") {
		t.Errorf("brackets = %v, want %v", brackets, want)
	}
	if lock, pause := store.index("lock"), store.index("pause job"); lock < 0 || pause < 0 || lock > pause || pause > brackets0(store.fakeChunkStore) {
		t.Errorf("order of lock (%d), pause (%d), first decompress (%d):\n%s", lock, pause, brackets0(store.fakeChunkStore), strings.Join(store.log, "\n"))
	}
	wantTeardown(t, store.fakeChunkStore)

	// Every statement is bounded by the chunk that is open at the time AND
	// by the run window: the first chunk starts 4 days before -from and the
	// last ends 4 days after -to, and neither overhang is touched.
	windowEnd := to.AddDate(0, 0, 1)
	open = ""
	for _, l := range store.log {
		switch {
		case strings.HasPrefix(l, "decompress "):
			open = strings.TrimPrefix(l, "decompress ")
			continue
		case strings.HasPrefix(l, "compress "):
			open = ""
			continue
		case open == "" || !strings.Contains(l, " ["):
			continue
		}
		span := l[strings.Index(l, "["):]
		span = span[:strings.Index(span, ")")+1]
		lo, hi := parsePlanWindow(t, "plan "+span)
		var chunk timescale.TradeChunk
		for _, c := range chunks {
			if c.Name == open {
				chunk = c
			}
		}
		if lo.Before(chunk.RangeStart) || hi.After(chunk.RangeEnd) {
			t.Errorf("statement %q escapes open chunk %s [%s, %s)", l, open, chunk.RangeStart.Format(time.DateOnly), chunk.RangeEnd.Format(time.DateOnly))
		}
		if lo.Before(from) || hi.After(windowEnd) {
			t.Errorf("statement %q escapes the run window [%s, %s)", l, from.Format(time.DateOnly), windowEnd.Format(time.DateOnly))
		}
	}
	got := out.String()
	for _, want := range []string{
		"chunk 1/3 _timescaledb_internal._hyper_1_1_chunk",
		"chunk 3/3 _timescaledb_internal._hyper_1_3_chunk",
		"changed 1 row(s)",
		"300.0 MB -> 4.0 GB -> 300.0 MB",
		"restamped 3 row(s) across 14 exact-tier group-day(s) in [2026-01-05, 2026-01-18]",
		"CALL refresh_continuous_aggregate('prices_1m'",
		"acceptance: stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -day 2026-01-18 -days 14",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, got)
		}
	}
	// The estimated tier's own flag guidance has no business here.
	if strings.Contains(got, "-min-rel-delta") {
		t.Errorf("the exact tier's summary carries the anchor re-derive's flag guidance:\n%s", got)
	}
}

func TestExactChunkRestamp_DryRunCountsAndTouchesNoChunk(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeExactChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 6))
	_, copts, out := chunkTestOptions(false)
	run := exactChunkTestRun(t, store, false)

	if err := runExactChunkRestamp(context.Background(), store, run, "/etc/stellarindex.toml", from, to, copts); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"chunk plan: 3 trades chunk(s) intersect [2026-01-05, 2026-01-18]",
		"pre-flight: free 4.6 TB on /var/lib/postgresql/data",
		"DRY RUN: would take session advisory lock hashtext('usd-volume-restamp:trades')",
		"DRY RUN: nothing is decompressed",
		"would change 1 row(s)",
		"would restamp 2 row(s) across 14 exact-tier group-day(s)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, got)
		}
	}
	// Fail-closed: the dry run counts, it never applies, and it neither
	// decompresses a chunk nor touches the compression policy.
	if store.applies != 0 || len(store.done) != 0 {
		t.Errorf("the dry run wrote %d row(s) in %d apply call(s)", len(store.done), store.applies)
	}
	if store.count("count ") == 0 {
		t.Error("the dry run counted nothing — it must still report the rows it would change")
	}
	for _, prefix := range []string{"decompress ", "pause job", "resume job", "lock"} {
		if n := store.count(prefix); n != 0 {
			t.Errorf("dry run made %d %q call(s):\n%s", n, prefix, strings.Join(store.log, "\n"))
		}
	}
}

func TestExactChunkRestamp_RerunProbesAndSkipsFinishedChunks(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeExactChunkStore(chunks, from.Add(3*time.Hour), to.Add(2*time.Hour)) // chunks 1 and 3 dirty, 2 clean
	_, copts, out := chunkTestOptions(true)
	ctx := context.Background()

	if err := runExactChunkRestamp(ctx, store, exactChunkTestRun(t, store, true), "/etc/stellarindex.toml", from, to, copts); err != nil {
		t.Fatal(err)
	}
	if got := store.count("decompress "); got != 2 {
		t.Fatalf("first run decompressed %d chunk(s), want 2 (the clean middle chunk is probed and skipped):\n%s",
			got, strings.Join(store.log, "\n"))
	}
	if !strings.Contains(out.String(), "nothing to change — skipped, chunk left compressed") {
		t.Errorf("the clean chunk was not reported as skipped:\n%s", out.String())
	}

	// The rerun — same generation, same command: every chunk probes clean.
	store.log = nil
	out.Reset()
	if err := runExactChunkRestamp(ctx, store, exactChunkTestRun(t, store, true), "/etc/stellarindex.toml", from, to, copts); err != nil {
		t.Fatal(err)
	}
	if got := store.count("decompress "); got != 0 {
		t.Errorf("rerun decompressed %d chunk(s), want 0:\n%s", got, strings.Join(store.log, "\n"))
	}
	if got := store.count("restamp "); got != 0 {
		t.Errorf("rerun applied %d time(s), want 0", got)
	}
	if n := strings.Count(out.String(), "skipped, chunk left compressed"); n != 3 {
		t.Errorf("rerun reported %d skipped chunk(s), want 3:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "restamped 0 row(s)") {
		t.Errorf("rerun summary:\n%s", out.String())
	}
}

func TestExactChunkRestamp_StopsWhenTheChunkIsRecompressedUnderneath(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeExactChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
	store.recompressUnderneath = "_hyper_1_2_chunk"
	_, copts, out := chunkTestOptions(true)

	err := runExactChunkRestamp(context.Background(), store, exactChunkTestRun(t, store, true), "/etc/stellarindex.toml", from, to, copts)
	if err == nil || !errors.Is(err, timescale.ErrTradesChunkRecompressed) {
		t.Fatalf("err = %v, want the recompressed-underneath stop", err)
	}
	log := strings.Join(store.log, "\n")
	if !strings.Contains(log, "restamp refused in-chunk=_hyper_1_2_chunk") || strings.Contains(log, "_hyper_1_3_chunk") || len(store.done) != 1 {
		t.Errorf("after the guard tripped (%d rows done):\n%s", len(store.done), log)
	}
	for _, want := range []string{"RESUME:", "-tier exact -chunks", "-generation 1756800000"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("resume line lacks %q:\n%s", want, out.String())
		}
	}
	wantTeardown(t, store.fakeChunkStore)
}

// ─── the policy is re-enabled even when the tier's callback fails ────────
//
// The driver decompresses a chunk and hands it to the tier. Whatever the
// tier does with it — an anchor re-derive, an identity UPDATE, or, here, a
// callback that simply fails — the chunk is re-compressed, the policy is
// re-enabled and the lock is released before the tool exits non-zero. This
// is the driver's contract, so it is pinned on a stub tier rather than on
// either real one: a tier added later inherits it.

// stubChunkTier is a [chunkRestampTier] that reports one dirty chunk and
// then fails inside it.
type stubChunkTier struct {
	err     error
	written int64
	calls   int
}

func (s *stubChunkTier) write() bool { return true }
func (s *stubChunkTier) header(time.Time, time.Time, chunkRestampOptions) string {
	return "stub tier\n"
}

func (s *stubChunkTier) probe(context.Context, time.Time, time.Time) (bool, error) { return true, nil }
func (s *stubChunkTier) preview(context.Context, time.Time, time.Time) (string, error) {
	return "would change 0 row(s)", nil
}

func (s *stubChunkTier) restamp(context.Context, timescale.TradeChunk, time.Time, time.Time) (chunkRestampOutcome, error) {
	s.calls++
	return chunkRestampOutcome{Written: s.written, Note: "changed 0 row(s)"}, s.err
}
func (s *stubChunkTier) tick(time.Time)                             {}
func (s *stubChunkTier) finish(string, time.Time, time.Time) string { return "stub finished\n" }
func (s *stubChunkTier) resume(string, time.Time, time.Time, chunkRestampOptions) string {
	return "\nRESUME: stub\n"
}

func TestChunkRestamp_ReenablesTheCompressionPolicyWhenTheTierFails(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeChunkStore(chunks)
	_, copts, out := chunkTestOptions(true)
	tier := &stubChunkTier{err: errors.New("restamp callback exploded"), written: 4}

	err := runChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, copts, tier)
	if err == nil || !strings.Contains(err.Error(), "restamp callback exploded") {
		t.Fatalf("err = %v, want the tier's failure", err)
	}
	if tier.calls != 1 {
		t.Errorf("the tier's restamp ran %d time(s), want 1 (the walk stops at the failing chunk)", tier.calls)
	}
	log := strings.Join(store.log, "\n")
	// The failing chunk was re-compressed, the policy re-enabled and the
	// lock released — in that order — and the walk did not go on.
	if !strings.HasSuffix(log, "compress _hyper_1_1_chunk\nresume job 1000\nunlock") {
		t.Errorf("log does not end with the failing chunk's re-compress, the policy re-enable and the unlock:\n%s", log)
	}
	if strings.Contains(log, "_hyper_1_2_chunk") {
		t.Errorf("the walk continued past the failure:\n%s", log)
	}
	wantTeardown(t, store)
	if store.count("pause job") != 1 {
		t.Errorf("policy paused %d time(s), want exactly once:\n%s", store.count("pause job"), log)
	}
	if !strings.Contains(out.String(), "RESUME: stub") {
		t.Errorf("no resume line after the failure:\n%s", out.String())
	}
}

// ─── -chunks is available to BOTH tiers ──────────────────────────────────

// TestValidateRestampTierFlags_ChunkModeIsAvailableToBothTiers pins the
// flag contract the chunk walk's extension to the exact tier rests on:
// `-chunks` and the driver's own flags are accepted with `-tier exact`,
// and every flag that only means something for the anchor re-derive is
// still a refusal there — including `-chunk-batch`, because the exact
// tier's transaction is one `-slice` window and a row batch would be
// silently ignored.
func TestValidateRestampTierFlags_ChunkModeIsAvailableToBothTiers(t *testing.T) {
	t.Parallel()
	for _, f := range append([]string{"chunks"}, restampChunkOnlyFlags...) {
		if f == "chunk-batch" {
			continue
		}
		if err := validateRestampTierFlags(restampTierExact, map[string]bool{"chunks": true, f: true}); err != nil {
			t.Errorf("-%s with -tier exact -chunks was refused: %v", f, err)
		}
	}
	// The whole combination at once, as an operator would type it.
	set := map[string]bool{"chunks": true, "fill-null": true, "min-free-bytes": true, "generation": true, "allow-live-adjacent": true, "resume-paused-policy": true}
	if err := validateRestampTierFlags(restampTierExact, set); err != nil {
		t.Errorf("-tier exact -chunks with the driver's flags was refused: %v", err)
	}
	if err := validateRestampChunkFlags(true, set); err != nil {
		t.Errorf("the chunk-only check refused the same combination: %v", err)
	}
	// And the xlm-base-only flags still are.
	for _, f := range []string{"report", "sample", "batch", "min-rel-delta", "max-generation", "chunk-batch"} {
		err := validateRestampTierFlags(restampTierExact, map[string]bool{"chunks": true, f: true})
		if err == nil || !strings.Contains(err.Error(), "-"+f) {
			t.Errorf("-%s with -tier exact: err = %v, want a refusal naming the flag", f, err)
		}
	}
}
