// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Folding the two stored directions into the STORE changed what a list
// of pairs means to a caller that merges it.
//
// Before the fold, `A/B` and `B/A` were two different reads returning
// disjoint rows. After it they are one read returning the same rows,
// differing only in the orientation the answer arrives in. Every
// serving path that concatenates several pairs' trades into a single
// population — the tip window's alias merge, the fiat point combine —
// therefore has to ask for each MARKET once, or every trade lands in
// the mean twice, in two orientations.
//
// Both legs alias, so a caller only has to name two spellings of one
// asset family to produce a pair and its flip in the same set.

func mustAsset(t *testing.T, s string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(s)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", s, err)
	}
	return a
}

// TestTipMergePairsAsksForEachMarketOnce is the reachable case:
// /v1/price/tip?asset=native&quote=crypto:XLM. Both legs expand to the
// SAME alias family, so the cross emits native/crypto:XLM AND
// crypto:XLM/native — which tipWindowVWAP concatenates into one
// aggregate.VWAP call.
func TestTipMergePairsAsksForEachMarketOnce(t *testing.T) {
	native := mustAsset(t, "native")
	cryptoXLM := mustAsset(t, "crypto:XLM")

	merge, last := tipMergePairs(native, cryptoXLM)

	for _, set := range []struct {
		name  string
		pairs []canonical.Pair
	}{
		{"merge", merge},
		{"last", last},
	} {
		for i := range set.pairs {
			for j := i + 1; j < len(set.pairs); j++ {
				if set.pairs[i].EqualEitherWay(set.pairs[j]) {
					t.Errorf("tipMergePairs %s set holds %s/%s and %s/%s — the same "+
						"market twice.\n"+
						"The store serves a market whichever way round it is asked "+
						"for, so both reads return the SAME rows, one set of them "+
						"inverted. tipWindowVWAP concatenates the set and takes one "+
						"Σquote/Σbase over it, so every trade is counted twice and "+
						"the two orientations are summed together.",
						set.name,
						set.pairs[i].Base, set.pairs[i].Quote,
						set.pairs[j].Base, set.pairs[j].Quote)
				}
			}
		}
	}

	// The dedupe must not cost coverage: the market itself is still
	// read, in whichever spelling the walk reached first.
	if len(merge) == 0 {
		t.Fatal("merge set is empty — deduplicating markets must drop a " +
			"redundant SPELLING, never the market")
	}
	if !merge[0].Base.Equal(native) || !merge[0].Quote.Equal(cryptoXLM) {
		t.Errorf("first merged pair is %s/%s, want native/crypto:XLM — the "+
			"survivor is the spelling the walk reached first, so the alias "+
			"family's priority order still decides",
			merge[0].Base, merge[0].Quote)
	}
}

// TestDistinctMarketsKeepsOrderAndFirstSpelling pins the two properties
// every caller depends on: the SAC-last / classic-first orderings the
// alias family documents survive the dedupe, and a set with nothing to
// drop comes back unchanged.
func TestDistinctMarketsKeepsOrderAndFirstSpelling(t *testing.T) {
	a := mustAsset(t, "native")
	b := mustAsset(t, "crypto:XLM")
	c := mustAsset(t, "fiat:USD")

	mk := func(base, quote canonical.Asset) canonical.Pair {
		t.Helper()
		p, err := canonical.NewPair(base, quote)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		return p
	}

	in := []canonical.Pair{mk(a, c), mk(a, b), mk(c, a), mk(b, a), mk(b, c)}
	got := distinctMarkets(in)

	want := []canonical.Pair{mk(a, c), mk(a, b), mk(b, c)}
	if len(got) != len(want) {
		t.Fatalf("kept %d markets, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("position %d is %s/%s, want %s/%s — the dedupe drops the "+
				"LATER spelling and never reorders what it keeps",
				i, got[i].Base, got[i].Quote, want[i].Base, want[i].Quote)
		}
	}

	// Nothing to drop: same slice, same order.
	clean := []canonical.Pair{mk(a, c), mk(b, c)}
	if out := distinctMarkets(clean); len(out) != 2 ||
		!out[0].Equal(clean[0]) || !out[1].Equal(clean[1]) {
		t.Errorf("a set with no flip in it changed: %v", out)
	}
}

// TestUSDPeggedConstituentsAskForEachMarketOnce is the same invariant
// on the fiat combine. It feeds three readers at once — the point
// path's trade merge, the series' per-bucket combine and the coverage
// probe — and all three now read both stored directions, so a
// constituent appearing as its own flip would be summed into one
// bucket twice while the probe promised coverage for both spellings.
//
// This one holds today by accident rather than by construction: the peg
// expansion emits classic quote spellings only
// (aggregate.ExpandTargetPairWithClassicPegs skips a non-classic), so a
// base alias can appear as a peg quote but never the reverse. Widening
// the quote leg to a peg's SAC form — launch-plan row 1.14 — removes
// exactly that accident, and this test is what turns the property
// structural before that happens.
func TestUSDPeggedConstituentsAskForEachMarketOnce(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC",
		"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset USDC: %v", err)
	}
	srv := &Server{usdPeggedClassics: []canonical.Asset{usdc}}

	for _, base := range []string{"native", "crypto:XLM", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"} {
		target, err := canonical.NewPair(mustAsset(t, base), mustAsset(t, "fiat:USD"))
		if err != nil {
			t.Fatalf("NewPair(%s, fiat:USD): %v", base, err)
		}
		assertDistinctMarkets(t, base, srv.usdPeggedConstituents(target))
	}
}

// assertDistinctMarkets fails when any two entries name one market.
func assertDistinctMarkets(t *testing.T, who string, pairs []canonical.Pair) {
	t.Helper()
	if len(pairs) == 0 {
		t.Fatalf("%s: no constituents — the expansion no longer reaches a fiat quote", who)
	}
	for i := range pairs {
		for j := i + 1; j < len(pairs); j++ {
			if pairs[i].EqualEitherWay(pairs[j]) {
				t.Errorf("%s: constituent set holds %s/%s and %s/%s — the same "+
					"market twice, and every one of its trades would be merged "+
					"into the combined bar twice",
					who, pairs[i].Base, pairs[i].Quote, pairs[j].Base, pairs[j].Quote)
			}
		}
	}
}
