// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// These tests pin the server-side partial-bucket honesty rule (UXP-16
// class, audit 2026-07-31): every DAILY-grain bespoke series excludes the
// current, still-accumulating day — its partial bucket renders as a
// phantom activity/volume cliff on every daily chart — while the 24h
// window's HOURLY grain keeps its live edge.

// TestCompleteDaysOnlyFragment — the helper itself: empty at the 24h
// (hourly-grain) window, the day cutoff otherwise.
func TestCompleteDaysOnlyFragment(t *testing.T) {
	if got := completeDaysOnly(1, "ts"); got != "" {
		t.Errorf("completeDaysOnly(1) = %q, want empty (hourly grain keeps its live edge)", got)
	}
	if got := completeDaysOnly(90, "ts"); !strings.Contains(got, "ts < "+completeDayCutoffSQL) {
		t.Errorf("completeDaysOnly(90) = %q, want the %q cutoff", got, completeDayCutoffSQL)
	}
}

// TestDailySeriesExcludeCurrentDay sweeps every windowed bespoke series
// builder: the daily form must carry the complete-day cutoff, the hourly
// (24h) form must NOT.
func TestDailySeriesExcludeCurrentDay(t *testing.T) {
	builders := []struct {
		name string
		fn   func(windowDays int) string
	}{
		{"dexActivitySeriesQuery", dexActivitySeriesQuery},
		{"dexTradersSeriesQuery", dexTradersSeriesQuery},
		{"dexTopPairsSeriesQuery", dexTopPairsSeriesQuery},
		{"rozoSettledSeriesQuery", rozoSettledSeriesQuery},
		{"lendingAuctionSeriesQuery", lendingAuctionSeriesQuery},
		{"lendingPerPoolSeriesQuery", lendingPerPoolSeriesQuery},
		{"creditSettlementSeriesQuery", creditSettlementSeriesQuery},
		{"creditPositionsOpenedSeriesQuery", creditPositionsOpenedSeriesQuery},
		{"oracleUpdatesSeriesQuery", oracleUpdatesSeriesQuery},
		{"oraclePerFeedSeriesQuery", oraclePerFeedSeriesQuery},
		{"defindexVaultCountSeriesQuery", defindexVaultCountSeriesQuery},
		{"defindexStrategyVolumeSeriesQuery", defindexStrategyVolumeSeriesQuery},
		{"defindexStrategyEventsSeriesQuery", defindexStrategyEventsSeriesQuery},
		{"lendingSideSeriesQuery(supply)", func(d int) string { return lendingSideSeriesQuery(d, lendingSupplySideKinds) }},
		{"lendingSideSeriesQuery(borrow)", func(d int) string { return lendingSideSeriesQuery(d, lendingBorrowSideKinds) }},
		{"lendingBackstopFlowSeriesQuery(in)", func(d int) string { return lendingBackstopFlowSeriesQuery(d, lendingBackstopInflowKinds) }},
		{"lendingBackstopFlowSeriesQuery(out)", func(d int) string { return lendingBackstopFlowSeriesQuery(d, lendingBackstopOutflowKinds) }},
		{"cctpFlowSeriesQuery(inbound)", func(d int) string { return cctpFlowSeriesQuery(d, true) }},
		{"cctpFlowSeriesQuery(outbound)", func(d int) string { return cctpFlowSeriesQuery(d, false) }},
		{"cctpPerChainSeriesQuery(inbound)", func(d int) string { return cctpPerChainSeriesQuery(d, true) }},
		{"cctpPerChainSeriesQuery(outbound)", func(d int) string { return cctpPerChainSeriesQuery(d, false) }},
	}
	for _, b := range builders {
		if q := b.fn(1); strings.Contains(q, completeDayCutoffSQL) {
			t.Errorf("%s at the 24h window must keep its hourly live edge (no %q)", b.name, completeDayCutoffSQL)
		}
		for _, days := range []int{7, 30, 90} {
			if q := b.fn(days); !strings.Contains(q, "< "+completeDayCutoffSQL) {
				t.Errorf("%s at %dd (daily grain) must exclude the current partial day", b.name, days)
			}
		}
	}
	// The all-time cumulative net-inflow chart is always daily-grain.
	if q := cctpCumulativeNetInflowQuery(); !strings.Contains(q, "< "+completeDayCutoffSQL) {
		t.Error("cctpCumulativeNetInflowQuery must exclude the current partial day")
	}
}
