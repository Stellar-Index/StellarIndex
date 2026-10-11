// Package blend_emitter decodes on-chain events from the Blend **Emitter**
// contract: the emissions plumbing that mints and distributes BLND to the backstop pools.
//
// Wire shape, verified against every mainnet event in the certified ClickHouse
// lake (ADR-0034): topic[0] = Symbol("<event_name>") is the only topic; body:
//
//   - "distribute": Vec[ Address backstop_id, i128 amount ] → DistributeEvent
//   - "drop": a one-shot airdrop, Vec[ Vec[ Address recipient, i128 amount ], ... ] of VARIABLE
//     length → ONE DropEvent with the full Recipients slice (fanned out per recipient on write).
//   - "q_swap" QUEUES a timelocked backstop swap, Map{ new_backstop, new_backstop_token: Address,
//     unlock_time: u64 } → SwapConfigQueued; "swap" EXECUTES it after the timelock → SwapConfigExecuted
//
// GATING (ADR-0035/0040): blend_backstop ALSO emits a bare `distribute` (body `i128 amount`), so
// topic bytes alone would misfire. Matches() gates on CONTRACT IDENTITY via the curated registry
// (the comet.MainnetGatedSet() pattern); one mainnet instance, no factory.
//
// WASM audit closed (docs/operations/wasm-audits/blend_emitter.md): the sole hash decodes every
// lifetime event to the shapes above. BackfillSafe is true.
package blend_emitter

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is stamped on every emitted event and is the registry key for config, dispatcher,
// projector and gated-registry wiring.
const SourceName = "blend_emitter"

// MainnetEmitter is the single mainnet Blend Emitter contract, spanning Blend V1 to V2 with no
// address change in the lake. WASM-audited (docs/operations/wasm-audits/blend_emitter.md), so
// BackfillSafe is true.
const MainnetEmitter = "CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR"

// MainnetGatedSet is the curated Emitter allowlist, the ADR-0040 §1 mechanism-3 trust root (no
// factory namespace, so no deploy event to anchor on). As with comet.MainnetGatedSet(), a second
// Emitter must be operator-admitted (a protocol_contracts row or a new entry here) before its
// events attribute; it fails closed and surfaces as an ADR-0033 recognition gap.
func MainnetGatedSet() []string { return []string{MainnetEmitter} }

// emitterTopicArity is the topic count on every Emitter event (just the event Symbol, uniform
// across the lake); anything shorter is not an Emitter event.
const emitterTopicArity = 1

// Event-topic constants: the only four topic[0] symbols the Emitter has emitted on mainnet.
const (
	EventDistribute = "distribute"
	EventDrop       = "drop"
	EventQSwap      = "q_swap"
	EventSwap       = "swap"
)

// Pre-encoded base64 SCVal::Symbol blobs for topic[0]. The names are short enough for the SDK's
// compact symbol form; MustEncodeSymbol matches that wire form byte-for-byte
// (pinned by internal/scval/scval_test.go).
var (
	TopicSymbolDistribute = scval.MustEncodeSymbol(EventDistribute)
	TopicSymbolDrop       = scval.MustEncodeSymbol(EventDrop)
	TopicSymbolQSwap      = scval.MustEncodeSymbol(EventQSwap)
	TopicSymbolSwap       = scval.MustEncodeSymbol(EventSwap)
)

// Errors returned by the decode path.
var (
	// ErrNotEmitterEvent means topic[0] is no known Emitter symbol: an unrelated contract or,
	// post-gate, a WASM upgrade adding an event kind. The source has no EvictedOrphans()
	// reporter, so unknown kinds land in the dispatcher's global unmatched tally and the
	// ADR-0033 recognition audit is the signal that decoder coverage is incomplete.
	ErrNotEmitterEvent = errors.New("blend_emitter: not a recognised Emitter event")

	// ErrMalformedPayload — body didn't decode to the expected shape
	// for the matched event kind.
	ErrMalformedPayload = errors.New("blend_emitter: malformed event payload")

	// ErrNonPositiveAmount means a distribute/drop amount is zero or negative; a genuine emission
	// always moves a positive amount, so it is skipped and counted rather than emitted.
	ErrNonPositiveAmount = errors.New("blend_emitter: amount must be positive")

	// ErrEmptyRecipients means a drop's outer Vec is empty; treated as malformed rather than
	// silently emitting zero rows.
	ErrEmptyRecipients = errors.New("blend_emitter: drop event has no recipients")
)
