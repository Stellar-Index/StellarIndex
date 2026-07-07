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
	q := sorobanVolume24hUSDQuery

	// Bounded on BOTH the anchor CTE and the outer scan — the whole point
	// is a cheap 24h read, not a full prices_1m history walk.
	if strings.Count(q, "bucket >= now() - INTERVAL '24 hours'") < 2 {
		t.Error("query missing the 24h lower bound on the anchor CTE and/or the outer scan")
	}
	if !strings.Contains(q, "AND bucket  < now()") {
		t.Error("query missing the closed `bucket < now()` upper bound on the outer scan")
	}

	// USD-pegged legs come from the insert-time volume_usd (kept), XLM legs
	// are anchored via the xlm_usd CTE (added). Both must be present.
	if !strings.Contains(q, "WHEN volume_usd > 0") {
		t.Error("query missing the USD-pegged-leg branch (volume_usd > 0)")
	}
	if !strings.Contains(q, "(SELECT vwap FROM xlm_usd)") {
		t.Error("query missing the xlm_usd anchor multiplication")
	}
	// The XLM leg is valued off BOTH stored directions: native (or its SAC)
	// as base (volume) and as quote (vwap*volume).
	if !strings.Contains(q, "WHEN base_asset IN ('native', '"+nativeXLMSAC+"')") {
		t.Error("query missing the XLM-base-leg branch (native + SAC)")
	}
	if !strings.Contains(q, "WHEN quote_asset IN ('native', '"+nativeXLMSAC+"')") {
		t.Error("query missing the XLM-quote-leg branch (native + SAC)")
	}
	// Asset participates as either side; result floored to a definite "0".
	if !strings.Contains(q, "WHERE (base_asset = $1 OR quote_asset = $1)") {
		t.Error("query must match the asset as base OR quote")
	}
	if !strings.Contains(q, "COALESCE(sum(") {
		t.Error("query must COALESCE the sum so an empty asset returns 0, not NULL")
	}
}
