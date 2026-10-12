// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// The graph has two sort orders so each direction is a primary-key range read. A read off the wrong ordering
// returns correct rows but becomes a full scan on a served route, so only a query-text check catches it.
func TestAccountGraphReadsEachDirectionOffItsOwnOrdering(t *testing.T) {
	for _, tc := range []struct {
		name, query, wantTable, wantKey, forbidTable string
	}{
		{
			name:        "inbound creations read the by-created ordering",
			query:       accountGraphInboundQuery,
			wantTable:   "stellar.account_creator_edges_by_created",
			wantKey:     "WHERE created = ?",
			forbidTable: "stellar.account_creator_edges\n",
		},
		{
			name:        "inbound sponsorships read the by-sponsored ordering",
			query:       accountGraphInboundQuery,
			wantTable:   "stellar.account_sponsor_edges_by_sponsored",
			wantKey:     "WHERE sponsored = ?",
			forbidTable: "stellar.account_sponsor_edges\n",
		},
		{
			name:        "outbound creations read the by-creator ordering",
			query:       accountGraphOutboundQuery,
			wantTable:   "stellar.account_creator_edges\n",
			wantKey:     "WHERE creator = ?",
			forbidTable: "stellar.account_creator_edges_by_created",
		},
		{
			name:        "outbound sponsorships read the by-sponsor ordering",
			query:       accountGraphOutboundQuery,
			wantTable:   "stellar.account_sponsor_edges\n",
			wantKey:     "WHERE sponsor = ?",
			forbidTable: "stellar.account_sponsor_edges_by_sponsored",
		},
		{
			name:        "the created page reads the by-creator ordering",
			query:       accountGraphCreatedPageQuery,
			wantTable:   "stellar.account_creator_edges\n",
			wantKey:     "WHERE creator = ? AND created > ?",
			forbidTable: "stellar.account_creator_edges_by_created",
		},
		{
			name:        "the sponsored page reads the by-sponsor ordering",
			query:       accountGraphSponsoredPageQuery,
			wantTable:   "stellar.account_sponsor_edges\n",
			wantKey:     "WHERE sponsor = ? AND sponsored > ?",
			forbidTable: "stellar.account_sponsor_edges_by_sponsored",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.query, tc.wantTable) {
				t.Errorf("query does not read %s:\n%s", strings.TrimSpace(tc.wantTable), tc.query)
			}
			if !strings.Contains(tc.query, tc.wantKey) {
				t.Errorf("query does not filter on %q — the read would not be a "+
					"primary-key range:\n%s", tc.wantKey, tc.query)
			}
			if tc.forbidTable != "" && strings.Contains(tc.query, tc.forbidTable) {
				t.Errorf("query reads %s, whose ORDER BY does not lead with the filtered "+
					"column — the read degrades to a full scan on a served route",
					strings.TrimSpace(tc.forbidTable))
			}
		})
	}
}

// The graph must never be read out of a cycle's WORKING table. Those are TRUNCATEd and refilled
// window by window, so during a cycle they hold a PARTIAL archive, and a served read against one
// would intermittently answer "this account was created by nobody".
func TestAccountGraphNeverReadsAWorkingTable(t *testing.T) {
	working := []string{
		"stellar.account_creators_ops",
		"stellar.account_sponsors_ops",
	}
	queries := map[string]string{
		"inbound":       accountGraphInboundQuery,
		"outbound":      accountGraphOutboundQuery,
		"revocations":   accountGraphRevocationsQuery,
		"coverage":      accountGraphCoverageQuery,
		"createdPage":   accountGraphCreatedPageQuery,
		"sponsoredPage": accountGraphSponsoredPageQuery,
	}
	for name, q := range queries {
		for _, w := range working {
			if strings.Contains(q, w) {
				t.Errorf("the %s query reads %s, a per-cycle working table that is "+
					"truncated and refilled mid-cycle", name, w)
			}
		}
	}
}

// Every graph read must be keyed by the account, since an unkeyed one scans the whole edge table on a public
// route. The coverage query is the one exception: it reads a nine-row metric-keyed table.
func TestAccountGraphReadsAreAccountKeyed(t *testing.T) {
	for name, q := range map[string]string{
		"inbound":       accountGraphInboundQuery,
		"outbound":      accountGraphOutboundQuery,
		"revocations":   accountGraphRevocationsQuery,
		"createdPage":   accountGraphCreatedPageQuery,
		"sponsoredPage": accountGraphSponsoredPageQuery,
	} {
		if !strings.Contains(q, "= ?") {
			t.Errorf("the %s query carries no equality bind — it is not keyed by the "+
				"account and would scan:\n%s", name, q)
		}
	}
	if strings.Contains(accountGraphCoverageQuery, "stellar.account_creator_edges") ||
		strings.Contains(accountGraphCoverageQuery, "stellar.account_sponsor_edges") {
		t.Error("the unkeyed coverage query reads an edge table — it may only read the " +
			"metric-keyed stats tables")
	}
}

// History reads must be primary-key range reads off the outbound orderings; the reverse orderings return the
// same rows, so no payload test sees the mistake, yet it turns a per-account read into a full scan.
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

// Both statements must be account-scoped: an unkeyed arm would scan 21M creation or 4M sponsorship edges
// per request, the unbounded-scan shape that stalled /v1/assets.
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

// The working tables hold a PARTIAL archive for the length of a rollup cycle (truncated and
// refilled across many windows), which is why the edge tables are staged and EXCHANGEd. A
// history read that reached into one would serve a half-built series mid-cycle.
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

// The unplaced count subtracts on a UInt64 column; without the Int64 cast `creations - 2` wraps for a
// single-event edge. The guard discards the wrapped value, but the arithmetic must not produce one.
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

// The placement identity (events == placed + unplaced) makes the series exact rather than
// approximate, and the clamp stops a bad row from serving 2^64 placed events instead of
// failing. Exercised through the same assignment the reader's scan loop performs.
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
