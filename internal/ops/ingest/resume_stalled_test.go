package ingest

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestParseStalledCursor covers every branch of the sub_source parser
// + skip-reason path. Inputs are crafted to mirror the actual cursor
// shapes seen in production (per the 2026-05-28 r1 stall sweep).
func TestParseStalledCursor(t *testing.T) {
	cases := []struct {
		name         string
		cursor       timescale.Cursor
		wantSkip     bool
		wantFrom     uint32
		wantTo       uint32
		wantSources  []string
		wantContains string // substring expected in skipReason when wantSkip=true
	}{
		{
			name: "well-formed multi-decoder stall — resume window non-empty",
			cursor: timescale.Cursor{
				Sub:        "62200000-62210000:aquarius,band,sdex",
				LastLedger: 62205555,
			},
			wantFrom:    62205556,
			wantTo:      62210000,
			wantSources: []string{"aquarius", "band", "sdex"},
		},
		{
			name: "well-formed single-decoder stall (typical defindex/soroswap shape)",
			cursor: timescale.Cursor{
				Sub:        "53500714-54242319:soroswap",
				LastLedger: 54242000,
			},
			wantFrom:    54242001,
			wantTo:      54242319,
			wantSources: []string{"soroswap"},
		},
		{
			name: "last_ledger at-or-past target — stale-by-time only",
			cursor: timescale.Cursor{
				Sub:        "53500714-54242319:soroswap",
				LastLedger: 54242319,
			},
			wantSkip:     true,
			wantContains: "already at-or-past target",
		},
		{
			name: "last_ledger > target — also skip (cursor walked past target)",
			cursor: timescale.Cursor{
				Sub:        "53500714-54242319:soroswap",
				LastLedger: 54300000,
			},
			wantSkip:     true,
			wantContains: "already at-or-past target",
		},
		{
			name: "declared from > last_ledger+1 — inconsistent cursor",
			cursor: timescale.Cursor{
				Sub:        "62500000-62600000:phoenix",
				LastLedger: 62000000,
			},
			wantSkip:     true,
			wantContains: "cursor inconsistent",
		},
		{
			name: "garbage sub_source — refuse rather than guess",
			cursor: timescale.Cursor{
				Sub:        "not-a-range",
				LastLedger: 1000,
			},
			wantSkip:     true,
			wantContains: "doesn't match",
		},
		{
			name: "missing colon — refuse",
			cursor: timescale.Cursor{
				Sub:        "62200000-62210000",
				LastLedger: 62205555,
			},
			wantSkip:     true,
			wantContains: "doesn't match",
		},
		{
			name: "empty decoder list after colon — refuse",
			cursor: timescale.Cursor{
				Sub:        "62200000-62210000:",
				LastLedger: 62205555,
			},
			wantSkip:     true,
			wantContains: "doesn't match", // regex fails on `(.+)$` against empty
		},
		{
			name: "from overflows uint32 — refuse",
			cursor: timescale.Cursor{
				Sub:        "5000000000-5000010000:sdex", // > uint32 max ~4.29B
				LastLedger: 0,
			},
			wantSkip:     true,
			wantContains: "parse from",
		},
		{
			name: "to overflows uint32 — refuse",
			cursor: timescale.Cursor{
				Sub:        "62200000-5000000000:sdex",
				LastLedger: 62205555,
			},
			wantSkip:     true,
			wantContains: "parse to",
		},
		{
			name: "decoder CSV gets sorted",
			cursor: timescale.Cursor{
				Sub:        "62200000-62210000:soroswap,aquarius,blend",
				LastLedger: 62205555,
			},
			wantFrom:    62205556,
			wantTo:      62210000,
			wantSources: []string{"aquarius", "blend", "soroswap"}, // sorted
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseStalledCursor(tc.cursor)
			if got.skip != tc.wantSkip {
				t.Errorf("skip = %v, want %v (reason=%q)", got.skip, tc.wantSkip, got.skipReason)
			}
			if tc.wantSkip {
				if tc.wantContains != "" && !contains(got.skipReason, tc.wantContains) {
					t.Errorf("skipReason = %q, want substring %q", got.skipReason, tc.wantContains)
				}
				return
			}
			if got.rangeFrom != tc.wantFrom {
				t.Errorf("rangeFrom = %d, want %d", got.rangeFrom, tc.wantFrom)
			}
			if got.rangeTo != tc.wantTo {
				t.Errorf("rangeTo = %d, want %d", got.rangeTo, tc.wantTo)
			}
			if !reflect.DeepEqual(got.sources, tc.wantSources) {
				t.Errorf("sources = %v, want %v", got.sources, tc.wantSources)
			}
		})
	}
}

// TestParseStalledCursor_RoundTripsBackfillCursorSub pins the contract
// between `backfillCursorSub` (the producer) and `parseStalledCursor`
// (the consumer) — if either side changes its format the round-trip
// breaks and this test fires before resume-stalled silently drops
// every stalled cursor as "doesn't match shape".
func TestParseStalledCursor_RoundTripsBackfillCursorSub(t *testing.T) {
	opts := backfillOpts{
		from:    62_200_000,
		to:      62_210_000,
		sources: []string{"sdex", "aquarius", "blend"}, // unsorted on input
	}
	sub := backfillCursorSub(opts)
	c := timescale.Cursor{Sub: sub, LastLedger: 62_205_000}
	p := parseStalledCursor(c)
	if p.skip {
		t.Fatalf("round-trip skipped: %q (sub=%q)", p.skipReason, sub)
	}
	if p.rangeFrom != 62_205_001 || p.rangeTo != 62_210_000 {
		t.Errorf("range = [%d, %d], want [62205001, 62210000]", p.rangeFrom, p.rangeTo)
	}
	want := []string{"aquarius", "blend", "sdex"} // both sides sort
	if !reflect.DeepEqual(p.sources, want) {
		t.Errorf("sources = %v, want %v", p.sources, want)
	}
}

// TestResumeChunkFrom_SequentialReproducesOriginalCursorSub is the
// AGT-01 / DAT-12 / REL-09 regression: at parallel<=1 (the default),
// runOneCursorPlan must key the resumed chunk on the ORIGINAL declared
// `from` so backfillCursorSub reproduces the exact sub_source of the
// stalled cursor row — advancing that row in place — rather than a
// sibling row keyed on [last_ledger+1, to] that the stalled row's
// -resume path will never touch again.
func TestResumeChunkFrom_SequentialReproducesOriginalCursorSub(t *testing.T) {
	orig := timescale.Cursor{
		Sub:        "62200000-62210000:aquarius,band,sdex",
		LastLedger: 62205555,
	}
	p := parseStalledCursor(orig)
	if p.skip {
		t.Fatalf("unexpected skip: %s", p.skipReason)
	}

	chunkFrom := resumeChunkFrom(p, 1) // parallel=1, the resume-stalled default
	gotSub := backfillCursorSub(backfillOpts{from: chunkFrom, to: p.rangeTo, sources: p.sources})
	if gotSub != orig.Sub {
		t.Fatalf("resumed cursor sub_source = %q, want the ORIGINAL %q (a mismatch forks a sibling cursor row and the stalled row is never advanced/cleared)",
			gotSub, orig.Sub)
	}

	// And the chunk base must be the DECLARED from, not last_ledger+1.
	if chunkFrom != 62200000 {
		t.Fatalf("chunkFrom = %d, want the original declared from 62200000", chunkFrom)
	}
}

// TestResumeChunkFrom_ParallelUsesRemainingRange: at parallel>1 the
// resumed range is split into independent sub-chunks that each need
// their own cursor row, so the chunk base stays the REMAINING range
// (last_ledger+1), not the original declared from.
func TestResumeChunkFrom_ParallelUsesRemainingRange(t *testing.T) {
	orig := timescale.Cursor{
		Sub:        "62200000-62210000:aquarius,band,sdex",
		LastLedger: 62205555,
	}
	p := parseStalledCursor(orig)
	if p.skip {
		t.Fatalf("unexpected skip: %s", p.skipReason)
	}
	if got := resumeChunkFrom(p, 4); got != p.rangeFrom {
		t.Fatalf("resumeChunkFrom(parallel=4) = %d, want rangeFrom %d", got, p.rangeFrom)
	}
}

// TestOverlapsAnyDataGap covers the small interval-overlap helper.
// The function is the gate's primitive — a bug here would let
// false-positive plans through (act on cursors that aren't real
// gaps) or filter out genuine ones (miss real coverage holes).
func TestOverlapsAnyDataGap(t *testing.T) {
	gaps := []timescale.LedgerGap{
		{Start: 62642781, End: 62735517, Size: 92737},
		{Start: 62746866, End: 62757524, Size: 10659},
	}
	cases := []struct {
		name     string
		from, to uint32
		want     bool
	}{
		{name: "fully contained inside first gap", from: 62700000, to: 62710000, want: true},
		{name: "starts before, ends inside first gap", from: 62600000, to: 62700000, want: true},
		{name: "starts inside first gap, ends after", from: 62700000, to: 62800000, want: true},
		{name: "spans first gap entirely (and beyond)", from: 62500000, to: 62800000, want: true},
		{name: "between the two gaps", from: 62735518, to: 62746865, want: false},
		{name: "fully before any gap", from: 50000000, to: 50100000, want: false},
		{name: "fully after both gaps", from: 62800000, to: 62900000, want: false},
		{name: "exact match second gap", from: 62746866, to: 62757524, want: true},
		{name: "ends one before first gap start", from: 62500000, to: 62642780, want: false},
		{name: "starts one after first gap end", from: 62735518, to: 62740000, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := overlapsAnyDataGap(tc.from, tc.to, gaps)
			if got != tc.want {
				t.Errorf("overlapsAnyDataGap([%d, %d]) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestPlanHasSorobanDecoder verifies which decoder shapes route
// through the data-gap gate. SDEX-only plans skip the gate (no
// data-derived gap signal for the trades table yet); any plan
// containing at least one Soroban-era decoder name DOES go through.
func TestPlanHasSorobanDecoder(t *testing.T) {
	cases := []struct {
		name    string
		sources []string
		want    bool
	}{
		{name: "sdex only", sources: []string{"sdex"}, want: false},
		{name: "aquarius alone", sources: []string{"aquarius"}, want: true},
		{name: "defindex alone", sources: []string{"defindex"}, want: true},
		{name: "soroban-events pseudo", sources: []string{"soroban-events"}, want: true},
		{name: "mixed sdex + Soroban DEXes", sources: []string{"aquarius", "comet", "phoenix", "sdex", "soroswap"}, want: true},
		{name: "empty", sources: nil, want: false},
		{name: "unknown decoder", sources: []string{"some-future-source"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planHasSorobanDecoder(tc.sources)
			if got != tc.want {
				t.Errorf("planHasSorobanDecoder(%v) = %v, want %v", tc.sources, got, tc.want)
			}
		})
	}
}

// TestHasNonSorobanDecoder_AndGateSelection verifies hasNonSorobanDecoder
// (used both to select the mixed-plan gate path and to decide whether
// anyPlanNeedsClassicGate must build the classicGapGate at all).
func TestHasNonSorobanDecoder_AndGateSelection(t *testing.T) {
	cases := []struct {
		name    string
		sources []string
		want    bool
	}{
		{name: "sdex only", sources: []string{"sdex"}, want: true},
		{name: "soroban only", sources: []string{"aquarius"}, want: false},
		{name: "mixed", sources: []string{"aquarius", "sdex"}, want: true},
		{name: "unknown decoder counts as non-soroban", sources: []string{"some-future-source"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasNonSorobanDecoder(tc.sources); got != tc.want {
				t.Errorf("hasNonSorobanDecoder(%v) = %v, want %v", tc.sources, got, tc.want)
			}
		})
	}

	// anyPlanNeedsClassicGate must fire for a MIXED plan too (DAT-11) —
	// not just classic-only — or the classicGapGate is never built and
	// gateMixedPlan always sees classic.available=false.
	mixedOnly := []stalledCursorPlan{{sources: []string{"aquarius", "sdex"}}}
	if !anyPlanNeedsClassicGate(mixedOnly) {
		t.Fatal("anyPlanNeedsClassicGate must return true for a mixed (soroban+sdex) plan")
	}
	sorobanOnly := []stalledCursorPlan{{sources: []string{"aquarius"}}}
	if anyPlanNeedsClassicGate(sorobanOnly) {
		t.Fatal("anyPlanNeedsClassicGate must return false when no plan has an SDEX portion")
	}
}

// TestGateAgainstDataGaps_HappyPath puts the F-0020 cascade signature
// + a false-positive cursor + an SDEX-only cursor through the gate
// and pins the expected post-gate skip state. The whole point of
// this commit: false positives go from "actionable" to "skip" with
// a self-explanatory reason, real-gap cursors stay actionable.
func TestGateAgainstDataGaps_HappyPath(t *testing.T) {
	gaps := []timescale.LedgerGap{
		{Start: 62642781, End: 62735517, Size: 92737}, // F-0020 gap 1
	}
	plans := []stalledCursorPlan{
		{
			// Soroban cursor whose remaining range DOES overlap a gap.
			cursor:    timescale.Cursor{Sub: "62600000-62700000:aquarius"},
			rangeFrom: 62650000,
			rangeTo:   62700000,
			sources:   []string{"aquarius"},
		},
		{
			// Soroban cursor whose remaining range MISSES any gap
			// (sibling cursors covered it). Pre-gate: actionable;
			// post-gate: skipped as cursor-inventory false positive.
			cursor:    timescale.Cursor{Sub: "15300001-30599999:soroswap"},
			rangeFrom: 15394495,
			rangeTo:   30599999,
			sources:   []string{"soroswap"},
		},
		{
			// SDEX-only cursor — skipped when the SDEX gate is
			// unavailable (classicGapGate zero value).
			cursor:    timescale.Cursor{Sub: "2-15300000:sdex"},
			rangeFrom: 98334,
			rangeTo:   15300000,
			sources:   []string{"sdex"},
		},
		{
			// Pre-existing skip (parser-rejected). Gate must leave alone.
			cursor:     timescale.Cursor{Sub: "garbage"},
			skip:       true,
			skipReason: "doesn't match shape",
		},
	}

	out := gateAgainstDataGaps(plans, gaps, classicGapGate{}, false)

	// out[0] is the actionable real-gap one — gate should keep it un-skipped.
	if out[0].skip {
		t.Errorf("plan[0] aquarius/real-gap was skipped (reason=%q); want actionable", out[0].skipReason)
	}
	if out[0].cursor.Sub != "62600000-62700000:aquarius" {
		t.Errorf("plan[0] sub mismatch: got %q", out[0].cursor.Sub)
	}
	if !out[1].skip {
		t.Errorf("plan[1] soroswap/false-positive should be skipped after gate; got actionable")
	}
	if !strings.Contains(out[1].skipReason, "no soroban_events gap overlap") {
		t.Errorf("plan[1] skip reason should mention false-positive; got %q", out[1].skipReason)
	}
	if !out[2].skip {
		t.Errorf("plan[2] sdex-only should be skipped when the sdex gate is unavailable; got actionable")
	}
	if !strings.Contains(out[2].skipReason, "sdex data-gap gate unavailable") {
		t.Errorf("plan[2] skip reason should mention the unavailable SDEX gate; got %q", out[2].skipReason)
	}
	if !out[3].skip || out[3].skipReason != "doesn't match shape" {
		t.Errorf("plan[3] pre-existing skip should be untouched by the gate; got skip=%v reason=%q", out[3].skip, out[3].skipReason)
	}
}

// TestGateAgainstDataGaps_ForceClassic verifies the --force-classic-cursors
// opt-in: an SDEX-only plan that the default gate would skip MUST
// remain actionable when the flag is set. The flag is the operator's
// escape hatch for "I know the cursor inventory is right; act on it"
// — used sparingly, since the default safer behaviour is don't act
// without data-derived evidence.
func TestGateAgainstDataGaps_ForceClassic(t *testing.T) {
	plans := []stalledCursorPlan{
		{
			cursor:    timescale.Cursor{Sub: "2-15300000:sdex"},
			rangeFrom: 98334,
			rangeTo:   15300000,
			sources:   []string{"sdex"},
		},
	}
	out := gateAgainstDataGaps(plans, nil, classicGapGate{}, true) // forceClassic=true
	if out[0].skip {
		t.Errorf("with --force-classic-cursors the SDEX plan must stay actionable; got skip=%v reason=%q", out[0].skip, out[0].skipReason)
	}
}

// TestGateAgainstDataGaps_MixedPlanChecksBothSides is the DAT-11
// regression: a plan with BOTH a Soroban decoder and an SDEX decoder
// must not be skipped as a cursor-inventory false positive on a clean
// soroban_events check alone — the SDEX side needs its own
// independent confirmation via the same data-derived gate a
// classic-only plan gets.
func TestGateAgainstDataGaps_MixedPlanChecksBothSides(t *testing.T) {
	mixedPlan := func() stalledCursorPlan {
		return stalledCursorPlan{
			cursor:    timescale.Cursor{Sub: "61000000-61600000:aquarius,sdex"},
			rangeFrom: 61_100_000,
			rangeTo:   61_500_000,
			sources:   []string{"aquarius", "sdex"},
		}
	}

	t.Run("soroban side has a real gap — actionable without even checking classic", func(t *testing.T) {
		gaps := []timescale.LedgerGap{{Start: 61_200_000, End: 61_300_000, Size: 100_001}}
		out := gateAgainstDataGaps([]stalledCursorPlan{mixedPlan()}, gaps, classicGapGate{}, false)
		if out[0].skip {
			t.Fatalf("soroban-side real gap must keep the mixed plan actionable; got skip=%v reason=%q", out[0].skip, out[0].skipReason)
		}
	})

	t.Run("soroban clean, classic gate unavailable — must SKIP, not silently act (and must not claim false-positive)", func(t *testing.T) {
		out := gateAgainstDataGaps([]stalledCursorPlan{mixedPlan()}, nil, classicGapGate{}, false)
		if !out[0].skip {
			t.Fatalf("soroban-clean + SDEX-gate-unavailable must SKIP a mixed plan (unverified SDEX side); got actionable")
		}
		if !strings.Contains(out[0].skipReason, "sdex data-gap gate unavailable") {
			t.Fatalf("skip reason must explain the unverified SDEX side, got %q", out[0].skipReason)
		}
		if strings.Contains(out[0].skipReason, "false-positive") {
			t.Fatalf("must NOT be labeled a false-positive when the SDEX side was never checked, got %q", out[0].skipReason)
		}
	})

	t.Run("soroban clean, classic gate shows a real SDEX gap — actionable", func(t *testing.T) {
		gate := classicGapGate{
			available: true,
			floor:     60_000_000,
			gaps:      []timescale.LedgerGap{{Start: 61_150_000, End: 61_250_000, Size: 100_001}},
		}
		out := gateAgainstDataGaps([]stalledCursorPlan{mixedPlan()}, nil, gate, false)
		if out[0].skip {
			t.Fatalf("a real SDEX-side gap must keep the mixed plan actionable; got skip=%v reason=%q", out[0].skip, out[0].skipReason)
		}
	})

	t.Run("BOTH sides independently confirmed clean — genuine false positive, skip", func(t *testing.T) {
		gate := classicGapGate{
			available: true,
			floor:     60_000_000,
			gaps:      nil, // no sdex gap anywhere in the retained window
		}
		out := gateAgainstDataGaps([]stalledCursorPlan{mixedPlan()}, nil, gate, false)
		if !out[0].skip {
			t.Fatalf("both sides clean must skip the mixed plan; got actionable")
		}
	})

	t.Run("force-classic opts the SDEX side out of the gate — actionable on soroban-clean alone", func(t *testing.T) {
		out := gateAgainstDataGaps([]stalledCursorPlan{mixedPlan()}, nil, classicGapGate{}, true)
		if out[0].skip {
			t.Fatalf("--force-classic-cursors must make a soroban-clean mixed plan actionable even with no classic gate; got skip=%v reason=%q", out[0].skip, out[0].skipReason)
		}
	})
}

// TestGateClassicPlan_DataDerived pins the SDEX data-derived gate's
// decision table: with an available classicGapGate, SDEX-only plans
// are gated by (a) the served-tier retention floor and (b) real gap
// overlap in trades[source='sdex'] — instead of the historical
// blanket "not implemented for non-Soroban" skip.
func TestGateClassicPlan_DataDerived(t *testing.T) {
	gate := classicGapGate{
		available: true,
		floor:     61_000_000,
		gaps: []timescale.LedgerGap{
			{Start: 62_000_000, End: 62_100_000, Size: 100_001},
		},
	}
	cases := []struct {
		name       string
		from, to   uint32
		wantSkip   bool
		wantReason string // substring; empty = actionable
	}{
		{
			name: "overlaps a real sdex gap, fully inside retained window",
			from: 61_950_000, to: 62_050_000,
			wantSkip: false,
		},
		{
			name: "inside retained window but no gap overlap — false positive",
			from: 61_100_000, to: 61_500_000,
			wantSkip: true, wantReason: "no sdex data gap",
		},
		{
			name: "entirely below the retention floor",
			from: 50_000_000, to: 60_000_000,
			wantSkip: true, wantReason: "below the served-tier trades retention floor",
		},
		{
			name: "straddles the floor and overlaps a gap — needs review",
			from: 60_000_000, to: 62_050_000,
			wantSkip: true, wantReason: "starts below the retention floor",
		},
		{
			name: "straddles the floor, no gap above it — false-positive reason wins",
			from: 60_000_000, to: 61_500_000,
			wantSkip: true, wantReason: "no sdex data gap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := stalledCursorPlan{
				cursor:    timescale.Cursor{Sub: "x:sdex"},
				rangeFrom: tc.from,
				rangeTo:   tc.to,
				sources:   []string{"sdex"},
			}
			gateClassicPlan(&p, gate, false)
			if p.skip != tc.wantSkip {
				t.Fatalf("skip=%v want %v (reason=%q)", p.skip, tc.wantSkip, p.skipReason)
			}
			if tc.wantSkip && !strings.Contains(p.skipReason, tc.wantReason) {
				t.Errorf("skip reason %q missing %q", p.skipReason, tc.wantReason)
			}
		})
	}
}

// TestSdexGapTarget guards the registry coupling: the classic gate
// reuses the gap detector's sdex/trades target definition, so if
// that target is ever renamed or removed the gate silently degrades
// to the blanket skip — this test makes that a loud failure instead.
func TestSdexGapTarget(t *testing.T) {
	target, ok := sdexGapTarget()
	if !ok {
		t.Fatal("sdex/trades target missing from timescale.DefaultGapDetectorTargets")
	}
	if target.WhereFilter == "" {
		t.Error("sdex target must carry a WhereFilter (trades holds many sources)")
	}
	if target.LedgerColumn == "" {
		t.Error("sdex target must declare its ledger column")
	}
}

// TestPlanResumeStalled_FilterSemantics verifies the cursor-list →
// plan-list filter chain without touching Postgres: it operates on a
// pre-built slice that mimics what ListCursors would return, and
// calls the REAL matchesSourceFilter helper planResumeStalled uses
// (not a re-implementation) so this test can't drift from the actual
// filter behaviour. (The real planResumeStalled wraps
// store.ListCursors; this test exercises the post-list filter logic
// via a parallel helper to avoid the testcontainers cost for what is
// pure CPU work.)
//
// Filter precedence: source-prefix → min-lag → source-filter (decoder
// portion only — see matchesSourceFilter / AGT-08). -max-resumes is
// NOT part of this chain any more: it is applied by applyMaxResumesCap
// AFTER the data-gap gate, so it is covered by
// TestApplyMaxResumesCap_AppliesAfterGate instead.
func TestPlanResumeStalled_FilterSemantics(t *testing.T) {
	now := time.Now().UTC()
	rows := []timescale.Cursor{
		// 0: backfill, stale 30 min — filtered by min-lag=1h
		{Source: "backfill", Sub: "100-200:sdex", LastLedger: 150, UpdatedAt: now.Add(-30 * time.Minute)},
		// 1: backfill, stale 2 h — included
		{Source: "backfill", Sub: "300-400:sdex,aquarius", LastLedger: 350, UpdatedAt: now.Add(-2 * time.Hour)},
		// 2: backfill, stale 5 h — included (defindex substring will catch this)
		{Source: "backfill", Sub: "500-600:defindex,soroswap-router", LastLedger: 550, UpdatedAt: now.Add(-5 * time.Hour)},
		// 3: NOT backfill — filtered by source-prefix
		{Source: "ledgerstream", Sub: "", LastLedger: 62000000, UpdatedAt: now.Add(-5 * time.Hour)},
		// 4: backfill, stale 3 h, defindex substring
		{Source: "backfill", Sub: "700-800:defindex", LastLedger: 750, UpdatedAt: now.Add(-3 * time.Hour)},
		// 5: backfill, stale 6 h, "800" only appears in the ledger-range
		// prefix (900-800800 is contrived but the point stands: a
		// sourceFilter of "800" must NOT match on digits in the range).
		{Source: "backfill", Sub: "900000800-901000000:aquarius", LastLedger: 900000900, UpdatedAt: now.Add(-6 * time.Hour)},
	}

	cases := []struct {
		name         string
		minLag       time.Duration
		sourceFilter string
		wantSubs     []string
	}{
		{
			name:   "all backfill stalls over 1h",
			minLag: time.Hour,
			wantSubs: []string{
				"300-400:sdex,aquarius", "500-600:defindex,soroswap-router",
				"700-800:defindex", "900000800-901000000:aquarius",
			},
		},
		{
			name:         "filter to defindex substring",
			minLag:       time.Hour,
			sourceFilter: "defindex",
			wantSubs:     []string{"500-600:defindex,soroswap-router", "700-800:defindex"},
		},
		{
			name:         "filter digits that only appear in the ledger range must NOT match (AGT-08)",
			minLag:       time.Hour,
			sourceFilter: "800",
			wantSubs:     nil,
		},
		{
			name:     "raised min-lag prunes more",
			minLag:   4 * time.Hour,
			wantSubs: []string{"500-600:defindex,soroswap-router", "900000800-901000000:aquarius"},
		},
		{
			name:     "min-lag above any stall — empty",
			minLag:   24 * time.Hour,
			wantSubs: nil,
		},
	}

	filter := func(rows []timescale.Cursor, minLag time.Duration, src string) []string {
		var out []string
		for _, c := range rows {
			if len(c.Source) < len("backfill") || c.Source[:8] != "backfill" {
				continue
			}
			if now.Sub(c.UpdatedAt) < minLag {
				continue
			}
			if !matchesSourceFilter(c.Sub, src) {
				continue
			}
			out = append(out, c.Sub)
		}
		return out
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filter(rows, tc.minLag, tc.sourceFilter)
			if !reflect.DeepEqual(got, tc.wantSubs) {
				t.Errorf("subs = %v, want %v", got, tc.wantSubs)
			}
		})
	}
}

// TestMatchesSourceFilter_DecoderPortionOnly is the AGT-08 regression:
// the filter must match only the decoder-CSV portion of sub_source,
// never the numeric <from>-<to> range prefix.
func TestMatchesSourceFilter_DecoderPortionOnly(t *testing.T) {
	cases := []struct {
		name   string
		sub    string
		filter string
		want   bool
	}{
		{"empty filter matches everything", "100-200:sdex", "", true},
		{"decoder substring matches", "62200000-62210000:aquarius,band,sdex", "band", true},
		{"digits present ONLY in the ledger range must not match", "900000800-901000000:aquarius", "800", false},
		{"digits present in the decoder portion DO match", "100-200:reflector-cex", "cex", true},
		{"no colon — degrades to whole-string match", "not-a-range-800", "800", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesSourceFilter(tc.sub, tc.filter); got != tc.want {
				t.Errorf("matchesSourceFilter(%q, %q) = %v, want %v", tc.sub, tc.filter, got, tc.want)
			}
		})
	}
}

// TestApplyMaxResumesCap_AppliesAfterGate is the AGT-08 regression for
// the OTHER half of the finding: the cap must count only genuinely
// actionable (non-skip) plans, and any actionable plan past the cap
// is marked skip (not silently dropped or, worse, never gathered).
func TestApplyMaxResumesCap_AppliesAfterGate(t *testing.T) {
	plans := []stalledCursorPlan{
		{cursor: timescale.Cursor{Sub: "a"}, skip: true, skipReason: "gated out"},
		{cursor: timescale.Cursor{Sub: "b"}}, // actionable #1
		{cursor: timescale.Cursor{Sub: "c"}}, // actionable #2
		{cursor: timescale.Cursor{Sub: "d"}}, // actionable #3 — should be capped
		{cursor: timescale.Cursor{Sub: "e"}, skip: true, skipReason: "gated out"},
	}
	got := applyMaxResumesCap(plans, 2)

	var actionable []string
	for _, p := range got {
		if !p.skip {
			actionable = append(actionable, p.cursor.Sub)
		}
	}
	if want := []string{"b", "c"}; !reflect.DeepEqual(actionable, want) {
		t.Fatalf("actionable after cap = %v, want %v", actionable, want)
	}
	// "d" was genuinely actionable (not gate-skipped) but past the cap
	// — it must be marked skip WITH a distinct reason, not silently
	// vanish and not be conflated with a real gate-skip.
	if !got[3].skip || got[3].skipReason == "" || got[3].skipReason == "gated out" {
		t.Fatalf("plan 'd' (past cap) = skip=%v reason=%q, want skip=true with a cap-specific reason", got[3].skip, got[3].skipReason)
	}
	// Already-gate-skipped plans are untouched and don't count toward
	// the cap.
	if !got[0].skip || got[0].skipReason != "gated out" {
		t.Fatalf("plan 'a' must remain untouched, got skip=%v reason=%q", got[0].skip, got[0].skipReason)
	}
}

// TestApplyMaxResumesCap_ZeroMeansNoCap: -max-resumes 0 (the default)
// must not skip anything.
func TestApplyMaxResumesCap_ZeroMeansNoCap(t *testing.T) {
	plans := []stalledCursorPlan{
		{cursor: timescale.Cursor{Sub: "a"}},
		{cursor: timescale.Cursor{Sub: "b"}},
		{cursor: timescale.Cursor{Sub: "c"}},
	}
	got := applyMaxResumesCap(plans, 0)
	for i, p := range got {
		if p.skip {
			t.Fatalf("plan %d unexpectedly skipped with cap=0: %+v", i, p)
		}
	}
}

// contains is a substring check for test assertions. Duplicated from
// internal/ops/diagnostics' hubble_check_test.go (same package-local
// test-helper pattern) rather than shared — both are trivial,
// test-only, and pre-date the cmd/stellarindex-ops -> internal/ops/*
// package split (maintainability audit 2026-07-01, D1 finding M1-5).
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
