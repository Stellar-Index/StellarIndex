// Package sorocredit decodes events from an unbranded consumer-USDC
// credit / CDP protocol on Stellar (Soroban). It is neutral-named
// ("Soroban credit"): the protocol ships no on-chain brand, so we key
// everything off its single main contract.
//
//	Main contract: CCG5EWFY2KCWWYYEIUMIRG6WSAQFLDR5QE5FMCWY25N36XA5GYTCPQWR
//	Creator:       GADI6FHS…   WASM: 84a88013…
//
// The protocol runs its OWN USDC credit book (verified independent — not
// a wrapper). A user opens a position, which deploys a per-user
// `Collateral-<uuid>` child contract; the protocol then publishes
// periodic per-position statements and settles them.
//
// # Event surface (7 topic[0] symbols, all emitted BY the main contract)
//
//	NewCollateralContract   position opened → deploys a child Collateral-<uuid>
//	StatementPublished      a periodic per-position charge/settlement statement
//	Liquidation             a SCHEDULED settlement (see the semantic note below)
//	Withdrawal              a position withdrawal (USDC out to a recipient)
//	BeaconUpdated           config: price-beacon (oracle) reference changed
//	SupportedAssetAdded     config: a collateral/debt asset admitted
//	CollateralHashUpdated   config: the collateral-contract WASM hash rotated
//
// # CRITICAL SEMANTIC — `Liquidation` is a SCHEDULED SETTLEMENT, not distress
//
// The on-wire topic is the symbol "Liquidation", but these events are
// NOT distressed liquidations. A single keeper account
// (GA3PWX3H…) executes ALL of them, ~1:1 with StatementPublished
// (lake 2026-07-07: 187,926 statements vs 187,718 "Liquidation"s over the
// contract's life) and ~14/user/month uniformly — i.e. they are recurring
// scheduled settlements of published statements, not risk events. We
// therefore surface them as `settlement` (EventType [TypeSettlement],
// table `credit_settlements`) — NEVER as "liquidations". Do NOT let any
// downstream surface report a "221k liquidations" risk signal from this
// source.
//
// # Gating (ADR-0035)
//
// Single trust root: the main contract. NewCollateralContract is honored
// only when emitted by the trust root, and it announces the child
// `Collateral-<uuid>` C-address (topic[1]) which the decoder seeds into a
// [contractid.Registry] child set (a childgate, like blend). Every other
// event is honored from the trust root OR a registered child. The topics
// are distinctive, but two OTHER mainnet contracts emit the same symbols
// (~159 events total, lake 2026-07-07) — the identity gate rejects them.
// In practice ALL 7 event types are emitted by the main contract and the
// child contracts emit nothing (verified), so the childgate is
// forward-compat defense-in-depth; the trust root does the real gating.
//
// Per ADR-0013 this decoder reads SCVal exclusively through
// internal/scval — it never imports go-stellar-sdk/xdr directly
// (enforced by scripts/ci/lint-imports.sh).
package sorocredit

import (
	"errors"
	"time"

	"github.com/StellarIndex/stellar-index/internal/consumer"
	"github.com/StellarIndex/stellar-index/internal/scval"
)

// SourceName is the registry key for this source — appears in metrics
// labels, cursor rows, and storage attribution. Stable.
const SourceName = "sorocredit"

// MainnetContract is the protocol's single main contract on Stellar
// mainnet — the ONE trust root for the whole event surface (ADR-0035).
// Every business + config event is emitted by this contract; it also
// deploys the per-position `Collateral-<uuid>` child contracts.
const MainnetContract = "CCG5EWFY2KCWWYYEIUMIRG6WSAQFLDR5QE5FMCWY25N36XA5GYTCPQWR"

// GenesisLedger is the first ledger at which the main contract emitted
// any event (2026-03-12, verified against the r1 lake). Lower bound for
// the per-source gap detector + the ADR-0033 reconcile re-derive.
const GenesisLedger uint32 = 61_620_822

// Event topic[0] symbols, exactly as they appear on the wire (Soroban
// Symbols). The main contract emits all seven.
const (
	TopicNewCollateralContract = "NewCollateralContract"
	TopicStatementPublished    = "StatementPublished"
	// TopicLiquidation is the on-wire symbol; we classify it as the
	// SCHEDULED-SETTLEMENT [TypeSettlement] — see the package doc.
	TopicLiquidation           = "Liquidation"
	TopicWithdrawal            = "Withdrawal"
	TopicBeaconUpdated         = "BeaconUpdated"
	TopicSupportedAssetAdded   = "SupportedAssetAdded"
	TopicCollateralHashUpdated = "CollateralHashUpdated"
)

// Pre-encoded base64 SCVal::Symbol blobs for topic[0], computed once at
// init. Classify does a single string-equality compare per event rather
// than re-decoding the topic. Mirrors blend / cctp.
var (
	topicSymNewCollateralContract = scval.MustEncodeSymbol(TopicNewCollateralContract)
	topicSymStatementPublished    = scval.MustEncodeSymbol(TopicStatementPublished)
	topicSymLiquidation           = scval.MustEncodeSymbol(TopicLiquidation)
	topicSymWithdrawal            = scval.MustEncodeSymbol(TopicWithdrawal)
	topicSymBeaconUpdated         = scval.MustEncodeSymbol(TopicBeaconUpdated)
	topicSymSupportedAssetAdded   = scval.MustEncodeSymbol(TopicSupportedAssetAdded)
	topicSymCollateralHashUpdated = scval.MustEncodeSymbol(TopicCollateralHashUpdated)
)

// EventSymbols returns the seven topic[0] symbol strings this source
// consumes, for the projector's SQL topic prefilter (topic_0_sym IN …).
// These are distinctive (not part of the CAP-67 classic-token firehose),
// so a topic prefilter pulls exactly this source's events and the
// identity gate then rejects the two look-alike emitters.
func EventSymbols() []string {
	return []string{
		TopicNewCollateralContract,
		TopicStatementPublished,
		TopicLiquidation,
		TopicWithdrawal,
		TopicBeaconUpdated,
		TopicSupportedAssetAdded,
		TopicCollateralHashUpdated,
	}
}

// EventType is the decoder-side discriminator that routes a decoded
// event to its served-tier table. The string values are stable — they
// appear in EventKind(), the `event_type` CHECK on credit_events, and
// the reconciliation catalogue's per-table kind filters.
type EventType string

const (
	// TypeNewCollateralContract → credit_positions.
	TypeNewCollateralContract EventType = "new_collateral_contract"
	// TypeStatement → credit_statements.
	TypeStatement EventType = "statement_published"
	// TypeSettlement → credit_settlements. Decoded from the on-wire
	// "Liquidation" topic; named `settlement` because these are
	// scheduled recurring settlements, NOT distressed liquidations
	// (see the package doc).
	TypeSettlement EventType = "settlement"
	// TypeWithdrawal → credit_events.
	TypeWithdrawal EventType = "withdrawal"
	// TypeBeaconUpdated → credit_events (config).
	TypeBeaconUpdated EventType = "beacon_updated"
	// TypeSupportedAssetAdded → credit_events (config).
	TypeSupportedAssetAdded EventType = "supported_asset_added"
	// TypeCollateralHashUpdated → credit_events (config).
	TypeCollateralHashUpdated EventType = "collateral_hash_updated"
)

// Errors returned by the decode path. Callers classify via errors.Is.
var (
	// ErrNotSoroCreditEvent — topic[0] doesn't match any of the seven
	// tracked symbols. Returned by decodeOne so the dispatcher skips
	// cheaply rather than treating it as malformed.
	ErrNotSoroCreditEvent = errors.New("sorocredit: not a tracked event")

	// ErrMalformedPayload — topic arity / body shape / type tags don't
	// match what the contract emits. Per-event fail-loud rather than a
	// silent skip; surfaces decoder-vs-WASM drift.
	ErrMalformedPayload = errors.New("sorocredit: malformed event payload")
)

// Event is the single [consumer.Event] this source emits — one per
// decoded contract event. The [EventType] discriminator routes it to one
// of four served-tier tables (credit_positions / credit_statements /
// credit_settlements / credit_events); the sink type-switches on this
// Go type once and branches on EventType internally.
//
// Promoted string fields are strkeys / decimal-i128 strings; an empty
// value means "this event type carries no such field" and the writer
// stores SQL NULL. Amount is a decimal i128 string (ADR-0003 — NEVER
// int64). Attributes holds the event-type-specific remainder (settlement
// extra vectors; config-event bodies) as a jsonb blob.
type Event struct {
	// Universal identity (from events.Event).
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    int
	EventIndex int
	ObservedAt time.Time

	EventType EventType

	// Promoted, per-event-type fields ("" / nil → SQL NULL):
	CollateralContract string     // child Collateral-<uuid> C-addr (positions/statements/settlements/withdrawal)
	PositionUUID       string     // positions/statements/settlements
	StatementUUID      string     // statements/settlements
	PositionName       string     // positions — the raw "Collateral-<uuid>" string
	Owner              string     // positions — position owner (G-addr)
	Account            string     // settlements: the keeper; withdrawal: the recipient (G-addr)
	Asset              string     // settlements: debt asset; withdrawal: token; supported_asset_added: the asset (USDC SAC C-addr)
	Amount             string     // decimal i128; statement amount / settled amount / withdrawal amount
	StatementTime      *time.Time // statements — the u64 statement timestamp in the event body

	Attributes map[string]any
}

// EventKind implements [consumer.Event]. Dynamic on EventType so each
// of the four tables' events carries a distinct kind string — this is
// what lets the ADR-0033 reconciliation catalogue attribute re-derived
// counts per table (and gives metrics/logs a precise label). All start
// with the "sorocredit." prefix.
func (e Event) EventKind() string { return SourceName + "." + string(e.EventType) }

// Source implements [consumer.Event] — matches [SourceName].
func (Event) Source() string { return SourceName }

// Compile-time check that Event satisfies consumer.Event.
var _ consumer.Event = Event{}
