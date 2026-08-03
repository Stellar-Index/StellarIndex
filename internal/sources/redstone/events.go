// Package redstone decodes on-chain events from the RedStone
// Adapter contract (one contract that owns price storage for every
// feed + thin per-feed proxies that delegate reads).
//
// Wire shape, verified 2026-04-23 against the public adapter source
// (.discovery-repos/redstone-public-contracts/packages/
// stellar-connector/deployments/stellarMultiFeed/contracts/
// redstone-adapter/src/event.rs):
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
// The event carries prices + timestamps but NOT feed identifiers.
// Feed IDs live in the InvokeContract op args — the relayer calls
// `adapter.write_prices(updater, feed_ids: Vec<String>, payload)`.
// Our dispatcher surfaces those args via events.Event.OpArgs; the
// decoder zips `feed_ids` against `updated_feeds` one-to-one.
//
// Caveat: when the adapter's freshness verifier rejects a feed, it
// skips that entry in `updated_feeds` without skipping in
// `feed_ids`. We guard against this with a strict length check and
// surface ErrFeedIDCountMismatch if they disagree — a rare on-chain
// state we'd rather skip than attribute prices to the wrong assets.
// See docs/discovery/oracles/redstone.md for the full analysis.
package redstone

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the canonical string stamped on every OracleUpdate
// this package emits. Single source — unlike Reflector (3 variants),
// Redstone has one adapter contract covering all feeds.
const SourceName = "redstone"

// DefaultDecimals is the RedStone-wide price scale
// (redstone-price-feed/src/config.rs:1 — `pub const DECIMALS: u64 = 8`,
// a single constant shared by every per-feed proxy; verified against the
// deployed source 2026-08-03). Every feed publishes at 8 decimals
// regardless of the underlying asset class — there is no per-feed scale.
const DefaultDecimals uint8 = 8

// DefaultResolutionSeconds reflects the on-chain update cadence:
// `0.2% deviation OR 24h heartbeat`. Emitted as the
// `stellarindex_oracle_resolution_seconds` gauge by
// [pipeline.BuildDispatcher] at registration time, so the
// oracle-stale alert has a per-source threshold. Set to 24h (the
// lower bound on assumed freshness — per
// docs/discovery/oracles/redstone.md a feed may go quiet for up
// to 24h if no price movement exceeds the 0.2% deviation
// threshold).
const DefaultResolutionSeconds = 24 * 60 * 60

// WriteFnName is the adapter contract's update entry point and the
// only path that emits a REDSTONE-topic event. The dispatcher plumbs
// only the InvokeContract Args slice (not the function name), so the
// decoder cannot assert the call targeted this function BY NAME.
// What stands in for the literal name check, layered (2026-07-31
// hardening):
//
//  1. the dispatcher/lake OpArgs PROVENANCE gate — args are attached
//     only when the op's invoked contract IS the event's own contract,
//     so the args describe a direct call INTO the adapter, never a
//     wrapper's free-chosen top-level args;
//  2. structural validation of the write_prices signature
//     (Address, Vec<String>, Bytes — feedIDsFromOpArgs);
//  3. the body-vs-args updater cross-check (the event's `updater`
//     field must equal args[0] — decodeWritePrices);
//  4. the state-write corroboration: when the op's value-changed
//     contract-data keys are plumbed, the claimed feed set must match
//     the actually-written feed set (resolveFeedAttribution).
//
// A same-contract non-write_prices function that passes all four while
// emitting a REDSTONE event would be a new adapter WASM — covered by
// docs/architecture/contract-schema-evolution.md's per-WASM-hash audit
// gate. Kept as documentation of the invariant this layering leans on.
const WriteFnName = "write_prices"

// Event-topic constants.
const (
	EventTopic0 = "REDSTONE"
)

// TopicSymbolRedstone is the pre-encoded base64 SCVal::Symbol blob
// for topic[0]. Produced at init via scval.MustEncodeSymbol and
// used for byte-equality matching against Event.Topic entries.
var TopicSymbolRedstone = scval.MustEncodeSymbol(EventTopic0)

// Errors returned by the decode path.
var (
	// ErrNotRedstoneEvent — topic[0] doesn't match "REDSTONE".
	// Skip: this decoder owns only one topic.
	ErrNotRedstoneEvent = errors.New("redstone: not a REDSTONE event")

	// ErrMalformedPayload — event body doesn't decode to the
	// expected WritePrices map shape.
	ErrMalformedPayload = errors.New("redstone: malformed event payload")

	// ErrEmptyUpdates — every decoded entry was dropped by the
	// registry filter (all-unknown-symbol batches), so the caller
	// gets a loud signal instead of a silently empty result.
	//
	// NOTE (2026-07-29): this error NO LONGER covers on-wire-empty
	// `updated_feeds` vectors. The old doc claimed "the adapter only
	// emits when at least one feed passes the freshness check";
	// the lake disproves that — ~1.5% of all REDSTONE events ever
	// are `{updated_feeds: [], updater}` no-op pushes, in every
	// ledger band since source genesis. Those now decode to zero
	// updates with NO error (see decodeWritePrices).
	ErrEmptyUpdates = errors.New("redstone: empty updated_feeds vector")

	// ErrMissingOpArgs — the event arrived without InvokeContract
	// args attached. Either the producing tx wasn't an
	// InvokeContract (unexpected — write_prices is the only emit
	// path), or the dispatcher failed to populate them. Without
	// args we have no feed IDs to zip.
	ErrMissingOpArgs = errors.New("redstone: InvokeContract args unavailable")

	// ErrFeedIDCountMismatch — len(feed_ids from args) != len(
	// updated_feeds from event body). Happens when the adapter's
	// freshness verifier rejects one or more submitted feeds; in
	// that case we can't safely attribute the remaining prices to
	// specific feeds. Skip the whole event rather than risk
	// assigning a BTC price to ETH.
	ErrFeedIDCountMismatch = errors.New("redstone: feed_ids arity doesn't match updated_feeds; cannot safely zip")

	// ErrDuplicateFeedIDs — the op-args feed_ids vector contains a
	// repeated feed. A genuine write_prices can never carry one: the
	// redstone-core SDK's Config::try_new rejects duplicated feed_ids
	// outright (check_no_duplicates → Error::ConfigReoccurringFeedId,
	// .discovery-repos/redstone-rust-sdk/crates/redstone/src/core/
	// config.rs), so the adapter's get_prices_from_payload errors
	// before any event is emitted. Duplicates in args we're asked to
	// decode therefore mean the args did NOT drive the emitting call —
	// and pre-refusal they were an attribution-steering lever: a
	// duplicated feed inflated the state-write subset's arity
	// (audit F2), forcing the payload fallback on attacker-shaped
	// candidates. Refuse the whole event.
	ErrDuplicateFeedIDs = errors.New("redstone: duplicate feed_ids in op args")

	// ErrUpdaterMismatch — the event body's `updater` field disagrees
	// with the op args' updater (args[0]). write_prices publishes its
	// OWN updater argument in the event, so a disagreement means the
	// attached args belong to some other call than the one that
	// emitted this event. Refuse rather than attribute.
	ErrUpdaterMismatch = errors.New("redstone: event-body updater disagrees with op-args updater")

	// ErrStateWriteFeedMismatch — the op's value-changed contract-data
	// keys (events.Event.StateWriteKeys) name a feed set different
	// from the op-args feed_ids on an EQUAL-ARITY batch. An equal
	// arity means the adapter accepted every requested feed, and an
	// accepted feed's stored PriceData always changes — so the changed
	// feed-keyed writes must equal the feed_ids set exactly. A
	// mismatch means the args' feed identities don't describe what
	// the contract actually stored (steering, or a storage-shape
	// change) — refuse the whole event, never zip positionally on
	// uncorroborated names. Same honest-blind arm as
	// ErrAmbiguousSubset.
	ErrStateWriteFeedMismatch = errors.New("redstone: op-args feed_ids disagree with the op's value-changed state-write feed set")

	// ErrEventIndexOverflow — e.EventIndex exceeded eventFanoutStride
	// (DAT-06/trap-15, audit-2026-07-23). The synthetic OpIndex packs
	// (OperationIndex, EventIndex, vector position) into one uint32;
	// an EventIndex this large would spill into the next operation's
	// synthetic range. Real REDSTONE-adjacent ops emit at most a
	// handful of contract events, so hitting the stride means either
	// a decoder bug or a contract emitting far more events per op
	// than anything observed.
	ErrEventIndexOverflow = errors.New("redstone: EventIndex exceeds OpIndex fanout stride")

	// ErrOperationIndexOverflow — e.OperationIndex is negative or large
	// enough that the synthetic OpIndex packing
	// (OperationIndex*eventFanoutStride+EventIndex)*opIndexFanoutStride+i
	// spills past uint32. Guarded like EventIndex/vector-position (its
	// two siblings in the packing were bounds-checked; OperationIndex was
	// the one unguarded input — audit-2026-08-03). Unreachable on-chain
	// (Soroban caps ops-per-tx far below the bound); a hit means a
	// dispatcher bug feeding a bad index, which without the guard would
	// wrap and overlap another event's op_index block on the
	// oracle_updates PK.
	ErrOperationIndexOverflow = errors.New("redstone: OperationIndex exceeds OpIndex fanout bound")
)
