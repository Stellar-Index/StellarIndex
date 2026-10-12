// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// ─── buildWindowPlan: gap-free, overlap-free, exact coverage ──────────────

// The parallel scheduler depends on windows unioning to exactly [from,to], ascending, with no gap or overlap at any
// window size. A bug would silently drop or double-process ledgers: ON CONFLICT DO NOTHING absorbs an overlap and a gap
// has no local symptom.
func TestBuildWindowPlan_ExactCoverageNoGapsNoOverlaps(t *testing.T) {
	cases := []struct {
		name           string
		from, to, wnd  uint32
		wantWindowSpan uint32 // for single-window cases, sanity-check the span
	}{
		{"evenly-divides", 1000, 1999, 100, 0},
		{"remainder-absorbed-by-last", 1000, 1949, 100, 0},
		{"window-larger-than-range", 1000, 1050, 100_000, 51},
		{"single-ledger-range", 5, 5, 100, 1},
		{"window-of-one", 1000, 1010, 1, 0},
		{"large-realistic-range", 51_499_546, 62_987_213, 50_000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := buildWindowPlan(tc.from, tc.to, tc.wnd)
			if len(plan) == 0 {
				t.Fatalf("buildWindowPlan(%d,%d,%d) returned no windows", tc.from, tc.to, tc.wnd)
			}
			if plan[0].From != tc.from {
				t.Errorf("first window starts at %d, want %d", plan[0].From, tc.from)
			}
			if last := plan[len(plan)-1].To; last != tc.to {
				t.Errorf("last window ends at %d, want %d", last, tc.to)
			}
			var totalSpan int64
			for i, w := range plan {
				if w.To < w.From {
					t.Fatalf("window %d [%d,%d] has To < From", i, w.From, w.To)
				}
				if span := w.To - w.From + 1; span > tc.wnd && tc.wnd != 0 {
					t.Errorf("window %d [%d,%d] spans %d ledgers, exceeds -window=%d", i, w.From, w.To, span, tc.wnd)
				}
				totalSpan += int64(w.To-w.From) + 1
				if i > 0 {
					prev := plan[i-1]
					if w.From != prev.To+1 {
						t.Errorf("gap or overlap between window %d [%d,%d] and window %d [%d,%d]",
							i-1, prev.From, prev.To, i, w.From, w.To)
					}
				}
			}
			wantSpan := int64(tc.to-tc.from) + 1
			if totalSpan != wantSpan {
				t.Errorf("total windowed span = %d, want %d (from=%d to=%d)", totalSpan, wantSpan, tc.from, tc.to)
			}
			if tc.wantWindowSpan != 0 && len(plan) == 1 {
				if span := plan[0].To - plan[0].From + 1; span != tc.wantWindowSpan {
					t.Errorf("single-window span = %d, want %d", span, tc.wantWindowSpan)
				}
			}
		})
	}
}

func TestBuildWindowPlan_InvalidRangeReturnsNil(t *testing.T) {
	if got := buildWindowPlan(100, 50, 10); got != nil {
		t.Errorf("to<from: got %v, want nil", got)
	}
	if got := buildWindowPlan(100, 200, 0); got != nil {
		t.Errorf("window=0: got %v, want nil", got)
	}
}

// ─── windowScheduler: concurrency-safe claim ───────────────────────────────

// TestWindowScheduler_ConcurrentClaimExactlyOnce asserts that many goroutines on one scheduler
// claim every window EXACTLY once. buildWindowPlan proves the plan is gap/overlap-free; this
// proves the scheduler never hands a window to two workers or skips one under a race.
func TestWindowScheduler_ConcurrentClaimExactlyOnce(t *testing.T) {
	plan := buildWindowPlan(1000, 1000+237*50-1, 50) // 237 windows
	if len(plan) != 237 {
		t.Fatalf("test setup: got %d windows, want 237", len(plan))
	}
	sched := newWindowScheduler(plan)

	const goroutines = 32
	claimed := make([][]opsutil.RangeChunk, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for {
				w, ok := sched.claim()
				if !ok {
					return
				}
				claimed[idx] = append(claimed[idx], w)
			}
		}(g)
	}
	wg.Wait()

	seen := make(map[opsutil.RangeChunk]int)
	total := 0
	for _, list := range claimed {
		for _, w := range list {
			seen[w]++
			total++
		}
	}
	if total != len(plan) {
		t.Fatalf("claimed %d windows total, want %d", total, len(plan))
	}
	for _, w := range plan {
		if n := seen[w]; n != 1 {
			t.Errorf("window [%d,%d] claimed %d times, want exactly 1", w.From, w.To, n)
		}
	}
	// Exhausted: further claims all report not-ok.
	for i := 0; i < 5; i++ {
		if _, ok := sched.claim(); ok {
			t.Fatalf("claim() after exhaustion returned ok=true")
		}
	}
}

func TestWindowScheduler_EmptyPlanClaimsNothing(t *testing.T) {
	sched := newWindowScheduler(nil)
	if _, ok := sched.claim(); ok {
		t.Fatalf("claim() on empty scheduler returned ok=true")
	}
}

// ─── pendingWindows / windowCursorSub: resume correctness ─────────────────

func TestWindowCursorSub_Format(t *testing.T) {
	got := windowCursorSub("blend_backstop", opsutil.RangeChunk{From: 51_499_546, To: 51_549_545})
	want := "blend_backstop:51499546-51549545"
	if got != want {
		t.Errorf("windowCursorSub = %q, want %q", got, want)
	}
}

// Resume semantics: a window is skipped only when its checkpoint's last_ledger is at or past its OWN upper bound. A
// missing entry, another source's entry (prefix collision guard) or a stale/partial entry (last < w.To) must be retried.
func TestPendingWindows_ResumeFiltering(t *testing.T) {
	plan := []opsutil.RangeChunk{
		{From: 100, To: 199}, // done
		{From: 200, To: 299}, // stale/partial checkpoint (last < To) — must NOT be skipped
		{From: 300, To: 399}, // no checkpoint at all — must NOT be skipped
		{From: 400, To: 499}, // done
	}
	done := map[string]uint32{
		windowCursorSub("aquarius", plan[0]): 199,
		windowCursorSub("aquarius", plan[1]): 250, // < 299
		windowCursorSub("aquarius", plan[3]): 499,
		// A different source's identically-numbered window must never
		// leak into this source's resume set.
		windowCursorSub("blend", plan[2]): 399,
	}

	got := pendingWindows(plan, "aquarius", done)
	want := []opsutil.RangeChunk{plan[1], plan[2]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pendingWindows = %v, want %v", got, want)
	}
}

func TestPendingWindows_NoCheckpointsReturnsWholePlan(t *testing.T) {
	plan := buildWindowPlan(1, 100, 10)
	got := pendingWindows(plan, "rozo", nil)
	if !reflect.DeepEqual(got, plan) {
		t.Errorf("pendingWindows with empty done map should return the plan unchanged")
	}
}

// ─── checkLiveCursorGuard: the ADR-0048 D3 one-writer contract ────────────

func TestCheckLiveCursorGuard(t *testing.T) {
	cases := []struct {
		name           string
		haveLive       bool
		liveLastLedger uint32
		to             uint32
		allowOverlap   bool
		wantErr        bool
	}{
		{"live-at-or-above-to: allowed", true, 63_000_000, 62_000_000, false, false},
		{"live-exactly-at-to: allowed (boundary)", true, 62_000_000, 62_000_000, false, false},
		{"live-below-to: refused", true, 61_000_000, 62_000_000, false, true},
		{"no-live-cursor-at-all: refused by default", false, 0, 62_000_000, false, true},
		{"live-below-to-but-override: allowed", true, 61_000_000, 62_000_000, true, false},
		{"no-live-cursor-but-override: allowed", false, 0, 62_000_000, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkLiveCursorGuard(tc.haveLive, tc.liveLastLedger, tc.to, tc.allowOverlap)
			if tc.wantErr && err == nil {
				t.Errorf("checkLiveCursorGuard(%v,%d,%d,%v) = nil, want an error", tc.haveLive, tc.liveLastLedger, tc.to, tc.allowOverlap)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkLiveCursorGuard(%v,%d,%d,%v) = %v, want nil", tc.haveLive, tc.liveLastLedger, tc.to, tc.allowOverlap, err)
			}
		})
	}
}

// ─── decodeProjectedEvent: Matches/Decode/panic-recover discipline ────────

type fakeEvent struct {
	kind, source string
}

func (f fakeEvent) EventKind() string { return f.kind }
func (f fakeEvent) Source() string    { return f.source }

var _ consumer.Event = fakeEvent{}

// fakeDecoder implements dispatcher.Decoder with configurable behavior for
// exercising decodeProjectedEvent's three paths (no-match, decode error,
// decode panic) without needing a real protocol decoder.
type fakeDecoder struct {
	matches    bool
	decodeOuts []consumer.Event
	decodeErr  error
	panics     bool
}

func (f *fakeDecoder) Name() string              { return "fake" }
func (f *fakeDecoder) Matches(events.Event) bool { return f.matches }
func (f *fakeDecoder) Decode(events.Event) ([]consumer.Event, error) {
	if f.panics {
		panic("simulated decoder panic")
	}
	return f.decodeOuts, f.decodeErr
}

func TestDecodeProjectedEvent_NoMatchReturnsNothingWithoutDecoding(t *testing.T) {
	dec := &fakeDecoder{matches: false, panics: true} // would panic if Decode were called
	outs, softFail := decodeProjectedEvent("fake", dec, events.Event{}, slog.Default())
	if outs != nil || softFail {
		t.Errorf("no-match: got (%v,%v), want (nil,false)", outs, softFail)
	}
}

func TestDecodeProjectedEvent_SuccessReturnsOutputs(t *testing.T) {
	want := []consumer.Event{fakeEvent{kind: "fake.thing", source: "fake"}}
	dec := &fakeDecoder{matches: true, decodeOuts: want}
	outs, softFail := decodeProjectedEvent("fake", dec, events.Event{}, slog.Default())
	if softFail {
		t.Errorf("success: softFail = true, want false")
	}
	if !reflect.DeepEqual(outs, want) {
		t.Errorf("success: outs = %v, want %v", outs, want)
	}
}

func TestDecodeProjectedEvent_DecodeErrorIsSoftFail(t *testing.T) {
	dec := &fakeDecoder{matches: true, decodeErr: errors.New("malformed")}
	outs, softFail := decodeProjectedEvent("fake", dec, events.Event{}, slog.Default())
	if !softFail {
		t.Errorf("decode error: softFail = false, want true")
	}
	if outs != nil {
		t.Errorf("decode error: outs = %v, want nil", outs)
	}
}

func TestDecodeProjectedEvent_PanicIsRecoveredAsSoftFail(t *testing.T) {
	dec := &fakeDecoder{matches: true, panics: true}
	outs, softFail := decodeProjectedEvent("fake", dec, events.Event{Ledger: 42}, slog.Default())
	if !softFail {
		t.Errorf("panic: softFail = false, want true")
	}
	if outs != nil {
		t.Errorf("panic: outs = %v, want nil", outs)
	}
}

// The soft-fail skips the row, so the decode error must be logged.
func TestDecodeProjectedEvent_DecodeErrorIsLogged(t *testing.T) {
	var buf strings.Builder
	dec := &fakeDecoder{matches: true, decodeErr: errors.New("unknown map field amount_v9")}
	_, softFail := decodeProjectedEvent("fake", dec, events.Event{Ledger: 61234567}, slog.New(slog.NewTextHandler(&buf, nil)))
	if !softFail {
		t.Fatal("decode error: softFail = false, want true")
	}
	for _, want := range []string{"unknown map field amount_v9", "source=fake", "ledger=61234567"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log %q missing %q", buf.String(), want)
		}
	}
}

// ─── writeModeLabel ─────────────────────────────────────────────────────

func TestWriteModeLabel(t *testing.T) {
	if got := writeModeLabel(true); got != "WRITE" {
		t.Errorf("writeModeLabel(true) = %q, want WRITE", got)
	}
	if got := writeModeLabel(false); got == "WRITE" {
		t.Errorf("writeModeLabel(false) = %q, must not be WRITE", got)
	}
}

// Sanity-checks plan and scheduler together: once the scheduler is drained concurrently, every ledger in [from,to] is
// covered by exactly one claimed window, the property RunProjectedRebuild depends on.
func TestBuildWindowPlan_WindowSchedulerIntegration(t *testing.T) {
	const from, to, window = 51_499_546, 51_499_546 + 9_999, 137
	plan := buildWindowPlan(from, to, window)
	sched := newWindowScheduler(plan)

	covered := make(map[uint32]bool, to-from+1)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				w, ok := sched.claim()
				if !ok {
					return
				}
				mu.Lock()
				for l := w.From; l <= w.To; l++ {
					if covered[l] {
						t.Errorf("ledger %d covered by more than one window", l)
					}
					covered[l] = true
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	for l := uint32(from); l <= to; l++ {
		if !covered[l] {
			t.Fatalf("ledger %d never covered", l)
		}
	}
}

// The refusal message must carry what an operator needs to act: the current live-cursor position, the requested top and
// the override flag, not a generic "refused".
func TestCheckLiveCursorGuard_ErrorMentionsTheRelevantLedgers(t *testing.T) {
	err := checkLiveCursorGuard(true, 61_000_000, 62_000_000, false)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"61000000", "62000000", "allow-live-overlap"} {
		if !strings.Contains(msg, want) {
			t.Errorf("guard error %q missing %q", msg, want)
		}
	}
}

// A run whose workers all returned cleanly but which held windows or dropped
// trades lost rows; the command must exit non-zero so an unattended caller
// does not read it as a complete rebuild.
func TestProjectedRebuildOutcome_LossyRunExitsNonZero(t *testing.T) {
	cases := []struct {
		name string
		r    ProjectedRebuildResult
	}{
		{"held windows", ProjectedRebuildResult{WindowsProcessed: 4, WindowsHeld: 2, InsertErrors: 5}},
		{"permanent drops", ProjectedRebuildResult{WindowsProcessed: 4, PermanentDrops: 3}},
	}
	for _, tc := range cases {
		err := projectedRebuildOutcome(tc.r, nil, false)
		if !errors.Is(err, errProjectedRebuildIncomplete) {
			t.Errorf("%s: outcome = %v, want errProjectedRebuildIncomplete", tc.name, err)
		}
	}

	if err := projectedRebuildOutcome(ProjectedRebuildResult{WindowsProcessed: 4}, nil, false); err != nil {
		t.Errorf("clean run: outcome = %v, want nil", err)
	}
	fatal := errors.New("worker fatal")
	if err := projectedRebuildOutcome(ProjectedRebuildResult{}, fatal, false); !errors.Is(err, fatal) {
		t.Errorf("fatal run: outcome = %v, want it to wrap the worker error", err)
	}
	if err := projectedRebuildOutcome(ProjectedRebuildResult{WindowsHeld: 1}, fatal, true); err != nil {
		t.Errorf("interrupted run: outcome = %v, want nil (re-run with -resume)", err)
	}
}

// orphanFakeDecoder is a correlation-buffer decoder that reports evicted orphans.
type orphanFakeDecoder struct {
	fakeDecoder
	orphans int
}

func (f *orphanFakeDecoder) EvictedOrphans() int { return f.orphans }

// A rebuild whose decoder evicted orphaned swaps must say so in its result,
// its summary and its log, not read as a clean run.
func TestCollectEvictedOrphans_ReachesResultSummaryAndLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	src := projector.Source{Name: "soroswap", Decoder: &orphanFakeDecoder{orphans: 7}}

	n := collectEvictedOrphans(src, logger)
	if n != 7 {
		t.Fatalf("collectEvictedOrphans = %d, want 7", n)
	}
	for _, want := range []string{"level=WARN", "source=soroswap", "evicted_orphans=7"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log missing %q:\n%s", want, buf.String())
		}
	}
	lines := strings.Join(projectedRebuildLossLines(ProjectedRebuildResult{EvictedOrphans: n}), "\n")
	if !strings.Contains(lines, "7 orphaned event(s) EVICTED") {
		t.Errorf("summary loss lines missing the evicted count:\n%s", lines)
	}

	buf.Reset()
	if n := collectEvictedOrphans(projector.Source{Name: "soroswap", Decoder: &orphanFakeDecoder{}}, logger); n != 0 || buf.Len() != 0 {
		t.Errorf("zero evictions: got %d and log %q, want 0 and no log", n, buf.String())
	}
	if n := collectEvictedOrphans(projector.Source{Name: "aquarius", Decoder: &fakeDecoder{}}, logger); n != 0 || buf.Len() != 0 {
		t.Errorf("stateless decoder: got %d and log %q, want 0 and no log", n, buf.String())
	}
	if got := projectedRebuildLossLines(ProjectedRebuildResult{}); len(got) != 0 {
		t.Errorf("clean run loss lines = %q, want none", got)
	}
}

// TestSelectProjectedSource_WatchedSEP41DoesNotCaptureOtherNames pins that a
// non-projected -source is refused even though BuildRegistry appends the sep41
// sources whenever contracts are watched.
func TestSelectProjectedSource_WatchedSEP41DoesNotCaptureOtherNames(t *testing.T) {
	watched := []string{"CWATCHEDCONTRACT0000000000000000000000000000000000000000"}
	for _, name := range []string{"sdex", "band", "soroswap-router"} {
		reg, err := projector.BuildRegistry([]string{name}, config.OracleConfig{}, watched, nil)
		if err != nil {
			t.Fatalf("BuildRegistry(%q): %v", name, err)
		}
		if len(reg.Sources) == 0 {
			t.Fatalf("BuildRegistry(%q) with watched contracts: want the auto-included sep41 sources, got none", name)
		}
		if src, err := selectProjectedSource(reg, name); err == nil {
			t.Errorf("selectProjectedSource(%q) = %q, want a not-a-projector-source error", name, src.Name)
		} else if !strings.Contains(err.Error(), "is not a projector source") {
			t.Errorf("selectProjectedSource(%q) error = %v", name, err)
		}
	}
	for _, name := range []string{"sep41_transfers", "sep41_supply", "comet", "  Comet  "} {
		reg, err := projector.BuildRegistry([]string{name}, config.OracleConfig{}, watched, nil)
		if err != nil {
			t.Fatalf("BuildRegistry(%q): %v", name, err)
		}
		src, err := selectProjectedSource(reg, name)
		if err != nil {
			t.Fatalf("selectProjectedSource(%q): %v", name, err)
		}
		if want := strings.ToLower(strings.TrimSpace(name)); src.Name != want {
			t.Errorf("selectProjectedSource(%q) = %q, want %q", name, src.Name, want)
		}
	}
}

func TestCapProjectedRebuildWorkers_StaysInsideQueryBudget(t *testing.T) {
	for _, n := range []int{-1, 0, 1, 3, 4, 8, 64} {
		got := capProjectedRebuildWorkers(n)
		if got < 1 {
			t.Fatalf("n=%d: got %d workers, want >= 1", n, got)
		}
		if q := got * projectedRebuildQueriesPerWorker; q >= projectedRebuildQueryBudget {
			t.Errorf("n=%d: %d workers hold %d queries, ops_batch admits %d and one slot must stay free", n, got, q, projectedRebuildQueryBudget)
		}
	}
	if got := capProjectedRebuildWorkers(2); got != 2 {
		t.Errorf("a request inside the budget must pass through, got %d", got)
	}
}

// TestFinishProjectedRebuild_HeldWindowReachesTheHeartbeat pins that a run
// which lost rows is visible to stellarindex_ops_job_run_failed, not only
// to a caller that reads the exit code.
func TestFinishProjectedRebuild_HeldWindowReachesTheHeartbeat(t *testing.T) {
	cases := []struct {
		name        string
		result      ProjectedRebuildResult
		runErr      error
		interrupted bool
		wantErr     error
		wantExitOK  string
	}{
		{name: "held window", result: ProjectedRebuildResult{WindowsHeld: 1, InsertErrors: 3}, wantErr: errProjectedRebuildIncomplete, wantExitOK: "0"},
		{name: "permanent drop", result: ProjectedRebuildResult{PermanentDrops: 2}, wantErr: errProjectedRebuildIncomplete, wantExitOK: "0"},
		{name: "interrupted", runErr: context.Canceled, interrupted: true, wantExitOK: "0"},
		{name: "clean", wantExitOK: "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hb.prom")
			hb := opsutil.NewJobHeartbeat("projected-rebuild-blend", path, nil)
			hb.Start()
			err := finishProjectedRebuild(hb, tc.result, tc.runErr, tc.interrupted)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("want exit 0, got %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			want := `stellarindex_ops_job_last_exit_ok{ops_job="projected-rebuild-blend"} ` + tc.wantExitOK
			if !strings.Contains(string(body), want) {
				t.Fatalf("heartbeat missing %q:\n%s", want, body)
			}
		})
	}
}

// TestProjectedRebuildProgressLoop_FeedsTheHeartbeat pins that a heartbeat
// run publishes covered ledgers, so stellarindex_ops_job_no_progress does not
// ticket a healthy rebuild whose progress_total would otherwise sit at 0.
func TestProjectedRebuildProgressLoop_FeedsTheHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hb.prom")
	hb := opsutil.NewJobHeartbeat("projected-rebuild-blend", path, nil)
	hb.Start()
	counters := newProjectedRebuildCounters()
	counters.completedLedgers.Store(50000)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runProjectedRebuildProgressLoop(ctx, logger, "blend", 5*time.Millisecond, time.Now(), 1, 50000, counters, hb)
	hb.Stop(true)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `stellarindex_ops_job_progress_total{ops_job="projected-rebuild-blend"} 50000`
	if !strings.Contains(string(body), want) {
		t.Fatalf("heartbeat missing %q:\n%s", want, body)
	}
}

// writeOptsFor returns -write options over a decoder emitting outs for every row. Store is nil on purpose: the poison
// trade is rejected by Trade.Validate before the store is touched, and the non-trade output's nil-store panic is
// recovered by pipeline.HandleEvent into a generic unclassified error.
func writeOptsFor(outs ...consumer.Event) ProjectedRebuildOptions {
	return ProjectedRebuildOptions{
		Write:  true,
		Source: projector.Source{Name: "soroswap", Decoder: &fakeDecoder{matches: true, decodeOuts: outs}},
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// poisonSoroswapTrade is a trade the store can never hold (the zero-value
// canonical.Trade fails Validate), so pipeline.HandleEvent — the production
// sink this tool writes through — returns a *pipeline.TradeDroppedError.
func poisonSoroswapTrade() consumer.Event {
	return soroswap.TradeEvent{Trade: canonical.Trade{Source: "soroswap", Ledger: 77, TxHash: "deadbeef"}}
}

// HandleEvent reports a permanently dropped trade; counting it as a window-holding insert error would leave a
// deterministically poison trade's window un-checkpointed on every resume. The drop is counted separately, adds nothing
// to the holding count, so the window checkpoints and the loss stays visible.
func TestApplyProjectedEvent_PermanentlyDroppedTradeDoesNotHoldTheWindow(t *testing.T) {
	counters := newProjectedRebuildCounters()
	emitted, insertErrs := applyProjectedEvent(context.Background(),
		writeOptsFor(poisonSoroswapTrade()), quietLogger(), counters, events.Event{Ledger: 77})

	if emitted != 1 {
		t.Errorf("emitted = %d, want 1", emitted)
	}
	if insertErrs != 0 {
		t.Errorf("window-holding insert errors = %d, want 0 — a deterministic store rejection must not hold the window: no re-run can land it, so every resumed run would redo this window forever", insertErrs)
	}
	if got := counters.permanentDrops.Load(); got != 1 {
		t.Errorf("permanentDrops = %d, want 1 — the drop must be counted, not vanish", got)
	}
}

// Guard for the other side of the permanent-drop rule: a failure that is NOT a permanent trade drop still holds the
// window. A poison trade beside it does not mask it, and every output of the row is still offered to the sink.
func TestApplyProjectedEvent_OtherFailuresStillHoldTheWindow(t *testing.T) {
	counters := newProjectedRebuildCounters()
	emitted, insertErrs := applyProjectedEvent(context.Background(),
		writeOptsFor(poisonSoroswapTrade(), rozo.Event{}, poisonSoroswapTrade()),
		quietLogger(), counters, events.Event{Ledger: 77})

	if emitted != 3 {
		t.Errorf("emitted = %d, want 3 — every output of the row is offered to the sink", emitted)
	}
	if insertErrs != 1 {
		t.Errorf("window-holding insert errors = %d, want 1 (the failed non-trade insert, and only it)", insertErrs)
	}
	if got := counters.permanentDrops.Load(); got != 2 {
		t.Errorf("permanentDrops = %d, want 2 — one per dropped trade", got)
	}
}

// The two loss classes need opposite operator actions, so the summary must not blur them: a held window says re-run; a
// permanent drop must say a re-run cannot land it.
func TestProjectedRebuildLossLines_NeverTellsTheOperatorToReRunAPermanentDrop(t *testing.T) {
	if got := projectedRebuildLossLines(ProjectedRebuildResult{}); len(got) != 0 {
		t.Errorf("clean run printed loss lines: %q", got)
	}

	drops := strings.Join(projectedRebuildLossLines(ProjectedRebuildResult{PermanentDrops: 3}), "\n")
	for _, want := range []string{"3 trade(s) PERMANENTLY rejected", "ARE checkpointed", "re-run CANNOT land them", "-resume=false"} {
		if !strings.Contains(drops, want) {
			t.Errorf("permanent-drop report is missing %q:\n%s", want, drops)
		}
	}
	if strings.Contains(drops, "NOT checkpointed") || strings.Contains(drops, "RE-RUN the same command") {
		t.Errorf("permanent-drop report must not claim a held window or prescribe a plain re-run:\n%s", drops)
	}

	held := strings.Join(projectedRebuildLossLines(ProjectedRebuildResult{WindowsHeld: 2, InsertErrors: 5}), "\n")
	for _, want := range []string{"2 window(s) NOT checkpointed", "5 insert(s) failed", "RE-RUN the same command"} {
		if !strings.Contains(held, want) {
			t.Errorf("held-window report is missing %q:\n%s", want, held)
		}
	}
	if strings.Contains(held, "PERMANENTLY rejected") {
		t.Errorf("held-window report must not mention permanent drops when there are none:\n%s", held)
	}
}

type fakeSEP41RollupResetter struct {
	calls     int
	contracts [][]string
	ctxErr    error
	err       error
}

func (f *fakeSEP41RollupResetter) ResetSEP41SupplyRollupFold(ctx context.Context, contractIDs []string) (int64, error) {
	f.calls++
	f.contracts = append(f.contracts, contractIDs)
	f.ctxErr = ctx.Err()
	return 3, f.err
}

func TestResetSEP41RollupAfterRebuild_FullResetOnSEP41SupplyWrite(t *testing.T) {
	f := &fakeSEP41RollupResetter{}
	var out bytes.Buffer
	if err := resetSEP41RollupAfterRebuild(context.Background(), &out, f, sep41supply.SourceName, true); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("ResetSEP41SupplyRollupFold called %d times, want 1", f.calls)
	}
	if f.contracts[0] != nil {
		t.Errorf("contract scope = %v, want nil (FULL reset)", f.contracts[0])
	}
	if !strings.Contains(out.String(), "reset 3 sep41_supply_rollup fold row(s)") {
		t.Errorf("output %q does not report the reset row count", out.String())
	}
}

// An interrupted run's ctx is already cancelled, but the windows it wrote
// still sit below the checkpoint, so the reset must not inherit the cancel.
func TestResetSEP41RollupAfterRebuild_RunsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeSEP41RollupResetter{}
	if err := resetSEP41RollupAfterRebuild(ctx, &bytes.Buffer{}, f, sep41supply.SourceName, true); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if f.calls != 1 || f.ctxErr != nil {
		t.Fatalf("calls=%d ctxErr=%v, want 1 call on a live context", f.calls, f.ctxErr)
	}
}

func TestResetSEP41RollupAfterRebuild_NoResetForDryRunOrOtherSources(t *testing.T) {
	for _, tc := range []struct {
		source string
		write  bool
	}{
		{sep41supply.SourceName, false},
		{"aquarius", true},
		{"sep41_transfers", true},
	} {
		f := &fakeSEP41RollupResetter{}
		var out bytes.Buffer
		if err := resetSEP41RollupAfterRebuild(context.Background(), &out, f, tc.source, tc.write); err != nil {
			t.Fatalf("%s write=%v: %v", tc.source, tc.write, err)
		}
		if f.calls != 0 {
			t.Errorf("%s write=%v: ResetSEP41SupplyRollupFold called %d times, want 0", tc.source, tc.write, f.calls)
		}
		if wantNote := tc.source == sep41supply.SourceName; strings.Contains(out.String(), "would then ResetSEP41SupplyRollupFold") != wantNote {
			t.Errorf("%s write=%v: dry-run note present=%v, want %v (output %q)", tc.source, tc.write, !wantNote, wantNote, out.String())
		}
	}
}

func TestResetSEP41RollupAfterRebuild_PropagatesError(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeSEP41RollupResetter{err: boom}
	if err := resetSEP41RollupAfterRebuild(context.Background(), &bytes.Buffer{}, f, sep41supply.SourceName, true); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the resetter's error", err)
	}
}
