// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package soroswap

import (
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// TestDecoder_GatedContractSetIsExactlyWhatMatchesAccepts pins the
// enumeration the completeness re-derive scopes its lake read to: every
// verified factory, every seeded pair and every pair a new_pair announced
// — and nothing Matches would reject. A narrower set would drop real
// events from the expected side (a false projection red); a wider one
// only costs read volume.
func TestDecoder_GatedContractSetIsExactlyWhatMatchesAccepts(t *testing.T) {
	token0 := makeContractStrkey(t, 0x10)
	token1 := makeContractStrkey(t, 0x11)
	seeded := makeContractStrkey(t, 0x20)
	announced := makeContractStrkey(t, 0x21)
	foreign := makeContractStrkey(t, 0x30)

	d := NewDecoder(WithSeededPairTokensDecoder(map[string]PairTokens{seeded: {}}))
	if _, err := d.Decode(makeNewPairEvent(t, token0, token1, announced)); err != nil {
		t.Fatalf("Decode new_pair: %v", err)
	}

	got := d.GatedContractSet()
	want := append(slices.Clone(MainnetFactories), seeded, announced)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("GatedContractSet = %v, want factories ∪ {seeded, announced} = %v", got, want)
	}

	swapFrom := func(c string) events.Event {
		return events.Event{Topic: []string{TopicPrefixPair, TopicSymbolSwap}, ContractID: c}
	}
	for _, c := range []string{seeded, announced} {
		if !d.Matches(swapFrom(c)) {
			t.Errorf("Matches rejects registered pair %s that GatedContractSet lists", c)
		}
	}
	if d.Matches(swapFrom(foreign)) || slices.Contains(got, foreign) {
		t.Errorf("foreign contract %s: Matches=%v, listed=%v — want neither", foreign,
			d.Matches(swapFrom(foreign)), slices.Contains(got, foreign))
	}
}
