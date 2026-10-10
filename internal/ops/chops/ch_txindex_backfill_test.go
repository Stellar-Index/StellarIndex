// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"
)

// caseParseTxIndexBackfillFlags_RefusesBareFullHistory pins the safe
// default: a BARE `ch-txindex-backfill` (no bounds, no -full) must NOT resolve
// into the implicit ledger-2..tip (~10.2B row) backfill. Before the guard the
// defaults (-from 2, -to 0=tip) meant an argument-less invocation silently
// kicked off the entire history — a heavy job the runbook says must be
// babysat. The full run is still available, but only with an explicit word.
func caseParseTxIndexBackfillFlags_RefusesBareFullHistory(t *testing.T) {
	_, err := parseTxIndexBackfillFlags([]string{"-write"})
	if err == nil {
		t.Fatal("bare invocation (no -from/-to/-full) was accepted — it must refuse the " +
			"implicit full-history backfill")
	}
	if !strings.Contains(err.Error(), "full-history") {
		t.Fatalf("refusal error should explain the full-history footgun, got: %v", err)
	}
}

// caseParseTxIndexBackfillFlags_ExplicitOptInsAreAccepted pins that each
// intentional path still works and resolves to the expected plan.
func caseParseTxIndexBackfillFlags_ExplicitOptInsAreAccepted(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantFrom uint32
		wantTo   uint32
	}{
		{"full opts into 2..tip", []string{"-full", "-write"}, 2, 0},
		{"explicit from is a resume point", []string{"-from", "80000000", "-write"}, 80_000_000, 0},
		{"explicit to bounds the range", []string{"-to", "1000000", "-dry-run"}, 2, 1_000_000},
		{"explicit from+to", []string{"-from", "500000", "-to", "1000000", "-write"}, 500_000, 1_000_000},
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

// caseParseTxIndexBackfillFlags_ZeroFromStillRejected keeps the pre-existing
// invariant: -from 0 and -window 0 are invalid regardless of the new guard.
func caseParseTxIndexBackfillFlags_ZeroFromStillRejected(t *testing.T) {
	if _, err := parseTxIndexBackfillFlags([]string{"-from", "0", "-write"}); err == nil {
		t.Error("-from 0 must be rejected")
	}
	if _, err := parseTxIndexBackfillFlags([]string{"-full", "-window", "0", "-write"}); err == nil {
		t.Error("-window 0 must be rejected")
	}
}

// TestParseTxIndexBackfillFlags groups the ch-txindex-backfill flag contracts as named subtests.
func TestParseTxIndexBackfillFlags(t *testing.T) {
	t.Run("ParseTxIndexBackfillFlags_RefusesBareFullHistory", caseParseTxIndexBackfillFlags_RefusesBareFullHistory)
	t.Run("ParseTxIndexBackfillFlags_ExplicitOptInsAreAccepted", caseParseTxIndexBackfillFlags_ExplicitOptInsAreAccepted)
	t.Run("ParseTxIndexBackfillFlags_ZeroFromStillRejected", caseParseTxIndexBackfillFlags_ZeroFromStillRejected)
}
