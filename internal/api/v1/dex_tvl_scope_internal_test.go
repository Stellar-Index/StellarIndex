// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"
)

// A pooled-liquidity protocol may be summed into the headline or left out, but never silently: an indexed AMM missing from
// tvl_total.excluded leaves readers unable to see it, and the drill-down would wrongly blame deployment wiring.
// Both surfaces read dexTVLScopeExclusions, so this guard makes the next pooled protocol declare itself.
func TestDEXTVLScope_PooledProtocolIsDerivedOrExplicitlyExcluded(t *testing.T) {
	t.Parallel()

	derived := map[string]bool{}
	for _, d := range NewDEXTVLCache(DEXTVLSources{}).derivations() {
		derived[d.name] = true
	}
	if len(derived) == 0 {
		t.Fatal("no TVL derivations enumerated — this guard needs re-aiming")
	}

	excluded := map[string]bool{}
	for _, ex := range dexTVLScopeExclusions {
		excluded[ex.Subject] = true
	}

	for _, p := range protocolRegistry {
		// Pooled-liquidity and vault/AUM categories only. Lending/oracle/bridge protocols are not
		// AMM-headline candidates, and those carrying a comparable figure (blend, sorocredit,
		// defindex, upshift) already have their own exclusion. "yield" is included because a
		// vault's AUM has the same double-count/no-current-state shape a reader might sum.
		if p.Category != "amm" && p.Category != "dex" && p.Category != "yield" {
			continue
		}
		if derived[p.Name] || excluded[p.Name] {
			continue
		}
		t.Errorf("protocol %q is category %q but is neither summed into tvl_total nor named in "+
			"dexTVLScopeExclusions: the headline omits it with no reason on the wire, and "+
			"GET /v1/protocols/%s/tvl blames the omission on deployment wiring. Add a scope "+
			"exclusion saying why, or wire a derivation.", p.Name, p.Category, p.Name)
	}
}
