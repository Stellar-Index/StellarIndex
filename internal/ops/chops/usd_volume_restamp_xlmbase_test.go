// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"math/big"
	"strings"
	"testing"
)

// TestCheckRestampLiveOverlap is the one-writer contract for a `trades`
// re-stamp. `usd_volume` has exactly one live writer — the indexer's
// ingest path at the ledgerstream cursor — and a restamp is only
// legitimate BEHIND it: overlapping lets the restamp's high
// derive_generation beat the ingest path's own value for a row the tail
// is still writing (or the reverse), so one of the two writers is
// silently discarded on a money column.
func TestCheckRestampLiveOverlap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		haveLive  bool
		live, top uint32
		allow     bool
		wantErr   string
	}{
		{name: "tail well past the window", haveLive: true, live: 64_200_000, top: 61_000_000},
		{name: "tail exactly at the window top", haveLive: true, live: 61_000_000, top: 61_000_000},
		{
			name: "tail one ledger short", haveLive: true, live: 60_999_999, top: 61_000_000,
			wantErr: "live ledgerstream cursor is 60999999",
		},
		{
			name: "indexer resyncing far behind", haveLive: true, live: 1_000, top: 61_000_000,
			wantErr: "below the top of the requested window",
		},
		{
			name: "no cursor at all is a refusal", haveLive: false, top: 61_000_000,
			wantErr: "the indexer has never run",
		},
		{name: "explicit override", haveLive: true, live: 1_000, top: 61_000_000, allow: true},
		{name: "window holds no on-chain rows", haveLive: false, top: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkRestampLiveOverlap(tc.haveLive, tc.live, tc.top, tc.allow)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("err = nil, want a refusal containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "-allow-live-overlap") {
				t.Errorf("the refusal must name its override; got %v", err)
			}
		})
	}
}

// TestValidateRestampTierFlags: an unknown -tier is rejected, and an
// xlm-base-only flag passed with -tier exact is an ERROR rather than a
// silent no-op. An operator who typed -report and got a write run, or
// who typed -min-rel-delta and got every row rewritten, has been
// actively misled by the tool on a money column.
func TestValidateRestampTierFlags(t *testing.T) {
	t.Parallel()
	if err := validateRestampTierFlags(restampTierExact, nil); err != nil {
		t.Fatalf("bare -tier exact: %v", err)
	}
	if err := validateRestampTierFlags(restampTierXLMBase, map[string]bool{"report": true, "batch": true}); err != nil {
		t.Fatalf("-tier xlm-base with its own flags: %v", err)
	}
	err := validateRestampTierFlags("estimated", nil)
	if err == nil || !strings.Contains(err.Error(), `want "exact" or "xlm-base"`) {
		t.Fatalf("unknown tier: err = %v", err)
	}
	for _, f := range restampXLMBaseOnlyFlags {
		err := validateRestampTierFlags(restampTierExact, map[string]bool{f: true})
		if err == nil || !strings.Contains(err.Error(), "-"+f) {
			t.Errorf("-tier exact with -%s: err = %v, want a refusal naming the flag", f, err)
		}
	}
}

// TestParseMinRelDelta: the write filter is a fraction, and "no filter"
// is the default — the anchor's number is a re-derivation of the correct
// value, so leaving a row 0.5% wrong for tidiness is not the default
// posture on a money column.
func TestParseMinRelDelta(t *testing.T) {
	t.Parallel()
	for _, empty := range []string{"", "  ", "0", "0.0"} {
		got, err := parseMinRelDelta(empty)
		if err != nil || got != nil {
			t.Errorf("parseMinRelDelta(%q) = %v, %v; want nil, nil", empty, got, err)
		}
	}
	got, err := parseMinRelDelta("0.01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(big.NewRat(1, 100)) != 0 {
		t.Errorf("parseMinRelDelta(0.01) = %s, want 1/100", got.RatString())
	}
	if _, err := parseMinRelDelta("-0.01"); err == nil {
		t.Error("a negative -min-rel-delta must be refused")
	}
	if _, err := parseMinRelDelta("one percent"); err == nil {
		t.Error("a non-numeric -min-rel-delta must be refused")
	}
}

// TestRatPercent renders the report's percentages without float math
// (ADR-0003 keeps money-adjacent arithmetic out of float space).
func TestRatPercent(t *testing.T) {
	t.Parallel()
	if got := ratPercent(nil); got != "0%" {
		t.Errorf("ratPercent(nil) = %q, want 0%%", got)
	}
	if got := ratPercent(big.NewRat(1, 100)); got != "1.0000%" {
		t.Errorf("ratPercent(1/100) = %q, want 1.0000%%", got)
	}
	if got := ratPercent(big.NewRat(1, 3)); got != "33.3333%" {
		t.Errorf("ratPercent(1/3) = %q, want 33.3333%%", got)
	}
}
