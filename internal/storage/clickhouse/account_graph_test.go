// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// The graph exists in two sort orders precisely so that each direction is
// a primary-key range read. Reading a direction off the WRONG ordering is
// the failure this pins, and it is a silent one: the query returns the
// right rows, so no test of the payload can see it — it just becomes a
// full scan of a 21-million-row table on a served route, which is the
// exact regression stellar.ops_by_source was built to retire (a
// skip-index scan measured 110x slower than the PK-prefixed read).
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

// The graph must never be read out of a cycle's WORKING table. Those are
// TRUNCATEd and refilled over 65 walked windows, so for the length of a
// cycle they hold a PARTIAL archive — a served read against one would
// answer "this account was created by nobody" for most of the chain,
// intermittently, and only while a cycle happened to be running.
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

// Every graph read must be keyed by the account. An unkeyed one is a scan
// of the whole edge table on an unauthenticated route — the coverage
// query is the single deliberate exception, and it reads a metric-keyed
// table of nine rows.
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
