// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
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

// ─── resolveVerifyTo: fail-closed against an independent tip (GH-1180) ────

func TestResolveVerifyTo(t *testing.T) {
	cases := []struct {
		name               string
		chMax, independent uint32
		wantTo             uint32
		wantErr            bool
	}{
		{"chMax at or ahead of independent tip passes through", 63_100_000, 63_099_000, 63_100_000, false},
		{"within tolerance is expected lag, not truncation", 63_100_000, 63_100_050, 63_100_000, false},
		{"three partitions short of the independent tip fails closed", 60_000_000, 63_000_000, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveVerifyTo("verify-lake", tc.chMax, tc.independent)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveVerifyTo(%d,%d) = %d, <nil>, want an error (truncated lake must fail closed)", tc.chMax, tc.independent, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveVerifyTo(%d,%d) unexpected error: %v", tc.chMax, tc.independent, err)
			}
			if got != tc.wantTo {
				t.Fatalf("resolveVerifyTo(%d,%d) = %d, want %d", tc.chMax, tc.independent, got, tc.wantTo)
			}
		})
	}
}

// ─── lakeCheckLine / lakeChecksRunLabel: SKIPPED vs. zero (GH-1195) ───────

func TestLakeCheckLineDistinguishesSkippedFromZero(t *testing.T) {
	skipped := lakeCheckLine("hash_chain", false, "0 broken link(s)")
	if !strings.Contains(skipped, "SKIPPED") {
		t.Fatalf("lakeCheckLine(ran=false) = %q, want it to say SKIPPED, not the zero-valued detail", skipped)
	}
	ran := lakeCheckLine("hash_chain", true, "0 broken link(s)")
	if strings.Contains(ran, "SKIPPED") || !strings.Contains(ran, "0 broken link(s)") {
		t.Fatalf("lakeCheckLine(ran=true) = %q, want the detail, not SKIPPED", ran)
	}
}

func TestLakeChecksRunLabel(t *testing.T) {
	if got := lakeChecksRunLabel(true, false, true); got != "contiguity,hashchain" {
		t.Fatalf("lakeChecksRunLabel(contiguity,hashchain) = %q", got)
	}
	if got := lakeChecksRunLabel(false, false, false); got != "none" {
		t.Fatalf("lakeChecksRunLabel(none) = %q, want %q", got, "none")
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
