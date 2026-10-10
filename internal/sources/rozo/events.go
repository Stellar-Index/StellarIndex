// Package rozo decodes Rozo intent-bridge events on Soroban.
//
// Scoped to **v1 Payment**, the Rozo contract found live on mainnet.
// v2 Forwarder + IntentBridge and the newer rozo-intents schema are
// documented in docs/protocols/rozo.md for follow-up.
//
// Design rationale: docs/protocols/rozo.md.
//
// Wiring: decode.go decodes; consumer.go projects each event into the
// canonical rozo.Event row; dispatcher_adapter.go is the Decoder; the
// projector, this table's sole writer (ADR-0032), persists each row via
// Store.InsertRozoEvent into the rozo_events hypertable (migration 0039,
// a per-protocol table). See README.md §Wiring.
package rozo

import (
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the registry key for this source. Used in
// `external.Registry`, status-page labels, and per-source metric
// labels. Must be stable across versions — appending v2 / v3
// support means new SOURCE entries (rozo-forwarder,
// rozo-intent-bridge) rather than renaming this one.
const SourceName = "rozo"

// GenesisLedger is the ledger of the first event across all four Rozo
// contracts (the exact genesis, derived from the lake); rozo_events is
// projected to exactly here. protocols_registry.go's
// ProtocolMeta.GenesisLedger for "rozo" must equal this.
const GenesisLedger uint32 = 60_829_397

// MainnetPaymentContract is the original verified deployment of the
// v1 Payment contract on Stellar pubnet, verified via stellar.expert.
// It is part of [MainnetPaymentContracts], which lists every C-wallet
// Rozo uses for bridge-out flows.
//
// Source: https://github.com/RozoAI/rozo-intents-contracts (v1).
const MainnetPaymentContract = "CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL"

// MainnetPaymentContracts is the set of Rozo bridge-out C contracts on pubnet.
// All emit the same PaymentEvent / FlushEvent schemas and the decoder matches by
// topic[0], so adding a contract is a watchlist concern, not a decoder-shape
// change.
//
// Most bridge-out volume flows through these C wallets (callers without a
// memo); memo-bearing G-wallet flows go through [MainnetRelayerAccounts].
//
// The 4th entry (CAFO6OUZ…) was admitted on lake evidence alone, without RozoAI
// confirmation. Its WASM hash is bytewise identical to the original three
// (b56aedea…4414, docs/operations/wasm-audits/rozo.md), so that audit covers it
// by construction. Its instance storage matches theirs (dest = the second
// [MainnetRelayerAccounts] entry, usdc = Circle's USDC SAC), and its single
// payment_event, 68 ledgers after deploy, was a smoke test rather than a spoof.
// Two other contracts collide on the short-form `payment` symbol with unrelated
// bodies and are correctly excluded (see Classify in decode.go).
var MainnetPaymentContracts = []string{
	"CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL",
	"CCRLTS3CMJHYHFD7MYRBJPNW6R3LCXNDO2B6TK6AS6FSXAHR6GBMGLRE",
	"CAQPKW5AUPEA4C7OERZRUCBWT5RZDSETO4PR5REVRC5MT4CF3PBSKXQC",
	"CAFO6OUZAL62SGDVGHHJPSCOOF3HUKXLED3C3FS5RRQI2VBZ4F5HBPXI",
}

// MainnetRelayerAccounts is the set of CLASSIC Stellar accounts
// Rozo's relayer infrastructure uses to handle USDC / EURC bridge
// flows. Confirmed by RozoAI: "those 2 addresses should cover most of
// the txs on usdc/eurc".
//
// These are G-strkey accounts (classic accounts), not contracts.
// They show up as either the SOURCE or DESTINATION of classic
// `payment` operations carrying USDC / EURC. They don't emit Soroban
// contract events.
//
// Tracking pattern (when wired): add to the supply observer's
// watched-account set or to a bridge-specific observer that records
// balance-change deltas into a bridge_relayer_balances hypertable.
// Useful for: (a) reconciling bridge inflow vs outflow, (b) flagging
// stuck relayer balances, (c) deriving USDC / EURC bridge volume
// independent of on-Soroban contract events.
var MainnetRelayerAccounts = []string{
	"GADDIYCVR2Z6H46YWZE53LICP56ZBNEUUT2QAG4QHSWVIYE44HS7W3XY",
	"GB4CLV3UMXDPFP5OQJQKUCWPRJXPXPJSHTUKZEJLAIZFZR7UHYAQ6EB4",
}

// Event kinds — the internal classify() labels routing to the right
// decoder. The contract source (`v1/stellar/payment/src/lib.rs`)
// suggested `symbol_short!("payment")` / `symbol_short!("flush")`, but
// the DEPLOYED mainnet contract emits the full-length ScSymbols
// "payment_event" / "flush_event" (13/11 chars — too long for
// symbol_short!). Confirmed against the lake: the three gated Rozo
// contracts emitted topic_0_sym="payment_event" (393 events) and zero
// as "payment", so a short-form-only match would never fire. We match
// BOTH forms: the long form is what's live, the short form is kept for
// forward-safety (contracts upgrade in place; a future/other version
// could emit either).
const (
	EventPayment = "payment"
	EventFlush   = "flush"

	// The actual on-wire topic[0] symbols the deployed contract emits.
	symPaymentEvent = "payment_event"
	symFlushEvent   = "flush_event"
)

// Topic-prefix base64 strings (topic[0]). Pre-computed at package
// init via scval.MustEncodeSymbol so the classify() hot path does
// a single string-equal comparison rather than a full SCVal
// decode per event.
var (
	TopicSymbolPayment = scval.MustEncodeSymbol(EventPayment) // short form (never observed live)
	TopicSymbolFlush   = scval.MustEncodeSymbol(EventFlush)   // short form (never observed live)

	// The live long-form topics — what the deployed contract emits.
	TopicSymbolPaymentEvent = scval.MustEncodeSymbol(symPaymentEvent) // topic[0] of payment events (live)
	TopicSymbolFlushEvent   = scval.MustEncodeSymbol(symFlushEvent)   // topic[0] of flush events (live)
)

// Payment is the canonical projection of one PaymentEvent emitted by Rozo v1's
// `pay(from, amount, memo)`.
//
// Body (v1/stellar/payment/src/lib.rs): ScMap { from: Address, destination:
// Address, amount: i128, memo: String }.
//
// The upstream source suggests topics (symbol_short!("payment"), from), but the
// deployed contract does not emit that: all 3 real lake fixtures (ledgers
// 61859684, 63147040, 61797898) have topic_count=1, a single Symbol
// ("payment_event",). `from` lives only in the body; decode it via
// DecodePayment's map lookup, never topic[1].
//
// USDC is the only token v1 handles (hardcoded at init), so no token field is
// surfaced; v2 will add one that varies per call.
type Payment struct {
	// Ledger / TxHash / OpIndex / ClosedAt come from the Event
	// envelope — included on the canonical struct so a downstream
	// sink doesn't need to re-thread the Event reference.
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string // RFC 3339 — caller parses via events.EventClosedAt()
	ContractID string

	// Payer — `from` field of PaymentEvent, read from the body
	// ScMap. NOT duplicated in the topic: the deployed contract's
	// topic is a 1-element `(payment_event,)` symbol only (verified
	// against the lake, see the type doc above) — there is no topic[1].
	From string

	// Recipient — `destination` from PaymentEvent. Fixed at
	// contract init; doesn't vary per call within a deployed
	// contract.
	Destination string

	// Amount in raw token units (USDC = 7 decimals on Stellar — the
	// same convention internal/sources/external/registry.go encodes as
	// AmountDecimals:7 for every on-chain DEX entry; rozo itself has no
	// AmountDecimals field there since bridges aren't VWAP-scaled). The
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
