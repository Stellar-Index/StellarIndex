// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The census recomputes only its newest day each cycle, so contract events re-derived into an earlier day stay out of
// /v1/contracts unless the run names the recompute, from the UTC day of the range's first ledger.
func TestCensusFollowUp_NamesTheRecomputeDay(t *testing.T) {
	orig := backfillLedgerCloseTimes
	t.Cleanup(func() { backfillLedgerCloseTimes = orig })

	backfillLedgerCloseTimes = func(_ context.Context, _ string, seqs []uint32) (map[uint32]time.Time, error) {
		if len(seqs) != 1 || seqs[0] != 50_000_000 {
			t.Fatalf("looked up %v, want only the range's first ledger", seqs)
		}
		return map[uint32]time.Time{50_000_000: time.Date(2024, 3, 5, 23, 59, 58, 0, time.UTC)}, nil
	}
	got := censusFollowUp(context.Background(), "unused", 50_000_000)
	for _, want := range []string{"FOLLOW-UP OWED", "stellar.contracts_census_daily", "ch-census-rollup -from-day 2024-03-05 -write"} {
		if !strings.Contains(got, want) {
			t.Fatalf("follow-up %q lacks %q", got, want)
		}
	}

	backfillLedgerCloseTimes = func(context.Context, string, []uint32) (map[uint32]time.Time, error) {
		return nil, errors.New("lake down")
	}
	got = censusFollowUp(context.Background(), "unused", 50_000_000)
	if !strings.Contains(got, "FOLLOW-UP OWED") || !strings.Contains(got, "-from-day <UTC day of ledger 50000000>") {
		t.Fatalf("follow-up without a close time %q must still be owed and name the ledger", got)
	}
}
