// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"
)

// ─── checkFieldValue / contiguityChecksRunLabel: SKIPPED vs. zero (GH-1195) ─

func TestCheckFieldValueDistinguishesSkippedFromZero(t *testing.T) {
	if got := checkFieldValue(false, 0); got != "SKIPPED" {
		t.Fatalf("checkFieldValue(ran=false, 0) = %q, want SKIPPED", got)
	}
	if got := checkFieldValue(true, 0); got != "0" {
		t.Fatalf("checkFieldValue(ran=true, 0) = %q, want %q (ran-and-clean must not look SKIPPED)", got, "0")
	}
}

func TestContiguityChecksRunLabel(t *testing.T) {
	if got := contiguityChecksRunLabel(true, false); got != "ledgers" {
		t.Fatalf("contiguityChecksRunLabel(ledgers only) = %q", got)
	}
	if got := contiguityChecksRunLabel(false, false); got != "none" {
		t.Fatalf("contiguityChecksRunLabel(none) = %q, want %q", got, "none")
	}
}

// ─── groupMissingIntoRanges: gap-range computation, no live CH ────────────

func TestGroupMissingIntoRanges(t *testing.T) {
	cases := []struct {
		name       string
		missing    []uint32
		cap        int
		wantRanges []gapRange
		wantTrunc  bool
	}{
		{"empty", nil, 200, nil, false},
		{"single", []uint32{5}, 200, []gapRange{{5, 5}}, false},
		{"one-contiguous-run", []uint32{5, 6, 7, 8}, 200, []gapRange{{5, 8}}, false},
		{
			"two-separate-runs",
			[]uint32{5, 6, 10, 11, 12},
			200,
			[]gapRange{{5, 6}, {10, 12}},
			false,
		},
		{
			"three-singletons",
			[]uint32{1, 3, 5},
			200,
			[]gapRange{{1, 1}, {3, 3}, {5, 5}},
			false,
		},
		{
			"cap-exactly-matches-range-count",
			[]uint32{1, 2, 10, 20},
			4, // 4 singleton-ish ranges: [1,2],[10,10],[20,20] = 3 ranges, under cap
			[]gapRange{{1, 2}, {10, 10}, {20, 20}},
			false,
		},
		{
			"cap-truncates",
			[]uint32{1, 2, 3, 10, 11, 20, 30, 40},
			2, // only room for 2 ranges: [1,3] and [10,11]; [20,20],[30,30],[40,40] dropped
			[]gapRange{{1, 3}, {10, 11}},
			true,
		},
		{
			"cap-zero-truncates-immediately",
			[]uint32{1, 2, 3},
			0,
			nil,
			true,
		},
		{
			"boundary-adjacent-values-merge",
			[]uint32{100, 101, 102, 103},
			200,
			[]gapRange{{100, 103}},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRanges, gotTrunc := groupMissingIntoRanges(tc.missing, tc.cap)
			if gotTrunc != tc.wantTrunc {
				t.Fatalf("groupMissingIntoRanges(%v, %d) truncated = %v, want %v", tc.missing, tc.cap, gotTrunc, tc.wantTrunc)
			}
			if len(gotRanges) != len(tc.wantRanges) {
				t.Fatalf("groupMissingIntoRanges(%v, %d) = %v, want %v", tc.missing, tc.cap, gotRanges, tc.wantRanges)
			}
			for i := range gotRanges {
				if gotRanges[i] != tc.wantRanges[i] {
					t.Fatalf("groupMissingIntoRanges(%v, %d)[%d] = %v, want %v", tc.missing, tc.cap, i, gotRanges[i], tc.wantRanges[i])
				}
			}
		})
	}
}

func TestGapRange_Count(t *testing.T) {
	cases := []struct {
		name string
		g    gapRange
		want uint64
	}{
		{"single-ledger", gapRange{From: 5, To: 5}, 1},
		{"four-ledgers", gapRange{From: 5, To: 8}, 4},
		{"large-range", gapRange{From: 1000, To: 1_999_999}, 1_999_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.g.Count(); got != tc.want {
				t.Fatalf("gapRange%v.Count() = %d, want %d", tc.g, got, tc.want)
			}
		})
	}
}

// ─── ecFloorSegments: floor-gating logic, no live CH ──────────────────────

func TestECFloorSegments(t *testing.T) {
	cases := []struct {
		name                                                       string
		from, to, ecFloor                                          uint32
		wantPendingFrom, wantPendingTo, wantGatedFrom, wantGatedTo uint32
		wantHasPending, wantHasGated                               bool
	}{
		{
			// The motivating scenario: range straddles the
			// known live-ingest floor.
			name: "floor-strictly-inside-range",
			from: 62_000_000, to: 64_000_000, ecFloor: 63_050_000,
			wantPendingFrom: 62_000_000, wantPendingTo: 63_049_999, wantHasPending: true,
			wantGatedFrom: 63_050_000, wantGatedTo: 64_000_000, wantHasGated: true,
		},
		{
			// -ec-floor at or below -from: everything requested is gated,
			// no pending segment.
			name: "floor-at-from",
			from: 63_050_000, to: 64_000_000, ecFloor: 63_050_000,
			wantHasPending: false,
			wantGatedFrom:  63_050_000, wantGatedTo: 64_000_000, wantHasGated: true,
		},
		{
			name: "floor-below-from",
			from: 63_050_000, to: 64_000_000, ecFloor: 10,
			wantHasPending: false,
			wantGatedFrom:  63_050_000, wantGatedTo: 64_000_000, wantHasGated: true,
		},
		{
			// -ec-floor beyond -to: everything requested is pending, no
			// gated segment.
			name: "floor-beyond-to",
			from: 2, to: 1_000_000, ecFloor: 63_050_000,
			wantPendingFrom: 2, wantPendingTo: 1_000_000, wantHasPending: true,
			wantHasGated: false,
		},
		{
			// -ec-floor exactly one past -to: entire range pending.
			name: "floor-one-past-to",
			from: 2, to: 999_999, ecFloor: 1_000_000,
			wantPendingFrom: 2, wantPendingTo: 999_999, wantHasPending: true,
			wantHasGated: false,
		},
		{
			// Single-ledger range, exactly at the floor: fully gated.
			name: "single-ledger-at-floor",
			from: 63_050_000, to: 63_050_000, ecFloor: 63_050_000,
			wantHasPending: false,
			wantGatedFrom:  63_050_000, wantGatedTo: 63_050_000, wantHasGated: true,
		},
		{
			// Single-ledger range, one below the floor: fully pending.
			name: "single-ledger-below-floor",
			from: 63_049_999, to: 63_049_999, ecFloor: 63_050_000,
			wantPendingFrom: 63_049_999, wantPendingTo: 63_049_999, wantHasPending: true,
			wantHasGated: false,
		},
		{
			// ecFloor == 0: never underflows (regression guard — ecFloor-1
			// must not wrap uint32 when ecFloor could be as low as 1).
			name: "floor-zero",
			from: 0, to: 100, ecFloor: 0,
			wantHasPending: false,
			wantGatedFrom:  0, wantGatedTo: 100, wantHasGated: true,
		},
		{
			name: "floor-one",
			from: 0, to: 100, ecFloor: 1,
			wantPendingFrom: 0, wantPendingTo: 0, wantHasPending: true,
			wantGatedFrom: 1, wantGatedTo: 100, wantHasGated: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf, pt, hasP, gf, gt, hasG := ecFloorSegments(tc.from, tc.to, tc.ecFloor)
			if hasP != tc.wantHasPending {
				t.Fatalf("ecFloorSegments(%d,%d,%d) hasPending = %v, want %v", tc.from, tc.to, tc.ecFloor, hasP, tc.wantHasPending)
			}
			if hasP && (pf != tc.wantPendingFrom || pt != tc.wantPendingTo) {
				t.Fatalf("ecFloorSegments(%d,%d,%d) pending = [%d,%d], want [%d,%d]",
					tc.from, tc.to, tc.ecFloor, pf, pt, tc.wantPendingFrom, tc.wantPendingTo)
			}
			if hasG != tc.wantHasGated {
				t.Fatalf("ecFloorSegments(%d,%d,%d) hasGated = %v, want %v", tc.from, tc.to, tc.ecFloor, hasG, tc.wantHasGated)
			}
			if hasG && (gf != tc.wantGatedFrom || gt != tc.wantGatedTo) {
				t.Fatalf("ecFloorSegments(%d,%d,%d) gated = [%d,%d], want [%d,%d]",
					tc.from, tc.to, tc.ecFloor, gf, gt, tc.wantGatedFrom, tc.wantGatedTo)
			}
		})
	}
}

// ─── formatCoverageLine: report-line rendering, no live CH ────────────────

func TestFormatCoverageLine(t *testing.T) {
	got := formatCoverageLine(100, 199, 100, 97, "GAP")
	want := "  [100,199]  expected=100 present=97 missing=3  GAP"
	if got != want {
		t.Fatalf("formatCoverageLine = %q, want %q", got, want)
	}
}

func TestFormatCoverageLine_NoDeficit(t *testing.T) {
	got := formatCoverageLine(100, 199, 100, 100, "OK")
	want := "  [100,199]  expected=100 present=100 missing=0  OK"
	if got != want {
		t.Fatalf("formatCoverageLine = %q, want %q", got, want)
	}
}

// TestFormatCoverageLine_PresentExceedsExpected guards the saturating
// subtraction: for Check 2, present (distinct entry_change ledgers) can exceed
// expected (tx-bearing ledgers) on protocol-upgrade ledgers. A raw
// expected-present would wrap uint64 to ~1.8e19 in the printed line.
func TestFormatCoverageLine_PresentExceedsExpected(t *testing.T) {
	got := formatCoverageLine(63_000_000, 63_999_999, 900, 901, "OK")
	want := "  [63000000,63999999]  expected=900 present=901 missing=0  OK"
	if got != want {
		t.Fatalf("formatCoverageLine = %q, want %q", got, want)
	}
}

// ─── toLedgerSeq: uint64 flag → uint32 ledger_seq, never silently wraps ───

func TestToLedgerSeq(t *testing.T) {
	got, err := toLedgerSeq("-from", 12345)
	if err != nil || got != 12345 {
		t.Fatalf("toLedgerSeq(12345) = (%d, %v), want (12345, nil)", got, err)
	}

	if _, err := toLedgerSeq("-to", uint64(1)<<40); err == nil {
		t.Fatalf("toLedgerSeq(2^40) should have errored on overflow")
	}
}

// ─── auto -ec-floor + exemption reporting (GH-1092) ───────────────────────

func TestAutoECFloor(t *testing.T) {
	cases := []struct {
		name       string
		edge       uint32
		found      bool
		from, want uint32
	}{
		{"edge-inside-range-is-the-floor", 38_000_001, true, 2, 38_000_001},
		{"edge-at-from-gates-everything", 2, true, 2, 2},
		{"edge-below-from-clamps", 5, true, 10, 10},
		{"no-coverage-in-range-fails-closed", 0, false, 1_000, 1_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoECFloor(tc.edge, tc.found, tc.from); got != tc.want {
				t.Fatalf("autoECFloor(%d,%v,%d) = %d, want %d", tc.edge, tc.found, tc.from, got, tc.want)
			}
		})
	}
}

func TestECFloorExemptionLine(t *testing.T) {
	got := ecFloorExemptionLine(63_050_000, 2, "operator-supplied -ec-floor")
	want := "ec-floor=63050000 (operator-supplied -ec-floor): EXEMPTING [2,63049999] from the hard gate (reported as backfill-pending, not counted)"
	if got != want {
		t.Fatalf("ecFloorExemptionLine = %q, want %q", got, want)
	}
	got = ecFloorExemptionLine(2, 2, "auto")
	want = "ec-floor=2 (auto): nothing exempted, every ledger from 2 is hard-gated"
	if got != want {
		t.Fatalf("ecFloorExemptionLine = %q, want %q", got, want)
	}
}

func TestECFloorFlagLabel(t *testing.T) {
	if got := ecFloorFlagLabel(0); got != "auto" {
		t.Fatalf("ecFloorFlagLabel(0) = %q, want auto", got)
	}
	if got := ecFloorFlagLabel(7); got != "7" {
		t.Fatalf("ecFloorFlagLabel(7) = %q, want 7", got)
	}
}

// ─── Check 1 verdict must not claim "exactly once" over duplicates (GH-1091) ─

func TestLedgerCheckOKLine(t *testing.T) {
	if got := ledgerCheckOKLine(0); got != "check 1: OK — every ledger present exactly once" {
		t.Fatalf("ledgerCheckOKLine(0) = %q", got)
	}
	got := ledgerCheckOKLine(3)
	if strings.Contains(got, "exactly once") || !strings.Contains(got, "3 un-merged duplicate row(s)") {
		t.Fatalf("ledgerCheckOKLine(3) = %q, must name 3 duplicate rows and not claim exactly once", got)
	}
}
