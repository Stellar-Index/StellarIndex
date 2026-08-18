package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// TestRecognitionCensus_SoroswapPairSeedRequired pins the fix for the soroswap
// recognition false-red: the recognition census builds its decoder chain via
// pipeline.BuildDispatcher, and unless the soroswap decoder is seeded with the
// pair registry its pairTokens map is empty — so Matches() rejects every real
// SoroswapPair protocol event (swap/sync/deposit/withdraw/skim), each becoming
// a false "unhandled topic" gap attributed to soroswap even though the indexer
// decodes + serves them. compute-completeness now threads
// WithSeededPairTokensDecoder(registry) into the census dispatcher (built from
// LoadSoroswapPairRegistry, the SAME set attribution folds into ownerOf).
//
// The test proves BOTH directions on the same event so it can't pass
// vacuously: unseeded MUST reject (reproduces the bug), seeded MUST recognize.
func TestRecognitionCensus_SoroswapPairSeedRequired(t *testing.T) {
	// A real registered soroswap pair contract (from the r1 pair registry).
	const pairID = "CB46LMGJC7SYSH4C7SBNLV635OX5BSNQDGRR32NRXAV7N2AVNZMQUJ3A"
	// A pair "sync" flow event — topic[0]=String("SoroswapPair"), topic[1]=Symbol("sync").
	pairEvent := events.Event{
		ContractID: pairID,
		Topic:      []string{soroswap.TopicPrefixPair, soroswap.TopicSymbolSync},
	}

	// Pre-fix: an UNSEEDED census dispatcher must NOT recognize the pair event.
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
