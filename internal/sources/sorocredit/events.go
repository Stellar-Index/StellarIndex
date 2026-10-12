// Package sorocredit decodes events from an unbranded consumer-USDC credit / CDP protocol on
// Stellar (Soroban). It ships no on-chain brand, so everything is keyed off its single main
// contract, which runs its OWN USDC credit book and deploys a per-user `Collateral-<uuid>` child
// per position.
//
// CRITICAL: `Liquidation` is a SCHEDULED SETTLEMENT, not distress. One keeper account executes
// ALL of them, ~1:1 with StatementPublished. They surface as `settlement` ([TypeSettlement], table
// `credit_settlements`), NEVER as "liquidations"; no surface may report a liquidation risk signal
// from this source.
//
// Gating (ADR-0035): the main contract is the single trust root. NewCollateralContract is honored
// only from it and announces the child C-address (topic[1]), seeded into a [contractid.Registry]
// child set; every other event needs the trust root OR a registered child. Two OTHER mainnet
// contracts emit the same symbols, so the identity gate, not the topic, rejects them.
//
// Per ADR-0013 SCVal is read only through internal/scval.
package sorocredit

import (
	"errors"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SourceName is the stable registry key for this source (metrics labels, cursor rows, storage attribution).
const SourceName = "sorocredit"

// MainnetContract is the protocol's single main contract and the ONE trust root for the event surface (ADR-0035); it emits every event and deploys the `Collateral-<uuid>` children.
const MainnetContract = "CCG5EWFY2KCWWYYEIUMIRG6WSAQFLDR5QE5FMCWY25N36XA5GYTCPQWR"

// GenesisLedger is the first ledger at which the main contract emitted any event, the lower bound for the gap detector and the reconcile re-derive.
const GenesisLedger uint32 = 61_620_822

// Event topic[0] symbols, exactly as they appear on the wire (Soroban
// Symbols). The main contract emits all eight.
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
	// TopicTreasuryUpdated is a config event rotating the treasury pointer. Body is
	// Vec[Address, Address] (old, new), kept verbatim like BeaconUpdated / CollateralHashUpdated
	// with no promoted column (see decodeConfigBody).
	TopicTreasuryUpdated = "TreasuryUpdated"
)

// Pre-encoded base64 SCVal::Symbol blobs for topic[0], so classify compares strings instead of
// re-decoding the topic per event.
var (
	topicSymNewCollateralContract = scval.MustEncodeSymbol(TopicNewCollateralContract)
	topicSymStatementPublished    = scval.MustEncodeSymbol(TopicStatementPublished)
	topicSymLiquidation           = scval.MustEncodeSymbol(TopicLiquidation)
	topicSymWithdrawal            = scval.MustEncodeSymbol(TopicWithdrawal)
	topicSymBeaconUpdated         = scval.MustEncodeSymbol(TopicBeaconUpdated)
	topicSymSupportedAssetAdded   = scval.MustEncodeSymbol(TopicSupportedAssetAdded)
	topicSymCollateralHashUpdated = scval.MustEncodeSymbol(TopicCollateralHashUpdated)
	topicSymTreasuryUpdated       = scval.MustEncodeSymbol(TopicTreasuryUpdated)
)

// topicClassifications is the single source of truth pairing each tracked topic[0] symbol with its
// [EventType]. classify() (decode.go) and EventSymbols() both derive from it so they cannot drift
// apart, which would drop an event from recognition (ADR-0033).
var topicClassifications = []struct {
	Topic string
	Sym   string
	Type  EventType
}{
	{TopicNewCollateralContract, topicSymNewCollateralContract, TypeNewCollateralContract},
	{TopicStatementPublished, topicSymStatementPublished, TypeStatement},
	{TopicLiquidation, topicSymLiquidation, TypeSettlement},
	{TopicWithdrawal, topicSymWithdrawal, TypeWithdrawal},
	{TopicBeaconUpdated, topicSymBeaconUpdated, TypeBeaconUpdated},
	{TopicSupportedAssetAdded, topicSymSupportedAssetAdded, TypeSupportedAssetAdded},
	{TopicCollateralHashUpdated, topicSymCollateralHashUpdated, TypeCollateralHashUpdated},
	{TopicTreasuryUpdated, topicSymTreasuryUpdated, TypeTreasuryUpdated},
}

// classifyBySym indexes topicClassifications by encoded symbol for classify().
var classifyBySym = func() map[string]EventType {
	m := make(map[string]EventType, len(topicClassifications))
	for _, c := range topicClassifications {
		m[c.Sym] = c.Type
	}
	return m
}()

// EventSymbols returns the eight topic[0] symbols this source consumes, for the projector's SQL topic prefilter; the identity gate then rejects look-alike emitters.
func EventSymbols() []string {
	syms := make([]string, len(topicClassifications))
	for i, c := range topicClassifications {
		syms[i] = c.Topic
	}
	return syms
}

// EventType routes a decoded event to its served-tier table; values are stable (they appear in EventKind(), the `event_type` CHECK on credit_events and reconciliation filters).
type EventType string

const (
	// TypeNewCollateralContract → credit_positions.
	TypeNewCollateralContract EventType = "new_collateral_contract"
	// TypeStatement → credit_statements.
	TypeStatement EventType = "statement_published"
	// TypeSettlement → credit_settlements. Decoded from the on-wire "Liquidation" topic but
	// scheduled settlements, NOT distressed liquidations (see the package doc).
	TypeSettlement EventType = "settlement"
	// TypeWithdrawal → credit_events.
	TypeWithdrawal EventType = "withdrawal"
	// TypeBeaconUpdated → credit_events (config).
	TypeBeaconUpdated EventType = "beacon_updated"
	// TypeSupportedAssetAdded → credit_events (config).
	TypeSupportedAssetAdded EventType = "supported_asset_added"
	// TypeCollateralHashUpdated → credit_events (config).
	TypeCollateralHashUpdated EventType = "collateral_hash_updated"
	// TypeTreasuryUpdated → credit_events (config). Treasury-pointer rotation; body verbatim.
	TypeTreasuryUpdated EventType = "treasury_updated"
)

// Errors returned by the decode path. Callers classify via errors.Is.
var (
	// ErrNotSoroCreditEvent — topic[0] matches none of the eight tracked symbols; the dispatcher
	// skips it cheaply rather than treating it as malformed.
	ErrNotSoroCreditEvent = errors.New("sorocredit: not a tracked event")

	// ErrMalformedPayload — topic arity, body shape or type tags differ from what the contract
	// emits. Fail-loud per event, not a silent skip, so decoder-vs-WASM drift surfaces.
	ErrMalformedPayload = errors.New("sorocredit: malformed event payload")
)

// Event is the single [consumer.Event] this source emits; [EventType] routes it to its table. Promoted string fields are strkeys or decimal-i128 strings (empty = SQL NULL); Amount is a decimal i128 string (ADR-0003, never int64); Attributes holds the rest as jsonb.
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

// EventKind implements [consumer.Event]; it varies with EventType (all start "sorocredit.") so reconciliation attributes counts per table.
func (e Event) EventKind() string { return SourceName + "." + string(e.EventType) }

// Source implements [consumer.Event] — matches [SourceName].
func (Event) Source() string { return SourceName }

// Compile-time check that Event satisfies consumer.Event.
var _ consumer.Event = Event{}
