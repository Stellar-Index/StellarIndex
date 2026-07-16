// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func TestSaturatingSub(t *testing.T) {
	cases := []struct {
		name string
		a, b uint64
		want uint64
	}{
		{"normal", 10, 3, 7},
		{"equal", 5, 5, 0},
		// b>a: a raw uint64 subtraction would wrap to ~1.8e19 instead of
		// saturating to 0 — the exact regression class the
		// verify-contiguity review caught (CLAUDE.md ADR-0034 invariant:
		// never truncate/wrap a displayed count silently).
		{"b-greater-than-a", 3, 10, 0},
		{"both-zero", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := saturatingSub(tc.a, tc.b); got != tc.want {
				t.Fatalf("saturatingSub(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestBoundarySeqsToCheck(t *testing.T) {
	windows := []clickhouse.HashChainWindowResult{
		{From: 2, To: 999_999},
		{From: 1_000_000, To: 1_999_999},
		{From: 2_000_000, To: 2_999_999},
	}

	got := boundarySeqsToCheck(windows, 2)
	want := []uint32{1_000_000, 2_000_000}
	if len(got) != len(want) {
		t.Fatalf("boundarySeqsToCheck() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("boundarySeqsToCheck()[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestBoundarySeqsToCheck_SingleWindow(t *testing.T) {
	windows := []clickhouse.HashChainWindowResult{{From: 2, To: 999_999}}
	if got := boundarySeqsToCheck(windows, 2); len(got) != 0 {
		t.Fatalf("boundarySeqsToCheck() = %v, want empty (only window is -from itself)", got)
	}
}

func TestBoundaryTag(t *testing.T) {
	cases := []struct {
		name string
		b    clickhouse.HashChainBoundaryResult
		want string
	}{
		{
			name: "linked",
			b:    clickhouse.HashChainBoundaryResult{Linked: true, SeqPresent: true, PredecessorPresent: true},
			want: "OK",
		},
		{
			name: "both-absent",
			b:    clickhouse.HashChainBoundaryResult{SeqPresent: false, PredecessorPresent: false},
			want: "BROKEN (both ledgers absent — a substrate gap; run verify-contiguity)",
		},
		{
			name: "seq-absent",
			b:    clickhouse.HashChainBoundaryResult{SeqPresent: false, PredecessorPresent: true},
			want: "BROKEN (ledger_seq absent — a substrate gap; run verify-contiguity)",
		},
		{
			name: "predecessor-absent",
			b:    clickhouse.HashChainBoundaryResult{SeqPresent: true, PredecessorPresent: false},
			want: "BROKEN (predecessor absent — a substrate gap; run verify-contiguity)",
		},
		{
			name: "hash-mismatch",
			b:    clickhouse.HashChainBoundaryResult{SeqPresent: true, PredecessorPresent: true, Linked: false},
			want: "BROKEN (both present, hash mismatch)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := boundaryTag(tc.b); got != tc.want {
				t.Fatalf("boundaryTag() = %q, want %q", got, tc.want)
			}
		})
	}
}
