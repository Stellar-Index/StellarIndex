// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"testing"
)

// ─── lakeExitCode: capped exit-code aggregation, no live CH ───────────────

func TestLakeExitCode(t *testing.T) {
	cases := []struct {
		name                     string
		gaps, deficiency, broken uint64
		want                     int
	}{
		{"all-zero", 0, 0, 0, 0},
		{"gaps-only", 3, 0, 0, 3},
		{"deficiency-only", 0, 5, 0, 5},
		{"broken-only", 0, 0, 2, 2},
		{"sums-all-three", 10, 20, 30, 60},
		{"exactly-255-not-capped", 100, 100, 55, 255},
		{"just-over-cap", 100, 100, 56, 255},
		{"way-over-cap", 1_000_000, 0, 0, 255},
		{"sum-overflow-guard-still-caps", 200, 200, 200, 255},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lakeExitCode(tc.gaps, tc.deficiency, tc.broken); got != tc.want {
				t.Fatalf("lakeExitCode(%d,%d,%d) = %d, want %d", tc.gaps, tc.deficiency, tc.broken, got, tc.want)
			}
		})
	}
}

// ─── parseLakeChecks: -checks flag parsing, no live CH ────────────────────

func TestParseLakeChecks(t *testing.T) {
	cases := []struct {
		name                                            string
		raw                                             string
		wantContiguity, wantEntryChanges, wantHashChain bool
		wantErr                                         bool
	}{
		{"default-all-three", "contiguity,entrychanges,hashchain", true, true, true, false},
		{"single-contiguity", "contiguity", true, false, false, false},
		{"single-entrychanges", "entrychanges", false, true, false, false},
		{"single-hashchain", "hashchain", false, false, true, false},
		{"two-of-three", "contiguity,hashchain", true, false, true, false},
		{"whitespace-tolerant", " contiguity , hashchain ", true, false, true, false},
		{"unknown-token-errors", "contiguity,bogus", false, false, false, true},
		{"empty-string-errors", "", false, false, false, true},
		{"only-commas-errors", ",,", false, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotC, gotE, gotH, err := parseLakeChecks(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseLakeChecks(%q) = nil error, want error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLakeChecks(%q) unexpected error: %v", tc.raw, err)
			}
			if gotC != tc.wantContiguity || gotE != tc.wantEntryChanges || gotH != tc.wantHashChain {
				t.Fatalf("parseLakeChecks(%q) = (%v,%v,%v), want (%v,%v,%v)",
					tc.raw, gotC, gotE, gotH, tc.wantContiguity, tc.wantEntryChanges, tc.wantHashChain)
			}
		})
	}
}
