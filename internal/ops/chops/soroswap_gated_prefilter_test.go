// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// TestCatalogue_SoroswapReDeriveIsContractScoped (#805): soroswap is the
// first catalogue source, and a failing prior projection verdict re-floors it
// at genesis (CS-095). With no contract scope that re-derive streamed every
// contract event from genesis to tip. The entry must scope the lake read to
// the soroswap gate — every factory plus every pair the (RPC-seeded) decoder
// holds — and exclude a look-alike emitting the same topics.
func TestCatalogue_SoroswapReDeriveIsContractScoped(t *testing.T) {
	cat, soroswapDec, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	src := catalogueSource(t, cat, "soroswap")
	if !slices.Equal(src.factories, soroswap.MainnetFactories) || src.creationSym != soroswap.PrefixFactory {
		t.Fatalf("soroswap factories=%v creationSym=%q, want every verified factory and %q",
			src.factories, src.creationSym, soroswap.PrefixFactory)
	}
	if src.newGatedDec == nil {
		t.Fatal("soroswap has no newGatedDec: its -ch re-derive streams the whole lake unfiltered")
	}

	const pairContract = "CSOROSWAPSEEDEDPAIRAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	const foreignContract = "CFOREIGNSWAPLOOKALIKEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	// The pass seeds this same decoder from the factory before the loop.
	soroswapDec.SeedPair(pairContract, canonical.Asset{}, canonical.Asset{})

	swap := func(c string) events.Event {
		return events.Event{Ledger: src.genesis + 1, ContractID: c, Topic: []string{soroswap.TopicPrefixPair, soroswap.TopicSymbolSwap}}
	}
	lake := countingEventStreamer{evs: []events.Event{swap(pairContract), swap(foreignContract)}}
	pf, blind, err := gatedPrefilter(context.Background(), lake, src, src.genesis+10)
	if err != nil {
		t.Fatalf("gatedPrefilter: %v", err)
	}
	if blind.Any() {
		t.Fatalf("unexpected blind spots: %v", blind)
	}
	want := append(slices.Clone(soroswap.MainnetFactories), pairContract)
	slices.Sort(want)
	if !slices.Equal(pf, want) {
		t.Errorf("soroswap prefilter = %v, want factories ∪ {seeded pair} = %v", pf, want)
	}
	if slices.Contains(pf, foreignContract) {
		t.Errorf("soroswap prefilter admits the look-alike %s", foreignContract)
	}
}
