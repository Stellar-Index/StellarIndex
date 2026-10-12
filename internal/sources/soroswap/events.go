// Package soroswap ingests trade events from the Soroswap Soroban DEX.
//
// Design reference: internal/sources/soroswap/README.md and docs/protocols/soroswap.md; read the
// Q1-Q6 quirk notes before modifying the correlation logic.
package soroswap

import (
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the stable source name used in metrics labels, canonical.Trade.Source and config.IngestionConfig.EnabledSources.
const SourceName = "soroswap"

// Event names: topic[1] of every pair/factory event is a Symbol SCVal with one of these values
// (topic[0] is the prefix String, see Prefix* below). Each publish in soroswap-core
// (pair/src/event.rs, factory/src/event.rs) is a 2-tuple of ScvString prefix and ScvSymbol name.
const (
	EventSwap     = "swap"
	EventSync     = "sync"
	EventDeposit  = "deposit"
	EventWithdraw = "withdraw"
	EventSkim     = "skim"

	// Emitted by the factory contract.
	EventNewPair = "new_pair"

	// EventPairToken is the classify() kind for the pair's LP-share SEP-41 events (`transfer`,
	// `mint`, `burn`, `approve`), which carry Symbol topic[0] rather than the "SoroswapPair"
	// String prefix. The Decoder deliberately does NOT claim them: see [Decoder.Matches].
	EventPairToken = "pair_token"
)

// Topic-prefix string values (topic[0]). The prefix slot is a String SCVal, NOT a Symbol, because
// the contracts publish `("SoroswapPair", symbol_short!("swap"))` with a string literal first.
const (
	PrefixPair    = "SoroswapPair"
	PrefixFactory = "SoroswapFactory"
	PrefixRouter  = "SoroswapRouter"
)

// Mainnet contract addresses — verified during Phase-1 audit against
// public/mainnet.contracts.json in soroswap-core.
const (
	// MainnetFactory is the current/primary Soroswap factory — the only
	// one that has deployed pairs with swap activity.
	MainnetFactory = "CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2"
	MainnetRouter  = "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH"

	// MainnetPairWASMHash identifies pair contracts by WASM hash instead of walking factory events.
	MainnetPairWASMHash = "18051456816b66f12e773a56f77c5794fac1b1fb7ab6e22d4fad5a412770f73e"
)

// MainnetFactories is the COMPLETE, lake-verified set of Soroswap factories on mainnet (ADR-0035), including three early factories whose pairs never swapped, so the `new_pair` gate honors every factory; re-run the `SoroswapFactory:new_pair` enumeration if a new one appears.
var MainnetFactories = []string{
	MainnetFactory,
	"CCIQM2O3YJQEKS7I77AS5IO3CU6UCBAUWHLWRBWVV336ZCSTKRNBKPHW", // early, 11 pairs
	"CDBRTEJMOUJQHFZCAW4JPXZ75HCRHZAQXG75ZMGQ2LMNXA5ID7RQIFSX", // early, 6 pairs
	"CCDATRT2EY6Y2KAZ7HM7BRZVZCB6RHL56PQUBWGBS2ML2JAK7VXFLCJY", // early, 4 pairs
}

// IsMainnetFactory reports whether contractID is a verified Soroswap factory, the trust-root set the new_pair gate consults.
func IsMainnetFactory(contractID string) bool {
	for _, f := range MainnetFactories {
		if f == contractID {
			return true
		}
	}
	return false
}

// Pre-encoded base64 SCVal blobs, byte-identical to what the contract emits on topic positions, so
// classify() compares bytes with no SCVal parse on the hot path. The golden wire-format test
// (TestGolden_symbolBytes in internal/scval/scval_test.go) fires if the SDK encoder shifts.
var (
	TopicPrefixPair    = scval.MustEncodeString(PrefixPair)    // topic[0] for pair events
	TopicPrefixFactory = scval.MustEncodeString(PrefixFactory) // topic[0] for factory events

	TopicSymbolSwap     = scval.MustEncodeSymbol(EventSwap)     // topic[1]
	TopicSymbolSync     = scval.MustEncodeSymbol(EventSync)     // topic[1]
	TopicSymbolDeposit  = scval.MustEncodeSymbol(EventDeposit)  // topic[1]
	TopicSymbolWithdraw = scval.MustEncodeSymbol(EventWithdraw) // topic[1]
	TopicSymbolSkim     = scval.MustEncodeSymbol(EventSkim)     // topic[1]
	TopicSymbolNewPair  = scval.MustEncodeSymbol(EventNewPair)  // topic[1] on factory

	// LP-share (pair-token) SEP-41 event names — topic[0] Symbols, not
	// the String protocol prefix. See EventPairToken.
	TopicSymbolLPTransfer = scval.MustEncodeSymbol("transfer") // topic[0]
	TopicSymbolLPMint     = scval.MustEncodeSymbol("mint")     // topic[0]
	TopicSymbolLPBurn     = scval.MustEncodeSymbol("burn")     // topic[0]
	TopicSymbolLPApprove  = scval.MustEncodeSymbol("approve")  // topic[0]
)

// Errors returned by the decode path. Callers classify via
// errors.Is.
var (
	// ErrUnknownEvent — topic[1] didn't match any of the event
	// names we care about. Most events fall into this class
	// (trades/sync we care about; others we ignore).
	ErrUnknownEvent = errors.New("soroswap: unknown event topic")

	// ErrSwapWithoutSync - a swap with no following sync. The dispatcher feeds events in order, so
	// surfacing this means a bug or a truncated event stream.
	ErrSwapWithoutSync = errors.New("soroswap: swap without sync")

	// ErrMalformedPayload — event fields don't match the expected
	// Soroswap schema (arity, types, contract).
	ErrMalformedPayload = errors.New("soroswap: malformed event payload")

	// ErrNonDirectionalSwap - the four swap amounts decoded but carry NO cross-token exchange:
	// neither (0_in>0 && 1_out>0) nor (1_in>0 && 0_out>0) holds. pair.swap() is directly invokable
	// and any mix that keeps K non-decreasing succeeds. It is a real event but not a trade, so
	// [Decoder.Decode] treats it as a RECOGNIZED NO-OP (zero rows, nil error); a decode error
	// would leave the ADR-0033 completeness re-derive blind on that ledger.
	ErrNonDirectionalSwap = errors.New("soroswap: non-directional swap (no cross-token exchange)")

	// ErrAmbiguousSwapDirection - THREE OR MORE of the four amounts are non-zero (overlapping in/out
	// legs), so more than one direction arm holds. Taking the first would report a GROSS `in` and
	// fabricate a price into VWAP/OHLC, so the whole event is refused. It wraps
	// ErrNonDirectionalSwap so the recognized-no-op handling in [Decoder.Decode] covers it.
	ErrAmbiguousSwapDirection = fmt.Errorf("%w: ambiguous direction (overlapping in/out legs)", ErrNonDirectionalSwap)
)
