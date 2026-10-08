// Package redstone decodes on-chain events from the RedStone Adapter contract (one contract owning
// price storage for every feed, plus thin per-feed read proxies).
//
// Wire shape, verified against the public adapter source (redstone-adapter/src/event.rs):
//
//	topic[0] = Symbol("REDSTONE")
//	body     = Map {
//	              "updater":       Address,
//	              "updated_feeds": Vec<PriceData>,
//	           }
//	PriceData = Map {
//	              "price":             U256,
//	              "package_timestamp": u64,
//	              "write_timestamp":   u64,
//	           }
//
// The event carries no feed identifiers: they are write_prices' `feed_ids` op arg (events.Event.OpArgs),
// zipped one-to-one against updated_feeds. A freshness-filtered (shorter) updated_feeds is resolved by
// decode.go's resolveFeedAttribution; see docs/protocols/redstone.md.
package redstone

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is stamped on every OracleUpdate; one adapter contract covers all feeds.
const SourceName = "redstone"

// DefaultDecimals is the RedStone-wide price scale (redstone-price-feed/src/config.rs:1, verified
// against the deployed source); there is no per-feed scale.
const DefaultDecimals uint8 = 8

// DefaultResolutionSeconds is the 24h heartbeat (updates are `0.2% deviation OR 24h`), exported as
// the `stellarindex_oracle_resolution_seconds` gauge so the oracle-stale alert has a per-source threshold.
const DefaultResolutionSeconds = 24 * 60 * 60

// WriteFnName is the adapter's update entry point, the only path that emits a REDSTONE event. The
// function name is not plumbed, so four layers stand in for a name check:
//
//  1. the OpArgs PROVENANCE gate — args attach only when the invoked contract IS the event's
//     contract, so they describe a direct call INTO the adapter, never a wrapper's args;
//  2. structural validation of the signature (Address, Vec<String>, Bytes — feedIDsFromOpArgs);
//  3. the body-vs-args updater cross-check (decodeWritePrices);
//  4. state-write corroboration: plumbed value-changed keys must match the claimed feed set.
//
// A same-contract function passing all four would be a new WASM, caught by the per-WASM-hash audit gate.
const WriteFnName = "write_prices"

// Event-topic constants.
const (
	EventTopic0 = "REDSTONE"
)

// TopicSymbolRedstone is the pre-encoded base64 SCVal::Symbol for topic[0], for byte-equality matching.
var TopicSymbolRedstone = scval.MustEncodeSymbol(EventTopic0)

// Errors returned by the decode path.
var (
	// ErrNotRedstoneEvent — topic[0] is not "REDSTONE"; skip.
	ErrNotRedstoneEvent = errors.New("redstone: not a REDSTONE event")

	// ErrMalformedPayload — the body is not the WritePrices map shape.
	ErrMalformedPayload = errors.New("redstone: malformed event payload")

	// ErrEmptyUpdates — a non-empty batch decoded to no rows: every price non-positive or every
	// feed_id unrepresentable even as raw. An on-wire-empty batch (~1.5% of events) is a no-op, and an
	// unregistered feed_id is a raw:<feed_id> row; neither returns this.
	ErrEmptyUpdates = errors.New("redstone: empty updated_feeds vector")

	// ErrMissingOpArgs — no InvokeContract args attached, so there are no feed IDs to zip.
	ErrMissingOpArgs = errors.New("redstone: InvokeContract args unavailable")

	// ErrFeedIDCountMismatch — feed_ids and updated_feeds arity differ and no resolution layer
	// attributed the subset; the event is skipped rather than risk a BTC price on ETH.
	ErrFeedIDCountMismatch = errors.New("redstone: feed_ids arity doesn't match updated_feeds; cannot safely zip")

	// ErrDuplicateFeedIDs — feed_ids repeats a feed. redstone-core's Config::try_new rejects those
	// before emitting, so the args did NOT drive the call; a duplicate would also inflate the state-write
	// subset's arity and force the payload fallback on attacker-shaped candidates.
	ErrDuplicateFeedIDs = errors.New("redstone: duplicate feed_ids in op args")

	// ErrUpdaterMismatch — the body's updater disagrees with args[0]: write_prices publishes its own
	// updater, so the args belong to another call. Refuse rather than attribute.
	ErrUpdaterMismatch = errors.New("redstone: event-body updater disagrees with op-args updater")

	// ErrStateWriteFeedMismatch — the op's value-changed state writes name a different feed set than
	// an equal-arity feed_ids (every accepted feed's entry changes), or contradict a subset's payload-median
	// fallback (payload.go F1 CAVEAT). Returned only when the fallback also fails; same honest-blind arm as ErrAmbiguousSubset.
	ErrStateWriteFeedMismatch = errors.New("redstone: op-args feed_ids disagree with the op's value-changed state-write feed set")

	// ErrEventIndexOverflow — e.EventIndex >= eventFanoutStride would spill into the next operation's
	// OpIndex range: a decoder bug or far more events per op than observed.
	ErrEventIndexOverflow = errors.New("redstone: EventIndex exceeds OpIndex fanout stride")

	// ErrOperationIndexOverflow — e.OperationIndex is negative or would wrap the uint32 OpIndex packing
	// into another event's block on the oracle_updates PK. Unreachable on-chain; a hit is a dispatcher bug.
	ErrOperationIndexOverflow = errors.New("redstone: OperationIndex exceeds OpIndex fanout bound")
)
