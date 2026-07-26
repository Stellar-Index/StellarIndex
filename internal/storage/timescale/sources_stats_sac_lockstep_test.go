// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestSourceStatsQueries_SACLiteralMatchesCanonical is the SQL half of
// the C4-012 membership↔valuation lock-step (audit-2026-07-23).
//
// These two queries embed the XLM SAC address as a STATIC literal (they
// must — they are `const` strings, deliberately never concatenated, so
// gosec G202 stays clean). Their volume CASE prices a SAC leg as XLM,
// while the row population comes from the caller's `= ANY($n)` form set,
// which is built from [canonical.AssetAliases]. If the literal here ever
// diverges from the constant there, the query goes back to valuing rows
// it does not select — the exact live undercount on
// /v1/markets/sources?asset=native that C4-012 recorded.
//
// Both queries must name it on BOTH legs (base and quote): XLM is a
// quote as often as it is a base (AQUA/XLM).
func TestSourceStatsQueries_SACLiteralMatchesCanonical(t *testing.T) {
	for name, q := range map[string]string{
		"pairSourceStatsQuery":  pairSourceStatsQuery,
		"assetSourceStatsQuery": assetSourceStatsQuery,
	} {
		if !strings.Contains(q, canonical.XLMSacContractID) {
			t.Errorf("%s does not contain canonical.XLMSacContractID (%s) — the volume CASE "+
				"and canonical.AssetAliases must name the same XLM SAC address",
				name, canonical.XLMSacContractID)
			continue
		}
		if got := strings.Count(q, canonical.XLMSacContractID); got != 2 {
			t.Errorf("%s mentions the XLM SAC %d time(s), want 2 (base leg and quote leg)", name, got)
		}
		// And the other two forms of the family must be reachable through
		// the bound array, not hard-coded — the filter is `= ANY($n)`.
		if !strings.Contains(q, "= ANY($1)") {
			t.Errorf("%s no longer binds the form set with `= ANY($1)`; the alias expansion "+
				"is how `native`/`crypto:XLM`/SAC all reach the same aggregate", name)
		}
	}

	// The alias family the handler binds must itself contain the literal
	// the SQL values — assert the join, not just each side.
	forms := canonical.AssetAliasStrings(canonical.NativeAsset())
	found := false
	for _, f := range forms {
		if f == canonical.XLMSacContractID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AssetAliasStrings(native) = %v, missing the SAC form the source-stats "+
			"volume CASE prices as XLM — Soroban XLM volume would be valued but never selected", forms)
	}
}
