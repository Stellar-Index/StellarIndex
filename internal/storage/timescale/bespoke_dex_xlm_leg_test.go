// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// The source_volume_1h read contract (migration 0068): the CAGG cannot
// materialize a finished USD figure — the XLM/USD multiply cross-
// references prices_1m — so it stores the raw inputs and the reader MUST
// apply
//
//	sum_usd_priced + (sum_xlm_base + sum_xlm_quote)/10^7 * <XLM/USD vwap>
//
// A reader that sums only sum_usd_priced serves every XLM-denominated leg
// the ingest valuation left unpriced as $0, and disagrees with the OTHER
// reader of the same CAGG (sourceVolumeHistory) — two different 24h
// volumes for one source, both rendered on the source page.
//
// These tests pin every term of that expression in both 24h readers.

// dexXLMLegTerms is every term of migration 0068's read expression that a
// correct source_volume_1h reader must carry.
var dexXLMLegTerms = []string{
	"sum_usd_priced",
	"sum_xlm_base",
	"sum_xlm_quote",
	"10000000::numeric",
	"SELECT vwap FROM xlm_usd",
}

// TestDexActivitySeriesQuery24hAppliesXLMLeg — the 24h hourly volume
// series must apply the whole read expression, not just the priced leg.
func TestDexActivitySeriesQuery24hAppliesXLMLeg(t *testing.T) {
	q := dexActivitySeriesQuery(1)
	for _, term := range dexXLMLegTerms {
		if !strings.Contains(q, term) {
			t.Errorf("24h activity series must apply migration 0068's read expression; missing %q", term)
		}
	}
	if !strings.Contains(q, "base_asset = 'native'") {
		t.Error("24h activity series must anchor the XLM leg on the native/USD vwap CTE")
	}
	// The vwap CTE is bound to no parameter: the query's own $1/$2 stay
	// source + window, so the caller's argument list is unchanged.
	if strings.Count(q, "$3") != 0 {
		t.Error("24h activity series must not introduce a third bind parameter")
	}
	assertWindowBounded(t, "24h activity series", q)
	assertDEXNumericSafe(t, "24h activity series", q)
}

// TestDexWindowKPIQuery24hAppliesXLMLeg — same for the 24h "USD volume"
// KPI, which is the headline figure the source page compares against
// /v1/sources' per-source volume for the same source and window.
func TestDexWindowKPIQuery24hAppliesXLMLeg(t *testing.T) {
	q := dexWindowKPIQuery(1)
	for _, term := range dexXLMLegTerms {
		if !strings.Contains(q, term) {
			t.Errorf("24h window KPI must apply migration 0068's read expression; missing %q", term)
		}
	}
	if !strings.Contains(q, "round(") {
		t.Error("24h window KPI must round the USD figure via exact NUMERIC round (ADR-0003)")
	}
	assertWindowBounded(t, "24h window KPI", q)
	assertDEXNumericSafe(t, "24h window KPI", q)
}

// TestDexWindowKPIQueryLongWindowUnchanged — the >1d form reads the daily
// pair CAGG (0064), which materializes no XLM inputs, so it must NOT grow
// an XLM leg it has no columns for.
func TestDexWindowKPIQueryLongWindowUnchanged(t *testing.T) {
	for _, days := range []int{7, 30, 90} {
		for _, q := range []string{dexWindowKPIQuery(days), dexActivitySeriesQuery(days)} {
			if strings.Contains(q, "sum_xlm_base") || strings.Contains(q, "xlm_usd") {
				t.Errorf("%dd query must stay on the daily pair CAGG's priced-only vol", days)
			}
		}
	}
}
