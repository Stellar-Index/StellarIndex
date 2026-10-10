package sushiswap_v3

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Compile-time proof the Decoder satisfies the dispatcher seam it is
// registered on.
var _ dispatcher.Decoder = (*Decoder)(nil)

// foreignContract is a real pubnet contract that is not a SushiSwap V3
// pool (the Soroswap factory). Standing in for "any other contract".
const foreignContract = "CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2"

func swapEvent(contractID, body string, ledger uint32, opIndex, eventIndex int) events.Event {
	return events.Event{
		Type:                     "contract",
		Ledger:                   ledger,
		LedgerClosedAt:           "2026-08-30T22:22:35Z",
		ContractID:               contractID,
		TxHash:                   swapTxHash,
		OperationIndex:           opIndex,
		EventIndex:               eventIndex,
		InSuccessfulContractCall: true,
		Topic:                    []string{TopicSymbolSwap},
		Value:                    body,
	}
}

func poolCreatedEvent(contractID, body string, ledger uint32) events.Event {
	return events.Event{
		Type:           "contract",
		Ledger:         ledger,
		LedgerClosedAt: "2026-03-03T20:51:59Z",
		ContractID:     contractID,
		TxHash:         "01d797bcc72dbde6664305ae30345e07c08f5a09cabd7fcf2a5e10e5b6b84765",
		Topic:          []string{TopicSymbolPoolCreated},
		Value:          body,
	}
}

// TestMatches_ForeignContractEmittingOurTopicsIsRejected is the ADR-0035
// gate, and it is the load-bearing test of this package.
//
// Every event in this protocol carries a ONE-element Symbol topic, and the
// symbols are the most generic on the network: a bounded 10k-ledger census
// of pubnet puts `mint` at 33% and `burn` at 12% of ALL contract events. A
// topic-only decoder would therefore attribute a large slice of the
// network's token traffic to this source. Replace [Decoder.Matches]'s
// identity check with a topic check and every case below flips to true.
func TestMatches_ForeignContractEmittingOurTopicsIsRejected(t *testing.T) {
	d := NewDecoder()
	for _, topic := range []struct {
		name  string
		topic string
	}{
		{EventSwap, TopicSymbolSwap},
		{EventMint, TopicSymbolMint},
		{EventBurn, TopicSymbolBurn},
		{EventCollect, TopicSymbolCollect},
		{EventInit, TopicSymbolInit},
		{EventUpgraded, TopicSymbolUpgraded},
		{EventMigrated, TopicSymbolMigrated},
	} {
		t.Run(topic.name, func(t *testing.T) {
			ev := events.Event{
				ContractID: foreignContract,
				Topic:      []string{topic.topic},
				Value:      goldenSwapSellToken0,
			}
			if d.Matches(ev) {
				t.Fatalf("a foreign contract emitting %q was claimed as SushiSwap V3", topic.name)
			}
		})
	}
}

// TestMatches_ForeignFactoryCannotSeedTheRegistry is the registry-poisoning
// case. If any contract could announce a pool, it could name token
// identities of its own choosing, register itself, and then have its own
// swaps recorded as real trades at prices it controls.
func TestMatches_ForeignFactoryCannotSeedTheRegistry(t *testing.T) {
	d := NewDecoder()
	ev := poolCreatedEvent(foreignContract, goldenPoolCreated, 61_487_379)
	if d.Matches(ev) {
		t.Fatal("pool_created from a non-factory contract was accepted")
	}

	// Even if the event were fed to Decode directly, nothing may be
	// registered: Matches is the gate, but the fan-out must not widen it.
	if _, err := d.Decode(ev); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	// The pool named in the body is a REAL pool, so it is already in the
	// curated seed; the check that matters is that a pool NOT in the seed
	// stays unregistered.
	unseeded := events.Event{
		ContractID: "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3",
		Topic:      []string{TopicSymbolSwap},
		Value:      goldenSwapSellToken0,
	}
	if d.Matches(unseeded) {
		t.Fatal("an unregistered contract was claimed")
	}
}

// TestMatches_CanonicalFactoryAndRegisteredPoolsAreAccepted is the
// positive half of the gate.
func TestMatches_CanonicalFactoryAndRegisteredPoolsAreAccepted(t *testing.T) {
	d := NewDecoder()
	if !d.Matches(poolCreatedEvent(MainnetFactory, goldenPoolCreated, 61_487_379)) {
		t.Error("pool_created from the canonical factory was rejected")
	}
	if !d.Matches(swapEvent(mainPool, goldenSwapSellToken0, 64_200_014, 0, 2)) {
		t.Error("swap from a curated pool was rejected")
	}
	for _, pool := range MainnetGatedSet() {
		ev := swapEvent(pool, goldenSwapSellToken0, 64_200_014, 0, 2)
		if !d.Matches(ev) {
			t.Errorf("swap from curated pool %s was rejected", pool)
		}
	}
}

// TestMatches_UnrelatedTopicFromARegisteredPoolIsNotClaimed keeps the gate
// from becoming a blanket claim on a pool contract. A registered pool that
// also emits, say, a SEP-41 `transfer` must leave that event for the
// source that owns it.
func TestMatches_UnrelatedTopicFromARegisteredPoolIsNotClaimed(t *testing.T) {
	d := NewDecoder()
	ev := events.Event{
		ContractID: mainPool,
		Topic:      []string{scval.MustEncodeSymbol("transfer")},
		Value:      goldenSwapSellToken0,
	}
	if d.Matches(ev) {
		t.Fatal("an unrelated topic from a registered pool was claimed")
	}
}

// TestSeedPool_AdmitsAPoolAndItsTokens covers the operator seam.
func TestSeedPool_AdmitsAPoolAndItsTokens(t *testing.T) {
	const pool = "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3"
	d := NewDecoder()
	if d.Matches(swapEvent(pool, goldenSwapSellToken0, 64_200_014, 0, 2)) {
		t.Fatal("the pool was gated in before being seeded")
	}
	d.SeedPool(pool, mustAsset(t, tokenXLM), mustAsset(t, tokenUSDC), MainnetFactory, 64_000_000)
	if !d.Matches(swapEvent(pool, goldenSwapSellToken0, 64_200_014, 0, 2)) {
		t.Fatal("the seeded pool was not gated in")
	}
	out, err := d.Decode(swapEvent(pool, goldenSwapSellToken0, 64_200_014, 0, 2))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
}

// TestGatedContractSet_IsTheProjectorPrefilter proves the prefilter the
// projector installs covers the factory AND every pool. Missing the
// factory would stop new pools ever registering; missing a pool would drop
// its trades.
func TestGatedContractSet_IsTheProjectorPrefilter(t *testing.T) {
	d := NewDecoder()
	set := d.GatedContractSet()
	seen := make(map[string]struct{}, len(set))
	for _, id := range set {
		seen[id] = struct{}{}
	}
	if _, ok := seen[MainnetFactory]; !ok {
		t.Error("the prefilter omits the factory — no new pool could ever register")
	}
	for pool := range MainnetPools {
		if _, ok := seen[pool]; !ok {
			t.Errorf("the prefilter omits curated pool %s", pool)
		}
	}
	if len(set) != len(MainnetPools)+len(MainnetFactories) {
		t.Errorf("prefilter has %d entries, want %d", len(set), len(MainnetPools)+len(MainnetFactories))
	}
}

// TestDecoderName is the cursor / metrics label contract.
func TestDecoderName(t *testing.T) {
	if got := NewDecoder().Name(); got != SourceName {
		t.Fatalf("Name = %s, want %s", got, SourceName)
	}
}
