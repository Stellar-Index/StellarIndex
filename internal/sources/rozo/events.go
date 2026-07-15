// Package rozo decodes Rozo intent-bridge events on Soroban.
//
// Currently scoped to **v1 Payment** — the only mainnet-live Rozo
// contract at 2026-05-20. v2 Forwarder + IntentBridge and the newer
// rozo-intents schema are pre-mainnet and documented in
// docs/architecture/rozo-stellar-coverage.md for follow-up.
//
// Design rationale: docs/architecture/rozo-stellar-coverage.md.
//
// Wiring (#41): decode.go decodes; consumer.go projects each event
// into the canonical rozo.Event row; dispatcher_adapter.go is the
// dispatcher Decoder; the indexer's sink persists via
// Store.InsertRozoEvent into the rozo_events hypertable
// (migration 0039, per-protocol table — operator-confirmed
// 2026-05-22). See README.md §Wiring.
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

// MainnetPaymentContract is the original verified deployment of the
// v1 Payment contract on Stellar pubnet. Verified 2026-05-20 via
// stellar.expert. Now part of [MainnetPaymentContracts] which lists
// every C-wallet Rozo uses for bridge-out flows.
//
// Source: https://github.com/RozoAI/rozo-intents-contracts (v1).
const MainnetPaymentContract = "CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL"

// MainnetPaymentContracts is the full set of Rozo bridge-out C
// contracts on Stellar pubnet. The original three were confirmed by
// RozoAI 2026-05-21 — all emit the same PaymentEvent / FlushEvent
// schemas. The decoder matches PaymentEvent / FlushEvent by topic[0],
// so adding a contract here is a watchlist concern (cross-validation
// + scoping), not a decoder-shape change.
//
// User flows: most bridge-out volume flows through C wallets when
// the user can't supply a memo (memo-less wallets, contract callers).
// G-wallet relayer flows handle the memo-bearing path — see
// [MainnetRelayerAccounts].
//
// 4th entry (`CAFO6OUZ…`) admitted 2026-07-09 per the §0.7
// recognition audit — it emitted exactly ONE payment_event (ledger
// 61522543) and wasn't yet on this list. Evidence, all independently
// verified against the ClickHouse lake on r1 (read-only: no operator
// confirmation from RozoAI obtained for this one):
//
//   - WASM hash bytewise IDENTICAL to the original three
//     (`b56aedeaf80c3d4b7c4c2ddf3893ac47c3ecff1a0a6f19152ca993e5bb294414`,
//     per docs/operations/wasm-audits/rozo.md) — read from its
//     contract-instance ledger entry at ledger 61522475.
//   - Instance storage carries the same 3-key init shape as the
//     others (`dest` Address, `usdc` Address, `init` bool). `dest` =
//     `GB4CLV3UMXDPFP5OQJQKUCWPRJXPXPJSHTUKZEJLAIZFZR7UHYAQ6EB4` —
//     an exact match to the second [MainnetRelayerAccounts] entry.
//     `usdc` = `CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75`
//     — the canonical Circle USDC SAC used elsewhere in this repo.
//   - Its one payment_event's `destination` field independently
//     resolves to that same relayer account.
//   - The contract was deployed+initialized at ledger 61522475 and
//     the payment fired 68 ledgers later with memo "test payment
//     0.01 USDC" — a deploy-then-smoke-test pattern, not a
//     spoof/collision (see decode.go's Classify doc for the topic-
//     collision risk this package guards against; two OTHER
//     contracts found in the same lake sweep — CDSXS5GK…, CCP6WOKM…
//     — collide on the legacy `payment` symbol but have unrelated
//     body schemas and are correctly NOT on this list).
//
// Because the WASM hash is bytewise identical to the already-audited
// hash (docs/operations/wasm-audits/rozo.md, approved 2026-05-26),
// this contract is covered by that same audit's findings by
// construction — no separate wasm-history walk needed. This is the
// doc's own documented re-audit trigger ("a new Rozo deploy beyond
// MainnetPaymentContracts"); the audit doc records the addition.
var MainnetPaymentContracts = []string{
	"CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL",
	"CCRLTS3CMJHYHFD7MYRBJPNW6R3LCXNDO2B6TK6AS6FSXAHR6GBMGLRE",
	"CAQPKW5AUPEA4C7OERZRUCBWT5RZDSETO4PR5REVRC5MT4CF3PBSKXQC",
	"CAFO6OUZAL62SGDVGHHJPSCOOF3HUKXLED3C3FS5RRQI2VBZ4F5HBPXI",
}

// MainnetRelayerAccounts is the set of CLASSIC Stellar accounts
// Rozo's relayer infrastructure uses to handle USDC / EURC bridge
// flows. Confirmed by RozoAI 2026-05-21 — "those 2 addresses should
// cover most of the txs on usdc/eurc".
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
// symbol_short!). Confirmed against the lake 2026-07-07: the three
// gated Rozo contracts emit topic_0_sym="payment_event" (393 events),
// zero as "payment" — so the original short-form match never fired and
// rozo_events was empty. We match BOTH forms: the long form is what's
// live, the short form is kept for forward-safety (contracts upgrade
// in place; a future/other version could emit either).
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
	TopicSymbolPayment = scval.MustEncodeSymbol(EventPayment) // legacy short form (never observed live)
	TopicSymbolFlush   = scval.MustEncodeSymbol(EventFlush)   // legacy short form (never observed live)

	// The live long-form topics — what the deployed contract emits.
	TopicSymbolPaymentEvent = scval.MustEncodeSymbol(symPaymentEvent) // topic[0] of payment events (live)
	TopicSymbolFlushEvent   = scval.MustEncodeSymbol(symFlushEvent)   // topic[0] of flush events (live)
)

// Payment is the canonical Go-side projection of one
// PaymentEvent emitted by Rozo v1's `pay(from, amount, memo)`
// function.
//
// On-wire body shape (from v1/stellar/payment/src/lib.rs):
//
//	#[contracttype]
//	pub struct PaymentEvent {
//	    pub from: Address,
//	    pub destination: Address,
//	    pub amount: i128,
//	    pub memo: String,
//	}
//
// The upstream source's `env.events().publish((PAYMENT,
// from.clone()), PaymentEvent { … })` call suggests a 2-tuple
// topic `(symbol_short!("payment"), from: Address)`, but that is
// NOT what the deployed mainnet contract emits. Verified against
// 3/3 real lake fixtures (ledgers 61859684, 63147040, 61797898;
// §0.7 verification, 2026-07-09): every observed payment_event has
// topic_count=1 — a single Symbol `("payment_event",)`, no
// second topic element. `from` is carried ONLY in the body ScMap,
// not duplicated as topic[1]. Do not rely on topic[1] for `from`;
// decode it via DecodePayment's map lookup like every other field.
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

	// Payer — `from` field of PaymentEvent, read from the body
	// ScMap. NOT duplicated in the topic: the deployed contract's
	// topic is a 1-element `(payment_event,)` symbol only (verified
	// 2026-07-09, see the type doc above) — there is no topic[1].
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
