// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// ─── buildWindowPlan: gap-free, overlap-free, exact coverage ──────────────

// TestBuildWindowPlan_ExactCoverageNoGapsNoOverlaps is the core correctness
// property the parallel scheduler depends on: whatever the window size,
// the plan's windows union to EXACTLY [from,to] with no gap and no overlap,
// in ascending order. A bug here would silently drop or double-process
// ledgers under concurrency (harder to notice than a single-threaded walk
// missing a range, because ON CONFLICT DO NOTHING absorbs an overlap
// silently and a gap has no local symptom).
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

// TestWindowScheduler_ConcurrentClaimExactlyOnce spins many goroutines
// against one scheduler built from a fixed window plan and asserts every
// window is claimed by EXACTLY one caller — the property the parallel
// worker pool depends on for "no gaps, no overlaps" under real
// concurrency (buildWindowPlan proves the plan itself is gap/overlap-free;
// this proves the scheduler doesn't hand a window to two workers or skip
// one under a race).
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

// TestPendingWindows_ResumeFiltering pins the resume semantics: a window is
// skipped ONLY when its checkpoint's last_ledger is at or past its OWN
// upper bound. A missing entry, a different source's entry (prefix
// collision guard), and a stale/partial entry (last < w.To — shouldn't
// happen in practice since checkpoints are written after a window fully
// streams, but defensive) must all be retried, never silently skipped.
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

// ─── writeModeLabel ─────────────────────────────────────────────────────

func TestWriteModeLabel(t *testing.T) {
	if got := writeModeLabel(true); got != "WRITE" {
		t.Errorf("writeModeLabel(true) = %q, want WRITE", got)
	}
	if got := writeModeLabel(false); got == "WRITE" {
		t.Errorf("writeModeLabel(false) = %q, must not be WRITE", got)
	}
}

// TestBuildWindowPlan_WindowSchedulerIntegration sanity-checks the two
// units together: every ledger in [from,to] is covered by exactly one
// claimed window once the scheduler is drained concurrently — the
// end-to-end property RunProjectedRebuild depends on.
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

// TestCheckLiveCursorGuard_ErrorMentionsTheRelevantLedgers asserts the
// refusal message carries the facts an operator needs to act on it
// (current live-cursor position, requested top, and the override flag)
// rather than a generic "refused" with no actionable detail.
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
