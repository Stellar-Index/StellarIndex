package blend

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// Auction events emitted by the Blend Decoder. These are NOT canonical.Trade rows (Blend has no
// spot prices); the sink routes them to the per-protocol auctions table, not trades.

// Event kind strings used by the sink type-switch and metric labels, in the `<source>.<event>` convention.
const (
	NewAuctionEventKind    = "blend.new_auction"
	FillAuctionEventKind   = "blend.fill_auction"
	DeleteAuctionEventKind = "blend.delete_auction"

	// Money-market event kinds. Each carries the canonical
	// (per-pool, per-user, per-asset) position-changing event.
	PositionEventKind = "blend.position"
	EmissionEventKind = "blend.emission"
	AdminEventKind    = "blend.admin"
)

// EventKind / Source on the per-event types implements
// [consumer.Event]. The dispatcher's output channel has a
// single concrete type — consumer.Event — and the sink picks each
// off via type-switch.

func (NewAuctionEvent) EventKind() string { return NewAuctionEventKind }
func (NewAuctionEvent) Source() string    { return SourceName }

func (FillAuctionEvent) EventKind() string { return FillAuctionEventKind }
func (FillAuctionEvent) Source() string    { return SourceName }

func (DeleteAuctionEvent) EventKind() string { return DeleteAuctionEventKind }
func (DeleteAuctionEvent) Source() string    { return SourceName }

// PositionEvent.EventKind routes to the blend_positions writer; the decoder emits one money-market kind per event.
func (PositionEvent) EventKind() string { return PositionEventKind }
func (PositionEvent) Source() string    { return SourceName }

func (EmissionEvent) EventKind() string { return EmissionEventKind }
func (EmissionEvent) Source() string    { return SourceName }

func (AdminEvent) EventKind() string { return AdminEventKind }
func (AdminEvent) Source() string    { return SourceName }

// Compile-time checks that each event type satisfies consumer.Event.
var (
	_ consumer.Event = NewAuctionEvent{}
	_ consumer.Event = FillAuctionEvent{}
	_ consumer.Event = DeleteAuctionEvent{}
	_ consumer.Event = PositionEvent{}
	_ consumer.Event = EmissionEvent{}
	_ consumer.Event = AdminEvent{}
)
