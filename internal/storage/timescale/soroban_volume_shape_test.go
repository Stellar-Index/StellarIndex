// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// TestSorobanVolume24hUSDQueryShape guards the XLM-anchored per-asset
// USD-volume query (#37). The load-bearing properties — a bounded 24h
// window (never an unbounded walk), the USD-pegged discriminator, and the
// XLM base+quote anchor legs — must not silently regress; a full-behaviour
// check lives in the integration suite (TestSorobanVolume24hUSD_*).
func TestSorobanVolume24hUSDQueryShape(t *testing.T) {
	// Whitespace-collapsed so the pins below match SQL, not its layout.
	q := strings.Join(strings.Fields(sorobanVolume24hUSDQuery), " ")

	// Bounded on BOTH the anchor CTE and the outer scan — the whole point
	// is a cheap 24h read, not a full prices_1m history walk.
	if strings.Count(q, "bucket >= now() - INTERVAL '24 hours'") < 2 {
		t.Error("query missing the 24h lower bound on the anchor CTE and/or the outer scan")
	}
	if !strings.Contains(q, "AND bucket <= now() - INTERVAL '1 minute'") {
		t.Error("query missing the closed-bucket `bucket <= now() - 1 minute` upper bound on the outer scan")
	}

	// Valued per trade: the insert-time usd_volume, else the XLM leg. An
	// either/or on a prices_1m row's volume_usd drops the unvalued trades
	// of a partly-valued bucket.
	if !strings.Contains(q, "COALESCE(usd_volume, CASE") {
		t.Error("query must value each trade as COALESCE(usd_volume, <XLM leg>)")
	}
	if strings.Contains(q, "volume_usd >") {
		t.Error("query must not pick a valuation from a prices_1m row's volume_usd")
	}
	if !strings.Contains(q, "(SELECT vwap FROM xlm_usd)") {
		t.Error("query missing the xlm_usd anchor multiplication")
	}
	// The XLM leg is valued off BOTH stored directions: native (or its SAC)
	// as base (base_amount) and as quote (quote_amount).
	if !strings.Contains(q, "WHEN base_asset IN ('native', '"+nativeXLMSAC+"')") {
		t.Error("query missing the XLM-base-leg branch (native + SAC)")
	}
	if !strings.Contains(q, "WHEN quote_asset IN ('native', '"+nativeXLMSAC+"')") {
		t.Error("query missing the XLM-quote-leg branch (native + SAC)")
	}
	// Asset participates as either side; result floored to a definite "0".
	if !strings.Contains(q, "FROM trades WHERE (base_asset = $1 OR quote_asset = $1) AND ts >= now() - INTERVAL '24 hours'") {
		t.Error("query must read the asset's trades as base OR quote, bounded by an index-usable 24h ts bound")
	}
	if !strings.Contains(q, "COALESCE(sum(") {
		t.Error("query must COALESCE the sum so an empty asset returns 0, not NULL")
	}
}
