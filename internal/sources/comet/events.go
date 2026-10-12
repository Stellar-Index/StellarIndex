// Package comet decodes on-chain events from Comet, a Soroban
// implementation of Balancer v1's weighted-AMM design (N >= 2 tokens,
// arbitrary weights).
//
// Wire shape: topic[0] = Symbol("POOL"), topic[1] = Symbol("<event_name>"),
// body a Map. Five events: "swap" -> a canonical.Trade; "join_pool" /
// "exit_pool" -> a LiquidityEvent per token; "deposit" / "withdraw" ->
// single-asset add / remove. Any other POOL topic is rejected with
// ErrNotCometEvent. BPT transfers belong to internal/sources/sep41_supply.
//
// `POOL` is shared by every Balancer-v1-derived contract, so routing is by topic bytes but
// attribution is gated on contract identity (ADR-0035/0040): Decoder.Matches claims only events
// from the curated registry. A comet-shaped event from an unregistered contract is left for the
// recognition audit, never attributed.
package comet

import (
	"errors"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the canonical string stamped on every event this package emits.
const SourceName = "comet"

// MainnetBackstopPool is the single known Comet pool on mainnet, Blend's BLND/USDC backstop (the wasm-audit census, docs/operations/wasm-audits/comet.md, found exactly one).
const MainnetBackstopPool = "CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM"

// MainnetGatedSet is the curated Comet pool allowlist, the ADR-0040 trust root (no factory event to anchor on); a new pool must be operator-admitted, until then it fails closed as a visible ADR-0033 recognition gap.
func MainnetGatedSet() []string { return []string{MainnetBackstopPool} }

// cometTopicArity is the minimum topic count on every Comet event: [Symbol("POOL"),
// Symbol("<event_name>")]. classify checks a floor, not equality, so an event with extra trailing
// topics still classifies on its first two; the identity gate in Matches, not arity, is trusted.
const cometTopicArity = 2

// LiquidityKind discriminates the four liquidity-mutating Comet events; values match the comet_liquidity.event_kind CHECK.
type LiquidityKind string

// Liquidity kinds map to two directions: join_pool / deposit -> "add" (grows the named token's reserves), exit_pool / withdraw ->
// "remove". The per-kind distinction stays in the row so a multi-token join (one event per token) is told from a single-asset deposit.
const (
	LiquidityJoinPool LiquidityKind = "join_pool"
	LiquidityExitPool LiquidityKind = "exit_pool"
	LiquidityDeposit  LiquidityKind = "deposit"
	LiquidityWithdraw LiquidityKind = "withdraw"
)

// IsValid reports whether k is one of the four known LiquidityKinds.
func (k LiquidityKind) IsValid() bool {
	switch k {
	case LiquidityJoinPool, LiquidityExitPool, LiquidityDeposit, LiquidityWithdraw:
		return true
	}
	return false
}

// Direction is the add/remove polarity of a liquidity event, matching the comet_liquidity.direction CHECK.
func (k LiquidityKind) Direction() string {
	switch k {
	case LiquidityJoinPool, LiquidityDeposit:
		return "add"
	case LiquidityExitPool, LiquidityWithdraw:
		return "remove"
	}
	return ""
}

// Event-topic constants, matched by byte equality via the TopicSymbol* blobs below.
const (
	EventTopic0   = "POOL"
	EventSwap     = "swap"
	EventJoinPool = "join_pool"
	EventExitPool = "exit_pool"
	EventDeposit  = "deposit"
	EventWithdraw = "withdraw"
)

// Pre-encoded base64 SCVal::Symbol blobs for topic[0] / topic[1]. The Soroban-SDK marshaller
// matches scval.MustEncodeSymbol byte-for-byte (pinned in internal/scval); all names are at most
// 9 chars, so they take the compact `symbol_short!` form, which the encoder picks by length.
var (
	TopicSymbolPool     = scval.MustEncodeSymbol(EventTopic0)
	TopicSymbolSwap     = scval.MustEncodeSymbol(EventSwap)
	TopicSymbolJoinPool = scval.MustEncodeSymbol(EventJoinPool)
	TopicSymbolExitPool = scval.MustEncodeSymbol(EventExitPool)
	TopicSymbolDeposit  = scval.MustEncodeSymbol(EventDeposit)
	TopicSymbolWithdraw = scval.MustEncodeSymbol(EventWithdraw)
)

// Errors returned by the decode path.
var (
	// ErrNotCometSwap - topic[0..1] doesn't match (POOL, swap); returned by `decodeSwap` on a
	// non-swap event. New decoders should prefer ErrNotCometEvent.
	ErrNotCometSwap = errors.New("comet: not a Comet POOL.swap event")

	// ErrNotCometEvent - topic[0..1] doesn't match any known Comet (POOL, <kind>) tuple. Unknown
	// kinds are rejected in Matches and land in the dispatcher's global unmatched tally; comet has
	// no EvictedOrphans() reporter, so the signal for a new kind on the gated pool is the ADR-0033
	// recognition audit (RecognitionOK=false).
	ErrNotCometEvent = errors.New("comet: not a recognised Comet POOL event")

	// ErrMalformedPayload — body did not decode to the Map shape for the matched kind.
	ErrMalformedPayload = errors.New("comet: malformed event payload")

	// ErrNonPositiveAmounts - token_amount_in or token_amount_out is zero or negative. A valid
	// swap has positive amounts on both sides, and liquidity events reject them too.
	// CAVEAT: a dust wdr_tokn_amt_in_get_lp_tokns_out withdraw can burn BPT with token_amount_out
	// 0, a real state change this reject drops. The drop is visible, not silent: the event Matches
	// but fails Decode, so the ADR-0033 reconcile counts it blind.Undecodable and flips the comet
	// verdict to complete=false. Accepting it would need the comet_liquidity CHECK(amount > 0)
	// relaxed.
	ErrNonPositiveAmounts = errors.New("comet: amounts must be positive")
)
