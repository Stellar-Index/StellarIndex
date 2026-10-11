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

// MainnetBackstopV1 re-exports blend.MainnetBackstopV1 so the
// backstop decoder's contract gate and the blend package agree on the
// V1 address without re-declaring the constant value here.
const MainnetBackstopV1 = blend.MainnetBackstopV1

// BackstopGenesisLedger is a lower bound on any Blend Backstop event: the
// Blend rollout's first contract instantiation, which the V1 backstop
// (earliest pinned lake event: 51_499_926) postdates. Sizes the gap
// detector's, the reconcile's and a fresh projector cursor's range.
const BackstopGenesisLedger = blend.FactoryGenesisLedger

// Event names: topic[0] Symbol strings across the V1 + V2 Backstop surface (V1 uses `rw_zone`
// where V2 uses `rw_zone_add`; V2 also has `rw_zone_remove`, unconfirmed in the lake).
//
// SCHEMA PROVENANCE: reverse-engineered from mainnet lake samples; V2 shapes are cross-checked
// against blend-contracts-v2 backstop/src/events.rs, V1 arities against lake bytes only. Until a
// full historical replay lands the source is LIVE-CAPTURE ONLY for backfill (README.md
// Provenance).
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
	// EventRwZoneRemove: zero lake occurrences at the last census (this
	// event had never fired on mainnet). Added per the EVERY-event
	// principle; decode.go documents a doc-comment/code discrepancy in the
	// upstream source for this one.
	EventRwZoneRemove = "rw_zone_remove"
)

// Topic[0] pre-encoded base64 — package-init constants so Classify()
// does single string-equal comparisons rather than a full SCVal
// decode per event (mirrors cctp / blend).
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

// EventKind implements [consumer.Event]. A single kind for the whole
// backstop surface — the per-event discriminator lives in EventType /
// the event_kind column, mirroring how cctp.Event uses one EventKind
// across its four event types.
func (Event) EventKind() string { return "blend_backstop.event" }

// Source implements [consumer.Event] — matches [SourceName].
func (Event) Source() string { return SourceName }

// Compile-time check that Event satisfies consumer.Event.
var _ consumer.Event = Event{}
