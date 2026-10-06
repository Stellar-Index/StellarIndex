// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// The 24h DEX readers value source_volume_1h at trade time only: they sum
// sum_usd_priced and never multiply the XLM legs by a current XLM/USD vwap.

func assertNoSpotXLM(t *testing.T, name, q string) {
	t.Helper()
	for _, bad := range []string{"xlm_usd", "vwap", "prices_1m"} {
		if strings.Contains(q, bad) {
			t.Errorf("%s must not value XLM legs at a current price; contains %q", name, bad)
		}
	}
	if !strings.Contains(q, "sum_usd_priced") {
		t.Errorf("%s must sum sum_usd_priced", name)
	}
}

func TestDexActivitySeriesQuery24hIsTradeTime(t *testing.T) {
	q := dexActivitySeriesQuery(1)
	assertNoSpotXLM(t, "24h activity series", q)
	assertWindowBounded(t, "24h activity series", q)
	assertDEXNumericSafe(t, "24h activity series", q)
}

func TestDexWindowKPIQuery24hIsTradeTime(t *testing.T) {
	q := dexWindowKPIQuery(1)
	assertNoSpotXLM(t, "24h window KPI", q)
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

// TestDexWindowKPIQuery24hReportsUnvaluedXLM — the 24h KPI must return the
// XLM it excluded so the block serves a named lower bound.
func TestDexWindowKPIQuery24hReportsUnvaluedXLM(t *testing.T) {
	q := dexWindowKPIQuery(1)
	if !strings.Contains(q, dexXLMLegUnvalued) {
		t.Error("24h window KPI must select the unvalued XLM leg (dexXLMLegUnvalued)")
	}
	assertDEXNumericSafe(t, "24h window KPI", q)
}

// TestDexVolumeKPIHintAnchorOutage — with unpriced XLM the 24h volume KPI
// hint must say lower bound and name the excluded XLM.
func TestDexVolumeKPIHintAnchorOutage(t *testing.T) {
	h := dexVolumeKPIHint(1, "30.0000000")
	if !strings.Contains(h, "LOWER BOUND") || !strings.Contains(h, "excludes 30.0000000 XLM") {
		t.Errorf("anchor-outage hint must be a named lower bound, got %q", h)
	}
	if h := dexVolumeKPIHint(1, ""); strings.Contains(h, "LOWER BOUND") {
		t.Errorf("24h hint with nothing excluded must not claim a lower bound, got %q", h)
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

// TestDexAvgTradeHintIsUSDVolumeOnly — the average divides the raw
// trades.usd_volume sum by priced trades.
func TestDexAvgTradeHintIsUSDVolumeOnly(t *testing.T) {
	for _, days := range []int{1, 7} {
		if h := dexAvgTradeHint(days); !strings.Contains(h, "usd_volume of priced trades") {
			t.Errorf("%dd avg hint must name its usd_volume-only numerator, got %q", days, h)
		}
	}
}
