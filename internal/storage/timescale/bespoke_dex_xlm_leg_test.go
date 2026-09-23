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

// TestDexWindowKPIQuery24hReportsUnvaluedXLM — dexXLMLegUSD COALESCEs a
// missing XLM/USD vwap to 0, and the anchor outage that empties xlm_usd is
// the one that parks volume in the XLM legs. The 24h KPI query must
// therefore also return the XLM it could not value, so the block can
// serve the figure as a lower bound instead of a silent total.
func TestDexWindowKPIQuery24hReportsUnvaluedXLM(t *testing.T) {
	q := dexWindowKPIQuery(1)
	if !strings.Contains(q, "(SELECT vwap FROM xlm_usd) IS NULL") {
		t.Error("24h window KPI must detect an empty XLM/USD vwap CTE")
	}
	if !strings.Contains(q, dexXLMLegUnvalued) {
		t.Error("24h window KPI must select the unvalued XLM leg (dexXLMLegUnvalued)")
	}
	assertDEXNumericSafe(t, "24h window KPI", q)
}

// TestDexVolumeKPIHintAnchorOutage — on an anchor outage the 24h volume
// KPI is the priced leg only; its hint must say lower bound and name the
// excluded XLM, never claim the XLM legs are valued.
func TestDexVolumeKPIHintAnchorOutage(t *testing.T) {
	h := dexVolumeKPIHint(1, "30.0000000")
	if !strings.Contains(h, "LOWER BOUND") || !strings.Contains(h, "excludes 30.0000000 XLM") {
		t.Errorf("anchor-outage hint must be a named lower bound, got %q", h)
	}
	if strings.Contains(h, "at the current XLM/USD vwap") {
		t.Errorf("anchor-outage hint must not claim the XLM legs are valued, got %q", h)
	}
	if h := dexVolumeKPIHint(1, ""); strings.Contains(h, "LOWER BOUND") || !strings.Contains(h, "XLM/USD vwap") {
		t.Errorf("anchored 24h hint must describe the XLM-valued leg, got %q", h)
	}
	if h := dexVolumeKPIHint(7, "30.0000000"); strings.Contains(h, "XLM") {
		t.Errorf("7d hint has no XLM leg to disclose, got %q", h)
	}
}

// TestDexUSDValuationNoteAnchorOutage — the block note is the served
// statement of derivation; on an outage it must not say the XLM legs are
// valued.
func TestDexUSDValuationNoteAnchorOutage(t *testing.T) {
	n := dexUSDValuationNote(1, "30.0000000")
	if strings.Contains(n, "additionally value") {
		t.Errorf("anchor-outage note must not claim the XLM legs are valued, got %q", n)
	}
	for _, want := range []string{"30.0000000 XLM", "NOT valued", "lower bounds"} {
		if !strings.Contains(n, want) {
			t.Errorf("anchor-outage note must contain %q, got %q", want, n)
		}
	}
}

// TestDexAvgTradeHint24hIsUnaugmented — the average divides the raw
// trades.usd_volume sum, not the XLM-augmented 24h volume KPI beside it,
// so neither its hint nor the block note may present it as that KPI ÷
// trades.
func TestDexAvgTradeHint24hIsUnaugmented(t *testing.T) {
	for _, days := range []int{1, 7} {
		h := dexAvgTradeHint(days)
		if strings.Contains(h, "window USD volume") || !strings.Contains(h, "usd_volume of priced trades") {
			t.Errorf("%dd avg hint must name its usd_volume-only numerator, got %q", days, h)
		}
	}
	if h := dexAvgTradeHint(1); !strings.Contains(h, "excludes the XLM-denominated legs") {
		t.Errorf("24h avg hint must disclose it omits the XLM leg, got %q", h)
	}
	if n := dexUSDValuationNote(1, ""); !strings.Contains(n, "average trade size") {
		t.Errorf("24h note must name the average trade size as usd_volume-only, got %q", n)
	}
}
