// Package soroswap ingests trade events from the Soroswap Soroban DEX.
//
// Design reference: internal/sources/soroswap/README.md and
// docs/discovery/dexes-amms/soroswap.md. See especially the Q1–Q4
// quirk notes in the README before modifying the correlation logic.
package soroswap

import "errors"

// Source name constant — appears in metrics labels, canonical.Trade.Source,
// and config.IngestionConfig.EnabledSources. Must be stable.
const SourceName = "soroswap"

// Event names — the first topic of every Soroswap pair/factory event
// is a Symbol SCVal with one of these literal values.
const (
	EventSwap     = "swap"
	EventSync     = "sync"
	EventDeposit  = "deposit"
	EventWithdraw = "withdraw"
	EventSkim     = "skim"

	// Emitted by the factory contract.
	EventNewPair = "new_pair"
)

// Mainnet contract addresses — verified during Phase-1 audit against
// public/mainnet.contracts.json in soroswap-core.
const (
	MainnetFactory = "CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2"
	MainnetRouter  = "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH"

	// MainnetPairWASMHash lets us identify Soroswap pair contracts by
	// hashing their wasm rather than walking factory events.
	// Useful for backfill short-cuts.
	MainnetPairWASMHash = "18051456816b66f12e773a56f77c5794fac1b1fb7ab6e22d4fad5a412770f73e"
)

// Pre-encoded base64 SCVal::Symbol("<event>") blobs — these are
// what stellar-rpc emits in event.topic[0] and what we'll pass as
// the filter topic to subscribe server-side. Pre-computing them
// avoids round-tripping every event through an XDR decoder just to
// classify it.
//
// These are intentionally **placeholder strings** until the real
// XDR-SCVal encoder lands (see TODO(#0)). Matching against a
// placeholder never produces a match against a real event stream
// — which is fine: the consumer is unit-tested with these placeholders,
// and wiring to real stellar-rpc is blocked on the same SDK dependency
// decision that unblocks proper SCVal encoding.
//
// Regeneration (future): `stellar-cli ... scval encode <SYMBOL>` or
// via the go-stellar-sdk's `xdr.NewSymbol`.
//
// Uniqueness is enforced by the compiler — a `switch` with duplicate
// `case` values fails to compile. That's our canary against
// accidentally setting two placeholders to the same value.
const (
	TopicSymbolSwap     = "PLACEHOLDER_TOPIC_SYMBOL_SWAP"      // TODO(#0): replace with real SCVal blob
	TopicSymbolSync     = "PLACEHOLDER_TOPIC_SYMBOL_SYNC"      // TODO(#0)
	TopicSymbolDeposit  = "PLACEHOLDER_TOPIC_SYMBOL_DEPOSIT"   // TODO(#0)
	TopicSymbolWithdraw = "PLACEHOLDER_TOPIC_SYMBOL_WITHDRAW"  // TODO(#0)
	TopicSymbolNewPair  = "PLACEHOLDER_TOPIC_SYMBOL_NEW_PAIR"  // TODO(#0)
)

// Errors returned by the decode path. Callers classify via
// errors.Is.
var (
	// ErrUnknownEvent — topic[0] didn't match any of the event
	// names we care about. Most events fall into this class
	// (trades/sync we care about; others we ignore).
	ErrUnknownEvent = errors.New("soroswap: unknown event topic")

	// ErrOrphanSync — a sync event with no preceding swap in the
	// same (ledger, tx_hash, op_index). Not a trade; drop.
	ErrOrphanSync = errors.New("soroswap: orphan sync (no matching swap)")

	// ErrSwapWithoutSync — a swap that didn't get its following
	// sync. Could happen if the sync is in a later RPC page; the
	// consumer's buffer should have caught this. Bug or truncation.
	ErrSwapWithoutSync = errors.New("soroswap: swap without sync")

	// ErrMalformedPayload — event fields don't match the expected
	// Soroswap schema (arity, types, contract).
	ErrMalformedPayload = errors.New("soroswap: malformed event payload")
)
