// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"strings"
	"testing"
)

// A pooled-liquidity protocol may be summed into the headline total, or
// it may be left out — but it may never be left out SILENTLY.
//
// The failure this guards is the one sushiswap_v3 shipped with (#350).
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
		// Pooled-liquidity categories only. A lending/oracle/bridge
		// protocol is not a candidate for an AMM headline, and the ones
		// that carry a comparable figure (blend, sorocredit, defindex)
		// already carry their own exclusion for a different reason.
		if p.Category != "amm" && p.Category != "dex" {
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

// The concentrated-liquidity refusal must say what it actually is. The
// generic reason is reserved for a protocol that HAS an absolute reserve
// source whose readers a deployment did not wire — a config gap an
// operator can close. SushiSwap V3's is neither config nor temporary.
func TestDEXTVLNotDerivedReason_ConcentratedLiquidityIsNotBlamedOnWiring(t *testing.T) {
	t.Parallel()

	got := dexTVLNotDerivedReason("sushiswap_v3")
	if strings.Contains(got, "not wired on this deployment") {
		t.Fatalf("sushiswap_v3 refusal = %q; it reads as a deployment-wiring gap an operator "+
			"could close, but no wiring produces a two-sided reserve for a V3 pool", got)
	}
	if !strings.Contains(got, "concentrated liquidity") {
		t.Errorf("sushiswap_v3 refusal = %q; it must name concentrated liquidity as the reason "+
			"no reserve-derived figure exists", got)
	}
	// The rule this surface exists to keep: an underivable figure is
	// absent, never zero-filled.
	if strings.Contains(got, "0.00") {
		t.Errorf("sushiswap_v3 refusal = %q; an underivable figure must be absent, not a zero", got)
	}
}
