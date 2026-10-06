// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// TestSorobanVolume24hUSDQueryShape guards the XLM-anchored per-asset
// USD-volume query. The load-bearing properties — a bounded 24h window, a
// per-trade valuation, the XLM leg at its own minute's anchor (never a
// current-window scalar) and the excluded-trade count — must not silently
// regress; behaviour lives in the integration suite (TestSorobanVolume24hUSD_*,
// TestXLMLegVolume_TradeTimeNeverSpot).
func TestSorobanVolume24hUSDQueryShape(t *testing.T) {
	q := strings.Join(strings.Fields(sorobanVolume24hUSDQuery), " ")
	for _, pin := range []struct{ sub, why string }{
		{"FROM trades WHERE (base_asset = $1 OR quote_asset = $1) AND ts >= now() - INTERVAL '24 hours'", "asset as base OR quote, index-usable 24h ts bound"},
		{"WHERE t.bucket >= now() - INTERVAL '24 hours' AND t.bucket <= now() - INTERVAL '1 minute'", "closed 1-minute buckets of the last 24h"},
		{"COALESCE(t.usd_volume, CASE", "per-trade valuation: usd_volume, else the XLM leg"},
		{"WHEN t.base_asset IN ('native', $2::text) THEN (t.base_amount / 1e7::numeric) * xa.vwap", "XLM-base leg (native + SAC) at the trade's anchor"},
		{"WHEN t.quote_asset IN ('native', $2::text) THEN (t.quote_amount / 1e7::numeric) * xa.vwap", "XLM-quote leg (native + SAC) at the trade's anchor"},
		{"LEFT JOIN xlm_usd_grid xa ON xa.minute = t.bucket", "anchor joined at the trade's own minute"},
		{"count(*) FILTER (WHERE usd IS NULL)", "excluded trades counted for the lower-bound flag"},
		{"COALESCE(sum(usd), 0)", "empty asset returns 0, not NULL"},
	} {
		if !strings.Contains(q, pin.sub) {
			t.Errorf("query lost %s: missing %q", pin.why, pin.sub)
		}
	}
	if strings.Contains(q, "percentile_disc") {
		t.Error("query values XLM legs with a current-window scalar")
	}
}
