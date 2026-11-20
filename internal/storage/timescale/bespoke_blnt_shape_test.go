// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// TestBLNTBespokeQueriesReadOnlyBLNTTables guards that the BLNT block reads blnt_events alone, scoped by contract_role, and never a
// Blend table or a Blend-only column.
func TestBLNTBespokeQueriesReadOnlyBLNTTables(t *testing.T) {
	for name, q := range map[string]string{
		"position KPIs": blntPositionKPISQL, "all-time": blntAllTimeKPISQL, "net flow": blntNetFlowSQL,
		"pool activity": blntPoolActivitySQL, "daily series": blntDailyPositionSeriesSQL(90), "auction KPIs": blntAuctionKPISQL,
		"recent auctions": blntRecentAuctionsSQL, "backstop count": blntBackstopCountSQL, "backstop flow": blntBackstopFlowSQL,
		"emissions": blntEmissionSQL,
	} {
		if !strings.Contains(q, "FROM blnt_events") || !strings.Contains(q, "contract_role") {
			t.Errorf("%s must read blnt_events scoped by contract_role", name)
		}
		for _, banned := range []string{"blend_", "token_amount", "b_or_d_amount"} {
			if strings.Contains(q, banned) {
				t.Errorf("%s reads Blend's shape (%s)", name, banned)
			}
		}
	}
}

// TestBLNTBackstopFlowNeverSumsAcrossTiers guards that backstop token sums group by (pool, tier): each tier holds its own asset.
func TestBLNTBackstopFlowNeverSumsAcrossTiers(t *testing.T) {
	if !strings.Contains(blntBackstopFlowSQL, "GROUP BY pool, tier") {
		t.Error("backstop flow must group by (pool, tier)")
	}
	if strings.Contains(blntBackstopFlowSQL, "amount2") {
		t.Error("backstop flow must sum tier-asset tokens (amount), never shares (amount2)")
	}
}

// TestBLNTSumsWithholdUndecodedAmounts guards that a summed group with a NULL amount is dropped, not understated.
func TestBLNTSumsWithholdUndecodedAmounts(t *testing.T) {
	for name, q := range map[string]string{"net flow": blntNetFlowSQL, "backstop flow": blntBackstopFlowSQL} {
		if !strings.Contains(q, "HAVING NOT bool_or(amount IS NULL)") {
			t.Errorf("%s must withhold a group with an undecoded amount", name)
		}
	}
	if !strings.Contains(blntEmissionSQL, "bool_or(event_kind = 'claim' AND amount IS NULL)") {
		t.Error("emission sum must detect an undecoded claim amount")
	}
}
