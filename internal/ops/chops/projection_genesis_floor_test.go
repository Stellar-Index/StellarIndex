// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"testing"
)

// Band's shape: genesis is its first on-chain write, but the served tier's
// first oracle_updates row sits 9.16M ledgers later. The lake holds band
// writes in that prefix, so projection must fail rather than claim complete
// from the served tier's own MIN(ledger).
const (
	bandGenesis   = uint32(50_842_736)
	bandServedMin = uint32(60_000_414)
	bandTip       = uint32(64_000_000)
)

func TestScopesFromServed_UnprojectedPrefixFailsProjection(t *testing.T) {
	src := reconSource{name: "band", genesis: bandGenesis, targets: []reconTarget{{"oracle_updates", "source = 'band'", nil}}}
	served := []servedFloor{{min: bandServedMin, present: true}}

	scopes, servedFrom, runFrom := scopesFromServed(src, served, bandGenesis, bandGenesis, bandTip)
	if scopes[0].From != bandGenesis || servedFrom != bandGenesis {
		t.Fatalf("scope.From=%d servedFrom=%d, want genesis %d — flooring at the served MIN %d hides the never-projected prefix",
			scopes[0].From, servedFrom, bandGenesis, bandServedMin)
	}

	expected := map[uint32]int{51_000_000: 2, 61_000_000: 1}
	actual := map[uint32]int{61_000_000: 1}
	delta, detail := projectionDelta(src, "oracle_updates",
		clipCounts(expected, scopes[0]), clipCounts(actual, scopes[0]), scopes[0].From, scopes[0].To)
	if delta != 2 {
		t.Fatalf("Σ|Δ| = %d, want 2 (the prefix rows at ledger 51000000); detail=%q", delta, detail)
	}
	if ok, claim := projectionClaim(servedFrom, runFrom, bandTip, delta == 0, detail, priorProjection{}, testScope); ok {
		t.Fatalf("projection_ok=true over an unprojected prefix: %s", claim)
	}
}

func TestScopesFromServed_WindowedSourceKeepsServedFloor(t *testing.T) {
	const (
		genesis   = uint32(2)
		servedMin = uint32(61_609_957)
		tip       = uint32(64_000_000)
	)
	src := reconSource{name: "sdex", genesis: genesis, servedWindowReason: "working set", targets: []reconTarget{{"trades", sdexTradesFilter, nil}}}
	scopes, servedFrom, _ := scopesFromServed(src, []servedFloor{{min: servedMin, present: true}}, genesis, genesis, tip)
	if scopes[0].From != servedMin || servedFrom != servedMin {
		t.Fatalf("scope.From=%d servedFrom=%d, want the served MIN %d for a declared window", scopes[0].From, servedFrom, servedMin)
	}
	scopes, _, _ = scopesFromServed(src, []servedFloor{{}}, genesis, genesis, tip)
	if scopes[0].From != genesis {
		t.Fatalf("empty windowed target scope.From = %d, want genesis %d (fail closed)", scopes[0].From, genesis)
	}
}

func TestSourceProjectionFloor_ReverifiesNarrowPriorFromGenesis(t *testing.T) {
	band := reconSource{name: "band", genesis: bandGenesis}
	narrow := priorProjection{known: true, ok: true, tip: bandTip, verifiedFrom: bandServedMin}
	if got := sourceProjectionFloor(band, true, narrow, bandTip, 0); got != bandGenesis {
		t.Errorf("-pass floor = %d, want genesis %d: the narrow prior cannot be carried", got, bandGenesis)
	}
	wide := narrow
	wide.verifiedFrom = bandGenesis
	if got := sourceProjectionFloor(band, true, wide, bandTip, 0); got != bandTip {
		t.Errorf("-pass floor = %d, want watermark %d for a prior verified from genesis", got, bandTip)
	}
	sdex := reconSource{name: "sdex", genesis: 2, servedWindowReason: "working set"}
	if got := sourceProjectionFloor(sdex, true, priorProjection{known: true, ok: true, tip: bandTip, verifiedFrom: 61_609_957}, bandTip, 0); got != bandTip {
		t.Errorf("windowed source -pass floor = %d, want watermark %d", got, bandTip)
	}
	if got := sourceProjectionFloor(band, false, narrow, bandTip, 63_000_000); got != 63_000_000 {
		t.Errorf("outside -pass the operator's -from must stand, got %d", got)
	}
}

// Every catalogue source claims its served tier from genesis unless it is on
// this reviewed list. Adding a source here is a decision that the served tier
// deliberately omits part of its lake history.
func TestCatalogue_ServedWindowExemptionsReviewed(t *testing.T) {
	reviewed := map[string]bool{"sdex": true}

	cfg := testConfigWithAllSources()
	cfg.Supply.WatchedSEP41Contracts = testWatchedSEP41
	cat, _, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	seen := map[string]bool{}
	for _, src := range cat {
		seen[src.name] = true
		switch {
		case src.servedWindowReason != "" && !reviewed[src.name]:
			t.Errorf("%s floors its projection at the served MIN (%q) without review — an unprojected prefix would read complete", src.name, src.servedWindowReason)
		case src.servedWindowReason == "" && reviewed[src.name]:
			t.Errorf("%s is on the reviewed window list but carries no servedWindowReason", src.name)
		}
	}
	for name := range reviewed {
		if !seen[name] {
			t.Errorf("reviewed window source %s is not in the catalogue", name)
		}
	}
	if !seen["band"] {
		t.Fatal("band missing from the catalogue fixture")
	}
}
