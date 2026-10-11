package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// TestRecognitionCensus_SoroswapPairSeedRequired pins that the recognition census seeds the
// soroswap decoder with the pair registry. Unseeded, pairTokens is empty and Matches() rejects
// every real SoroswapPair event, each becoming a false "unhandled topic" gap. The census
// dispatcher gets WithSeededPairTokensDecoder(registry) from LoadSoroswapPairRegistry, the
// SAME set attribution folds into ownerOf.
//
// Both directions run on the same event so it cannot pass vacuously: unseeded MUST reject,
// seeded MUST recognize.
func TestRecognitionCensus_SoroswapPairSeedRequired(t *testing.T) {
	// A real registered soroswap pair contract (from the r1 pair registry).
	const pairID = "CB46LMGJC7SYSH4C7SBNLV635OX5BSNQDGRR32NRXAV7N2AVNZMQUJ3A"
	// A pair "sync" flow event — topic[0]=String("SoroswapPair"), topic[1]=Symbol("sync").
	pairEvent := events.Event{
		ContractID: pairID,
		Topic:      []string{soroswap.TopicPrefixPair, soroswap.TopicSymbolSync},
	}

	// Control: an UNSEEDED census dispatcher must NOT recognize the pair event.
	unseeded, err := pipeline.BuildDispatcher([]string{soroswap.SourceName}, config.OracleConfig{}, nil)
	if err != nil {
		t.Fatalf("build unseeded dispatcher: %v", err)
	}
	if _, ok := unseeded.Recognize(pairEvent); ok {
		t.Fatal("unseeded census recognized a soroswap pair event — the bug is not reproduced; " +
			"this test would pass vacuously and prove nothing")
	}

	// The fix: a census dispatcher SEEDED with the pair registry recognizes it.
	seed := map[string]soroswap.PairTokens{pairID: {}} // Matches() needs only key presence
	seeded, err := pipeline.BuildDispatcher(
		[]string{soroswap.SourceName}, config.OracleConfig{}, nil,
		soroswap.WithSeededPairTokensDecoder(seed),
	)
	if err != nil {
		t.Fatalf("build seeded dispatcher: %v", err)
	}
	if _, ok := seeded.Recognize(pairEvent); !ok {
		t.Fatal("seeded census did NOT recognize a real soroswap pair event — the fix is ineffective")
	}
}
