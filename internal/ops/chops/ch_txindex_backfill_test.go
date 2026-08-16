// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"
)

// TestParseTxIndexBackfillFlags_RefusesBareFullHistory pins the W8.15 safe
// default: a BARE `ch-txindex-backfill` (no bounds, no -full) must NOT resolve
// into the implicit ledger-2..tip (~10.2B row) backfill. Before the guard the
// defaults (-from 2, -to 0=tip) meant an argument-less invocation silently
// kicked off the entire history — a heavy job the runbook says must be
// babysat. The full run is still available, but only with an explicit word.
func TestParseTxIndexBackfillFlags_RefusesBareFullHistory(t *testing.T) {
	_, err := parseTxIndexBackfillFlags(nil)
	if err == nil {
		t.Fatal("bare invocation (no -from/-to/-full) was accepted — it must refuse the " +
			"implicit full-history backfill")
	}
	if !strings.Contains(err.Error(), "full-history") {
		t.Fatalf("refusal error should explain the full-history footgun, got: %v", err)
	}
}

// TestParseTxIndexBackfillFlags_ExplicitOptInsAreAccepted pins that each
// intentional path still works and resolves to the expected plan.
func TestParseTxIndexBackfillFlags_ExplicitOptInsAreAccepted(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantFrom uint32
		wantTo   uint32
	}{
		{"full opts into 2..tip", []string{"-full"}, 2, 0},
		{"explicit from is a resume point", []string{"-from", "80000000"}, 80_000_000, 0},
		{"explicit to bounds the range", []string{"-to", "1000000"}, 2, 1_000_000},
		{"explicit from+to", []string{"-from", "500000", "-to", "1000000"}, 500_000, 1_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := parseTxIndexBackfillFlags(tc.args)
			if err != nil {
				t.Fatalf("parse(%v) errored: %v", tc.args, err)
			}
			if plan.from != tc.wantFrom {
				t.Errorf("from = %d, want %d", plan.from, tc.wantFrom)
			}
			if plan.to != tc.wantTo {
				t.Errorf("to = %d, want %d", plan.to, tc.wantTo)
			}
		})
	}
}

// TestParseTxIndexBackfillFlags_ZeroFromStillRejected keeps the pre-existing
// invariant: -from 0 and -window 0 are invalid regardless of the new guard.
func TestParseTxIndexBackfillFlags_ZeroFromStillRejected(t *testing.T) {
	if _, err := parseTxIndexBackfillFlags([]string{"-from", "0"}); err == nil {
		t.Error("-from 0 must be rejected")
	}
	if _, err := parseTxIndexBackfillFlags([]string{"-full", "-window", "0"}); err == nil {
		t.Error("-window 0 must be rejected")
	}
}
