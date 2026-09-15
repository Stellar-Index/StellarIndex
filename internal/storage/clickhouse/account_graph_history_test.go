// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// The history reads must be PRIMARY-KEY range reads off the outbound
// orderings, exactly like the graph's outbound arms. Reading them off the
// reverse orderings — account_creator_edges_by_created /
// account_sponsor_edges_by_sponsored — returns the same rows, so no test
// of the payload can see the mistake; it just turns a per-account read
// into a full scan of a 21-million-row table on a served route. Same
// silent failure the sibling graph test pins.
func TestAccountGraphHistoryReadsTheOutboundOrderings(t *testing.T) {
	for _, tc := range []struct {
		name, query, wantTable, wantKey, forbidTable string
	}{
		{
			name:        "periods read creations off the by-creator ordering",
			query:       accountGraphHistoryPeriodsQuery,
			wantTable:   "stellar.account_creator_edges\n",
			wantKey:     "WHERE creator = ?",
			forbidTable: "stellar.account_creator_edges_by_created",
		},
		{
			name:        "periods read sponsorships off the by-sponsor ordering",
			query:       accountGraphHistoryPeriodsQuery,
			wantTable:   "stellar.account_sponsor_edges\n",
			wantKey:     "WHERE sponsor = ?",
			forbidTable: "stellar.account_sponsor_edges_by_sponsored",
		},
		{
			name:        "totals read creations off the by-creator ordering",
			query:       accountGraphHistoryTotalsQuery,
			wantTable:   "stellar.account_creator_edges\n",
			wantKey:     "WHERE creator = ?",
			forbidTable: "stellar.account_creator_edges_by_created",
		},
		{
			name:        "totals read sponsorships off the by-sponsor ordering",
			query:       accountGraphHistoryTotalsQuery,
			wantTable:   "stellar.account_sponsor_edges\n",
			wantKey:     "WHERE sponsor = ?",
			forbidTable: "stellar.account_sponsor_edges_by_sponsored",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.query, tc.wantTable) {
				t.Errorf("query does not read %q", strings.TrimSpace(tc.wantTable))
			}
			if !strings.Contains(tc.query, tc.wantKey) {
				t.Errorf("query is not keyed by %q — that is a full scan, not a range read", tc.wantKey)
			}
			if strings.Contains(tc.query, tc.forbidTable) {
				t.Errorf("query reads %q, the wrong ordering for this direction", tc.forbidTable)
			}
		})
	}
}

// Both statements must be account-scoped. An unkeyed arm would scan
// 21 million creation edges or 4 million sponsorship edges per request,
// which is the standing no-unbounded-scan rule and the exact shape that
// stalled /v1/assets.
func TestAccountGraphHistoryReadsAreAccountKeyed(t *testing.T) {
	for name, q := range map[string]string{
		"periods": accountGraphHistoryPeriodsQuery,
		"totals":  accountGraphHistoryTotalsQuery,
	} {
		t.Run(name, func(t *testing.T) {
			froms := strings.Count(q, "FROM stellar.")
			keyed := strings.Count(q, "WHERE creator = ?") + strings.Count(q, "WHERE sponsor = ?")
			if froms != keyed {
				t.Errorf("%d table reads but %d account-keyed predicates — an arm is unbounded", froms, keyed)
			}
		})
	}
}

// The working tables hold a PARTIAL archive for the length of a rollup
// cycle (they are truncated and refilled across 65 walked windows), which
// is the whole reason the edge tables are staged and EXCHANGEd. A history
// read that reached into one would serve a half-built series mid-cycle
// and call it history.
func TestAccountGraphHistoryNeverReadsAWorkingTable(t *testing.T) {
	for _, working := range []string{
		"stellar.account_creators_ops",
		"stellar.account_sponsors_ops",
		"_staging",
	} {
		for name, q := range map[string]string{
			"periods": accountGraphHistoryPeriodsQuery,
			"totals":  accountGraphHistoryTotalsQuery,
		} {
			if strings.Contains(q, working) {
				t.Errorf("%s query reads %q, which holds a partial archive mid-cycle", name, working)
			}
		}
	}
}

// The unplaced count is a subtraction on a UInt64 column. Doing it
// without the Int64 cast makes `creations - 2` WRAP for a single-event
// edge; the surrounding guard discards the wrapped value today, but the
// arithmetic must not be able to produce one in the first place.
func TestAccountGraphHistoryUnplacedSubtractsInSignedArithmetic(t *testing.T) {
	q := accountGraphHistoryTotalsQuery
	for _, want := range []string{
		"greatest(toInt64(creations) - 2, 0)",
		"greatest(toInt64(sponsorships_started) - 2, 0)",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("totals query is missing %q — an unsigned subtraction here underflows to 2^64", want)
		}
	}
}

// The placement identity — events == placed + unplaced — is what makes
// the series honest rather than approximate, and the clamp is what keeps
// a hypothetical bad row from serving 2^64 placed events instead of
// failing. Exercised through the same assignment the reader's scan loop
// performs.
func TestAccountGraphHistoryPlacementIdentity(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		events, unplaced         uint64
		wantPlaced, wantUnplaced uint64
	}{
		{"every event placed", 12, 0, 12, 0},
		{"single-event edges only", 1, 0, 1, 0},
		{"a repeated edge leaves a middle", 10, 3, 7, 3},
		{"nothing at all", 0, 0, 0, 0},
		{"impossible row is clamped, not wrapped", 5, 9, 0, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var side AccountGraphHistorySide
			unplaced := tc.unplaced
			side.Events = tc.events
			if unplaced > tc.events {
				unplaced = tc.events
			}
			side.EventsUnplaced = unplaced
			side.EventsPlaced = tc.events - unplaced

			if side.EventsPlaced != tc.wantPlaced || side.EventsUnplaced != tc.wantUnplaced {
				t.Fatalf("placed/unplaced = %d/%d, want %d/%d",
					side.EventsPlaced, side.EventsUnplaced, tc.wantPlaced, tc.wantUnplaced)
			}
			if side.EventsPlaced+side.EventsUnplaced != side.Events {
				t.Errorf("identity broken: %d + %d != %d",
					side.EventsPlaced, side.EventsUnplaced, side.Events)
			}
		})
	}
}

// sideFor routes the SQL discriminator. Getting it wrong would silently
// file every sponsorship figure under creations.
func TestAccountGraphHistorySideFor(t *testing.T) {
	var h AccountGraphHistory
	if got := h.sideFor(GraphRelationSponsored); got != &h.Sponsored {
		t.Error("sponsored discriminator did not route to the sponsorship side")
	}
	if got := h.sideFor(GraphRelationCreated); got != &h.Created {
		t.Error("created discriminator did not route to the creation side")
	}
}
