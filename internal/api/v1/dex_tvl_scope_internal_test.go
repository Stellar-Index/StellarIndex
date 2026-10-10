// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"
)

// A pooled-liquidity protocol may be summed into the headline total, or
// it may be left out — but it may never be left out SILENTLY.
//
// The failure this guards is the one sushiswap_v3 shipped with.
// The protocol was registered, its trades served, and its category
// "amm" — and the TVL surface said nothing about it at all: it was
// absent from tvl_total.excluded, so a reader adding up the headline had
// no way to learn an indexed AMM was missing from it, and
// GET /v1/protocols/sushiswap_v3/tvl blamed the omission on this
// deployment's wiring ("not wired on this deployment") when the real
// reason is permanent and methodological — concentrated liquidity has no
// two-sided reserve to sum.
//
// Both halves come from dexTVLScopeExclusions, so one entry fixes both
// surfaces and this guard is what makes the next pooled protocol
// declare itself.
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
		// Pooled-liquidity and vault/AUM categories only. A
		// lending/oracle/bridge protocol is not a candidate for an AMM
		// headline, and the ones that carry a comparable figure (blend,
		// sorocredit, defindex, upshift) already carry their own
		// exclusion for a different reason. "yield" is included because
		// a vault's AUM is the same double-count/no-current-state-figure
		// shape a reader would otherwise expect summed here.
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
