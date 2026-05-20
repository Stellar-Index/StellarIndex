// Package rozo decodes Rozo intent-bridge events on Soroban.
//
// Currently scoped to **v1 Payment** — the only mainnet-live Rozo
// contract at 2026-05-20. v2 Forwarder + IntentBridge and the newer
// rozo-intents schema are pre-mainnet and documented in
// docs/architecture/rozo-stellar-coverage.md for follow-up.
//
// Design rationale + open questions: see the architecture doc.
// Storage shape (`bridge_events` shared vs `rozo_events` separate)
// is operator-gated and not yet wired — this package emits canonical
// Go-side event values; the sink that persists them ships after the
// storage decision lands.
package rozo

import (
	"github.com/RatesEngine/rates-engine/internal/scval"
)

// SourceName is the registry key for this source. Used in
// `external.Registry`, status-page labels, and per-source metric
// labels. Must be stable across versions — appending v2 / v3
// support means new SOURCE entries (rozo-forwarder,
// rozo-intent-bridge) rather than renaming this one.
const SourceName = "rozo"

// MainnetPaymentContract is the verified deployment of the
// v1 Payment contract on Stellar pubnet. Verified
// 2026-05-20 via stellar.expert.
//
// Source: https://github.com/RozoAI/rozo-intents-contracts (v1).
const MainnetPaymentContract = "CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL"

// Event names — these are the `symbol_short!()` literals from
// `v1/stellar/payment/src/lib.rs`:
//
//	const PAYMENT: Symbol = symbol_short!("payment");
//	const FLUSH:   Symbol = symbol_short!("flush");
//
// `symbol_short!()` caps at 9 chars; both fit, so on-wire they
// serialize as ScSymbol (not the long-form ScString).
const (
	EventPayment = "payment"
	EventFlush   = "flush"
)

// Topic-prefix base64 strings (topic[0]). Pre-computed at package
// init via scval.MustEncodeSymbol so the classify() hot path does
// a single string-equal comparison rather than a full SCVal
// decode per event.
var (
	TopicSymbolPayment = scval.MustEncodeSymbol(EventPayment) // topic[0] of payment events
	TopicSymbolFlush   = scval.MustEncodeSymbol(EventFlush)   // topic[0] of flush events
)

// Payment is the canonical Go-side projection of one
// PaymentEvent emitted by Rozo v1's `pay(from, amount, memo)`
// function.
//
// On-wire shape (from v1/stellar/payment/src/lib.rs):
//
//	#[contracttype]
//	pub struct PaymentEvent {
//	    pub from: Address,
//	    pub destination: Address,
//	    pub amount: i128,
//	    pub memo: String,
//	}
//
//	env.events().publish((PAYMENT, from.clone()), PaymentEvent { … })
//
// Topic shape: `(symbol_short!("payment"), from: Address)`.
// Body shape: the struct above as a ScMap (Soroban's
// `#[contracttype]` macro lays out struct fields as a Map).
//
// USDC is the only token v1 handles — the contract hardcodes
// `USDC_CONTRACT` at init and `pay` transfers via the USDC
// token client. We don't surface the token field on the event
// because v1 has exactly one token; v2 (when it lands) will
// add a token field that varies per call.
type Payment struct {
	// Ledger / TxHash / OpIndex / ClosedAt come from the Event
	// envelope — included on the canonical struct so a downstream
	// sink doesn't need to re-thread the Event reference.
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string // RFC 3339 — caller parses via events.EventClosedAt()
	ContractID string

	// Payer — `from` field of PaymentEvent. The same address also
	// appears as topic[1] (the `from.clone()` second tuple slot).
	From string

	// Recipient — `destination` from PaymentEvent. Fixed at
	// contract init; doesn't vary per call within a deployed
	// contract.
	Destination string

	// Amount in raw token units (USDC = 7 decimals on Stellar per
	// internal/sources/external/registry.go's USDC contract). The
	// decoder preserves i128 → *big.Int → string per ADR-0003 ("i128
	// never truncates to int64"). Stored as decimal string on the
	// wire shape; downstream may parse to *big.Int as needed.
	Amount string

	// Memo — the user-supplied tag passed to `pay`. Free-form;
	// often a Binance / Coinbase deposit address tag, sometimes a
	// merchant order ID. Length-bounded by Soroban's String type
	// (no hard cap stated by the contract).
	Memo string
}

// Flush is the canonical projection of one FlushEvent emitted by
// Rozo v1's `flush(token)` admin function. Sweeps non-USDC
// balances accidentally sent to the contract.
//
// On-wire shape:
//
//	#[contracttype]
//	pub struct FlushEvent {
//	    pub token: Address,
//	    pub destination: Address,
//	    pub amount: i128,
//	}
//
//	env.events().publish((FLUSH,), FlushEvent { … })
//
// Topic shape: 1-element `(symbol_short!("flush"),)`.
type Flush struct {
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string
	ContractID string

	Token       string
	Destination string
	Amount      string // i128 as decimal string per ADR-0003
}
