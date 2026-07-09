// Package blend_emitter decodes on-chain events from the Blend
// **Emitter** contract — the protocol-emissions plumbing that mints
// and distributes BLND to the backstop pools, separate from both the
// pool/pool-factory decoder (internal/sources/blend) and the
// Backstop decoder (internal/sources/blend_backstop).
//
// Wire shape, verified 2026-07-09 directly against the certified
// ClickHouse raw lake (ADR-0034; CH HTTP 8123, never MinIO's 9000) —
// every event this contract has EVER emitted on mainnet (469 total,
// 4 distinct topics, ALL single-topic):
//
//		topic[0] = Symbol("<event_name>")   (the only topic)
//		body     = per-event shape below
//
//	  - "distribute" (465 occurrences, ledgers 51,524,666–63,380,088) —
//	    one BLND emission distributed to a backstop.
//	    body = Vec[ Address backstop_id, i128 amount ]
//	    → emits a DistributeEvent
//	  - "drop"       (2 occurrences, ledgers 51,499,914 and 57,467,292) —
//	    a one-shot BLND airdrop to a VARIABLE-LENGTH recipient list
//	    (observed arities: 13 and 3).
//	    body = Vec[ Vec[ Address recipient, i128 amount ], ... ]
//	    → emits ONE DropEvent per contract event, carrying the full
//	    Recipients slice; the storage writer fans it out one row per
//	    recipient (recipient_index discriminator — same "coarse-PK
//	    data loss" lesson Phoenix/Aquarius already codify).
//	  - "q_swap"     (1 occurrence, ledger 56,992,670) — QUEUES a swap
//	    of the Emitter's target backstop + backstop token, subject to
//	    a timelock.
//	    body = Map{ new_backstop: Address, new_backstop_token: Address,
//	    unlock_time: u64 }
//	    → emits a SwapConfigEvent{Kind: SwapConfigQueued}
//	  - "swap"       (1 occurrence, ledger 57,467,277) — EXECUTES a
//	    previously queued backstop swap once its timelock has elapsed.
//	    Same body shape as q_swap (confirmed byte-identical on the
//	    real fixture: both events carry the same new_backstop /
//	    new_backstop_token / unlock_time values).
//	    → emits a SwapConfigEvent{Kind: SwapConfigExecuted}
//
// GATING (ADR-0035/0040): "distribute" is NOT a safe topic-only
// match — internal/sources/blend_backstop ALSO emits a bare
// `distribute` event (body: `i128 amount` only, no backstop_id) from
// the Backstop V1/V2 contracts. Routing on topic bytes alone would
// either misfire onto a backstop-emitted distribute or silently
// disagree on body shape. Matches() therefore gates on CONTRACT
// IDENTITY — the emitting contract must be in the curated registry —
// exactly the comet.MainnetGatedSet() pattern (curated set, no
// factory namespace to anchor on: the Emitter has a single canonical
// mainnet instance spanning Blend V1→V2).
//
// WASM audit CLOSED 2026-07-10 (docs/operations/wasm-audits/blend_emitter.md,
// ClickHouse-lake-only — no MinIO wasm-history walk): the contract's
// sole confirmed WASM hash
// (438a5528cff17ede6fe515f095c43c5f15727af17d006971485e52462e7e7b89)
// SHA256-verifies against bytes extracted from the lake, and ALL 469
// lifetime events (465/465 `distribute` exhaustively, not sampled,
// plus both `drop`s and the one `q_swap`/`swap`) decode to the exact
// shapes below. The audit did NOT corroborate this comment's earlier
// claim of "3 WASM uploads at ledgers 51,351,843 / 51,498,920 /
// 52,314,704" — the lake shows zero Soroban activity anywhere on the
// network at the first two ledgers, and the third resolves to the
// same single hash already established. BackfillSafe is now true.
package blend_emitter

import (
	"errors"

	"github.com/StellarIndex/stellar-index/internal/scval"
)

// SourceName is the canonical string stamped on every event this
// package emits, and the registry key used across config /
// dispatcher / projector / gated-registry wiring.
const SourceName = "blend_emitter"

// MainnetEmitter is the single canonical Blend Emitter contract on
// mainnet — one instance spanning Blend V1→V2 (verified against the
// ClickHouse lake 2026-07-09: 469 total events across its whole
// history, no address change observed). WASM-audited 2026-07-10
// (docs/operations/wasm-audits/blend_emitter.md): the contract's sole
// confirmed on-chain WASM hash SHA256-verifies, and all 469 lifetime
// events decode to the expected shape — BackfillSafe is true.
const MainnetEmitter = "CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR"

// MainnetGatedSet is the curated Emitter allowlist the decoder seeds
// — the ADR-0040 §1 mechanism-3 trust root (curated set; the Emitter
// has NO factory namespace, so there is no deploy event to anchor
// on). Mirrors comet.MainnetGatedSet(): a future genuine second
// Emitter deployment must be operator-admitted (a protocol_contracts
// row via seed-protocol-contracts, or a new entry here) before its
// events attribute — fail-closed, surfaced as an ADR-0033 recognition
// gap rather than silently attributed.
func MainnetGatedSet() []string { return []string{MainnetEmitter} }

// emitterTopicArity is the topic count on every Emitter event: just
// [Symbol("<event_name>")] — confirmed against all 469 lake events
// (topic_count = 1 uniformly). Anything shorter is not an Emitter
// event.
const emitterTopicArity = 1

// Event-topic constants — the four topic[0] symbols the Emitter
// contract has ever emitted on mainnet (full-topic census, 2026-07-09
// ClickHouse lake read).
const (
	EventDistribute = "distribute"
	EventDrop       = "drop"
	EventQSwap      = "q_swap"
	EventSwap       = "swap"
)

// Pre-encoded base64 SCVal::Symbol blobs for topic[0]. All four names
// are <= 9 chars so the Soroban SDK emits them via the compact
// `symbol_short!` form; scval.MustEncodeSymbol matches the wire form
// byte-for-byte (pinned by internal/scval/scval_test.go).
var (
	TopicSymbolDistribute = scval.MustEncodeSymbol(EventDistribute)
	TopicSymbolDrop       = scval.MustEncodeSymbol(EventDrop)
	TopicSymbolQSwap      = scval.MustEncodeSymbol(EventQSwap)
	TopicSymbolSwap       = scval.MustEncodeSymbol(EventSwap)
)

// Errors returned by the decode path.
var (
	// ErrNotEmitterEvent — topic[0] doesn't match any known Emitter
	// symbol. Skip: an unrelated contract, or (post-gate) a future
	// Emitter WASM upgrade that adds a new event kind. Operators see
	// the rate via stellarindex_source_orphan_events_total{source="blend_emitter"};
	// a sustained spike means decoder coverage is incomplete.
	ErrNotEmitterEvent = errors.New("blend_emitter: not a recognised Emitter event")

	// ErrMalformedPayload — body didn't decode to the expected shape
	// for the matched event kind.
	ErrMalformedPayload = errors.New("blend_emitter: malformed event payload")

	// ErrNonPositiveAmount — a distribute/drop amount is zero or
	// negative. A genuine emission/airdrop always moves a positive
	// BLND amount; zero-or-negative is either a contract bug or an
	// edge case we'd rather skip+count than emit.
	ErrNonPositiveAmount = errors.New("blend_emitter: amount must be positive")

	// ErrEmptyRecipients — a drop event's outer Vec has zero entries.
	// A drop with no recipients is not a meaningful airdrop; treated
	// as malformed rather than silently emitting zero rows.
	ErrEmptyRecipients = errors.New("blend_emitter: drop event has no recipients")
)
