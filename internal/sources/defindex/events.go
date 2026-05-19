// Package defindex decodes Soroban contract events emitted by the
// Blend autocompound *strategy* contracts that back paltalabs'
// DeFindex vaults on Stellar mainnet.
//
// IMPORTANT — what this actually decodes (corrected 2026-05-19):
// the contracts in scope publish events with topic[0] =
// ScvString("BlendStrategy") and a 2-tuple topic
// ("BlendStrategy", "deposit"|"withdraw"), body
// ScvMap{ from: Address, amount: i128 }. This was verified from
// real on-chain LCM via `ratesengine-ops scan-soroban-events`
// (e.g. ledger 57,056,389: ("BlendStrategy","deposit"){from,amount}).
//
// The earlier revision of this package targeted a fictional
// ("DeFindexVault", …){depositor, amounts:Vec<i128>,
// df_tokens_minted} schema lifted from paltalabs/defindex tag
// `1.0.0`; mainnet never deployed that. The deployed vault-address
// WASM (`11329c24…988`) is Blend strategy code. See
// docs/operations/wasm-audits/defindex.md "Audit result".
//
// We surface strategy deposit/withdraw events for flow attribution
// only — they are NOT price-discovery events and never contribute
// to VWAP. Dispatch is by topic (any contract emitting the
// BlendStrategy topic), mirroring the comet / aquarius
// shared-emitter topology — not a hand-curated contract set.
//
// See README.md for scope.
package defindex

import (
	"errors"
	"time"

	"github.com/RatesEngine/rates-engine/internal/canonical"
	"github.com/RatesEngine/rates-engine/internal/scval"
)

// SourceName is the registry key for this source. Kept as
// "defindex" (rather than renamed to e.g. "blend-strategy") so the
// registry / genesis / status-page keys stay stable; a rename is a
// separate product-taxonomy decision tracked in defindex.md.
const SourceName = "defindex"

// PrefixStrategy is topic[0] for every Blend strategy event. It is
// 13 chars, exceeds `symbol_short!`'s 9-char cap, so the SDK
// serialises it as ScvString (same pattern as Soroswap's
// "SoroswapPair"). Confirmed on-chain via scan-soroban-events.
const PrefixStrategy = "BlendStrategy"

// Topic[1] symbols for the user-facing flow events we decode. The
// strategy contract publishes more (harvest / keeper admin / …);
// Phase A only needs deposit + withdraw — the capital flow in/out
// of the strategy.
const (
	EventDeposit  = "deposit"
	EventWithdraw = "withdraw"
)

// Pre-encoded base64 SCVal blobs — byte-identical to what the
// contract emits — for cheap byte-equality classification on the
// hot path (no SCVal parsing for events we don't decode).
//
// Golden wire-format regression covered by
// internal/scval/scval_test.go::TestGolden_symbolBytes — if the SDK
// encoder shifts under us, that test fires before this ships.
var (
	TopicPrefixStrategy = scval.MustEncodeString(PrefixStrategy)
	TopicSymbolDeposit  = scval.MustEncodeSymbol(EventDeposit)
	TopicSymbolWithdraw = scval.MustEncodeSymbol(EventWithdraw)
)

// StrategyFlow is the canonical wire shape for one Blend strategy
// deposit or withdraw. Both directions share an identical body
// (`{from, amount}` — verified on-chain), so a single struct with a
// Direction discriminator is the natural shape.
//
// From is the caller moving capital — for these strategies it is
// typically the vault/router *contract* address (a C-strkey), not
// the end-user; end-user attribution requires correlating with the
// same-tx vault event (a Phase-B follow-up). It can also be a
// plain account G-strkey; scval.AsAddressStrkey renders both.
//
// Amount is the underlying-asset delta as a big-int-backed
// canonical.Amount (i128, never truncated — ADR-0003).
type StrategyFlow struct {
	Source     string
	Ledger     uint32
	ClosedAt   time.Time
	TxHash     string
	OpIndex    int
	ContractID string // the BlendStrategy contract that emitted
	Direction  Direction
	From       string           // account (G…) or contract (C…) strkey
	Amount     canonical.Amount // underlying-asset delta (i128)
}

// Direction discriminates the two flow types.
type Direction string

const (
	DirectionDeposit  Direction = "deposit"
	DirectionWithdraw Direction = "withdraw"
)

// Event wraps a StrategyFlow so it satisfies consumer.Event for the
// dispatcher / pipeline path. Phase A is log-only; the persist hook
// is a Phase-B add-on.
type Event struct {
	Flow StrategyFlow
}

// EventKind implements [consumer.Event].
func (e Event) EventKind() string {
	return "defindex.strategy." + string(e.Flow.Direction)
}

// Source implements [consumer.Event].
func (e Event) Source() string { return SourceName }

// Errors returned by the decode path. Callers classify via
// errors.Is.
var (
	// ErrUnknownEvent — topic shape doesn't match a deposit/withdraw
	// BlendStrategy event. The dispatcher's drop-counter records
	// these; not a failure ("strategy emits an event we don't
	// decode" — harvest / keeper admin — is normal).
	ErrUnknownEvent = errors.New("defindex: unknown strategy event topic")

	// ErrMalformedPayload — event body doesn't match the expected
	// {from, amount} schema (missing field, wrong type).
	ErrMalformedPayload = errors.New("defindex: malformed event payload")
)
