package blend_backstop

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// SourceName is the registry key for this source.
const SourceName = "blend_backstop"

// Backstop contracts: the two mainnet Blend Backstop deployments, a SEPARATE event surface from
// the pool decoder (internal/sources/blend). Gating on BOTH is mandatory because backstop
// symbols overlap with pool event symbols, so the contract id (ADR-0035), not the topic,
// disambiguates them.
//
//   - MainnetBackstopV2 is the Backstop V2 singleton (current).
//   - MainnetBackstopV1 is read from the sibling blend package; both were live for a span and a
//     backfill range may replay either.
const MainnetBackstopV2 = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"

// MainnetBackstopV1 re-exports blend.MainnetBackstopV1 so the two packages cannot disagree.
const MainnetBackstopV1 = blend.MainnetBackstopV1

// BackstopGenesisLedger is a lower bound on any Blend Backstop event, used to size the gap
// detector's, the reconcile's and a fresh projector cursor's range.
const BackstopGenesisLedger = blend.FactoryGenesisLedger

// Event names: topic[0] Symbol strings across the V1 + V2 Backstop surface (V1 `rw_zone` where V2 uses `rw_zone_add`; V2
// `rw_zone_remove` is unconfirmed in the lake). Schema is reverse-engineered from lake samples (V1 arities from lake bytes only), so
// the source is LIVE-CAPTURE ONLY for backfill until a full replay lands.
const (
	EventDeposit           = "deposit"
	EventClaim             = "claim"
	EventDonate            = "donate"
	EventQueueWithdrawal   = "queue_withdrawal"
	EventWithdraw          = "withdraw"
	EventDistribute        = "distribute"
	EventGulpEmissions     = "gulp_emissions"
	EventDequeueWithdrawal = "dequeue_withdrawal"
	EventDraw              = "draw"
	EventRwZoneAdd         = "rw_zone_add"
	// EventRwZone is the V1 reward-zone-update topic, renamed `rw_zone_add` in V2. It stays a
	// distinct kind so the stored event_kind reflects exactly what was on the wire.
	EventRwZone = "rw_zone"
	// EventRwZoneRemove has no lake occurrences; it is kept so every event is classified.
	// decode.go documents an upstream doc-comment/code discrepancy for it.
	EventRwZoneRemove = "rw_zone_remove"
)

// Topic[0] pre-encoded base64, so Classify() does string compares instead of an SCVal decode.
var (
	TopicSymbolDeposit           = scval.MustEncodeSymbol(EventDeposit)
	TopicSymbolClaim             = scval.MustEncodeSymbol(EventClaim)
	TopicSymbolDonate            = scval.MustEncodeSymbol(EventDonate)
	TopicSymbolQueueWithdrawal   = scval.MustEncodeSymbol(EventQueueWithdrawal)
	TopicSymbolWithdraw          = scval.MustEncodeSymbol(EventWithdraw)
	TopicSymbolDistribute        = scval.MustEncodeSymbol(EventDistribute)
	TopicSymbolGulpEmissions     = scval.MustEncodeSymbol(EventGulpEmissions)
	TopicSymbolDequeueWithdrawal = scval.MustEncodeSymbol(EventDequeueWithdrawal)
	TopicSymbolDraw              = scval.MustEncodeSymbol(EventDraw)
	TopicSymbolRwZoneAdd         = scval.MustEncodeSymbol(EventRwZoneAdd)
	TopicSymbolRwZone            = scval.MustEncodeSymbol(EventRwZone)
	TopicSymbolRwZoneRemove      = scval.MustEncodeSymbol(EventRwZoneRemove)
)

// Event is the [consumer.Event] the backstop Decoder emits, one per decoded contract event, in
// the blend_backstop_events row shape: identity fields, promoted columns (Pool / UserAddress /
// Amount / Amount2) and the remainder in Attributes.
//
// Pool / UserAddress are strkeys; Amount / Amount2 are decimal i128 strings (ADR-0003, never
// int64). An empty value means the event type has no such field and the sink writes SQL NULL.
// The sink (internal/pipeline/sink.go) type-switches on this; the projector is the sole writer
// in sole-writer mode.
type Event struct {
	ContractID  string
	Ledger      uint32
	TxHash      string
	OpIndex     int
	EventIndex  int
	ObservedAt  time.Time
	EventType   string         // one of the Event* constants
	Pool        string         // pool Address strkey; "" → none
	UserAddress string         // user Address strkey; "" → none
	Amount      string         // decimal i128; "" → none
	Amount2     string         // decimal i128; "" → none
	Attributes  map[string]any // event-type-specific remainder
}

// EventKind implements [consumer.Event]: one kind for the whole surface; EventType discriminates.
func (Event) EventKind() string { return "blend_backstop.event" }

// Source implements [consumer.Event] — matches [SourceName].
func (Event) Source() string { return SourceName }

// Compile-time check that Event satisfies consumer.Event.
var _ consumer.Event = Event{}
