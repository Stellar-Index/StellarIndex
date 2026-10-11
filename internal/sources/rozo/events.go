// Package rozo decodes Rozo intent-bridge events on Soroban.
//
// Scoped to **v1 Payment**, the Rozo contract live on mainnet; v2 and the newer schema are in
// docs/protocols/rozo.md.
//
// Wiring: decode.go decodes; consumer.go projects each event into the canonical rozo.Event row;
// dispatcher_adapter.go is the Decoder; the projector, this table's sole writer (ADR-0032),
// persists rows via Store.InsertRozoEvent into the rozo_events hypertable. See README.md.
package rozo

import (
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the registry key and metric label for this source. It must stay stable: v2 / v3
// support means new source entries, not renaming this one.
const SourceName = "rozo"

// GenesisLedger is the first event ledger across all four Rozo contracts (derived from the lake);
// rozo_events is projected from exactly here. ProtocolMeta.GenesisLedger for "rozo" must equal it.
const GenesisLedger uint32 = 60_829_397

// MainnetPaymentContract is the original verified v1 Payment deployment on pubnet, and part of
// [MainnetPaymentContracts]. Source: https://github.com/RozoAI/rozo-intents-contracts (v1).
const MainnetPaymentContract = "CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL"

// MainnetPaymentContracts is the set of Rozo bridge-out C contracts on pubnet. All emit the same
// PaymentEvent / FlushEvent schemas, so adding one is a watchlist change, not a decoder change.
// Memo-bearing G-wallet flows go through [MainnetRelayerAccounts] instead.
//
// The 4th entry (CAFO6OUZ...) runs the same WASM hash as the original three, so the existing
// audit (docs/operations/wasm-audits/rozo.md) covers it. Two other contracts collide on the
// short-form `payment` symbol with unrelated bodies and are excluded (see Classify in decode.go).
var MainnetPaymentContracts = []string{
	"CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL",
	"CCRLTS3CMJHYHFD7MYRBJPNW6R3LCXNDO2B6TK6AS6FSXAHR6GBMGLRE",
	"CAQPKW5AUPEA4C7OERZRUCBWT5RZDSETO4PR5REVRC5MT4CF3PBSKXQC",
	"CAFO6OUZAL62SGDVGHHJPSCOOF3HUKXLED3C3FS5RRQI2VBZ4F5HBPXI",
}

// MainnetRelayerAccounts is the set of CLASSIC (G-strkey) accounts Rozo's relayer uses for USDC /
// EURC bridge flows, confirmed by RozoAI to cover most such txs. They appear as source or
// destination of classic `payment` operations and emit no Soroban contract events.
//
// Tracking, when wired, would watch their balance deltas to reconcile bridge inflow vs outflow and
// derive bridge volume independent of on-Soroban events.
var MainnetRelayerAccounts = []string{
	"GADDIYCVR2Z6H46YWZE53LICP56ZBNEUUT2QAG4QHSWVIYE44HS7W3XY",
	"GB4CLV3UMXDPFP5OQJQKUCWPRJXPXPJSHTUKZEJLAIZFZR7UHYAQ6EB4",
}

// Event kinds, the internal classify() labels. The deployed contract emits the long symbols
// "payment_event" / "flush_event", not the short ones its source suggests, so a short-form-only
// match never fires. Both are matched because contracts upgrade in place.
const (
	EventPayment = "payment"
	EventFlush   = "flush"

	// The actual on-wire topic[0] symbols the deployed contract emits.
	symPaymentEvent = "payment_event"
	symFlushEvent   = "flush_event"
)

// Topic-prefix base64 strings (topic[0]), precomputed so the classify() hot path is one string
// comparison rather than a full SCVal decode per event.
var (
	TopicSymbolPayment = scval.MustEncodeSymbol(EventPayment) // short form (never observed live)
	TopicSymbolFlush   = scval.MustEncodeSymbol(EventFlush)   // short form (never observed live)

	// The live long-form topics — what the deployed contract emits.
	TopicSymbolPaymentEvent = scval.MustEncodeSymbol(symPaymentEvent) // topic[0] of payment events (live)
	TopicSymbolFlushEvent   = scval.MustEncodeSymbol(symFlushEvent)   // topic[0] of flush events (live)
)

// Payment is the canonical projection of one PaymentEvent emitted by Rozo v1's
// `pay(from, amount, memo)`. Body: ScMap { from: Address, destination: Address, amount: i128,
// memo: String }.
//
// The deployed contract emits a single-Symbol topic, not (payment, from). `from` lives only in
// the body; decode it via DecodePayment's map lookup, never topic[1]. USDC is the only v1 token,
// so no token field is surfaced.
type Payment struct {
	// Ledger / TxHash / OpIndex / ClosedAt come from the Event envelope, carried here so a
	// downstream sink need not re-thread the Event.
	Ledger     uint32
	TxHash     string
	OpIndex    int
	ClosedAt   string // RFC 3339 — caller parses via events.EventClosedAt()
	ContractID string

	// Payer, the `from` field read from the body ScMap; there is no topic[1].
	From string

	// Recipient, the `destination` field; fixed at contract init.
	Destination string

	// Amount is in raw token units (USDC = 7 decimals). The decoder preserves
	// i128 -> *big.Int -> decimal string per ADR-0003; i128 never truncates to int64.
	Amount string

	// Memo is the user-supplied free-form tag passed to `pay` (an exchange deposit tag or
	// merchant order ID).
	Memo string
}

// Flush is the canonical projection of one FlushEvent from Rozo v1's `flush(token)` admin
// function, which sweeps non-USDC balances accidentally sent to the contract. The body is a
// struct { token: Address, destination: Address, amount: i128 } under a 1-element flush topic.
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
