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
