package blend

import (
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Auction-event topic arity is 3 across all three auction events:
//
//	topic[0] = Symbol("new_auction" | "fill_auction" | "delete_auction")
//	topic[1] = u32(auction_type)
//	topic[2] = Address(user)
const auctionTopicArity = 3

// classify picks the event kind from topic[0], returning "" for non-Blend events so the dispatcher
// skips cheaply. It covers only the three auction topics; the dispatcher adapter routes through
// classifyAny (decode_money_market.go), which also maps money-market / admin / credit-risk topics.
func classify(e *events.Event) string {
	if len(e.Topic) == 0 {
		return ""
	}
	switch e.Topic[0] {
	case TopicSymbolNewAuction:
		return EventNewAuction
	case TopicSymbolFillAuction:
		return EventFillAuction
	case TopicSymbolDeleteAuction:
		return EventDeleteAuction
	default:
		return ""
	}
}

// AssetAmount is one (asset, amount) pair from an AuctionData
// `bid` or `lot` map. Amounts are i128 in the Blend contract; we
// surface them as *big.Int for canonical-types parity with the
// rest of the indexer (per AGENTS.md i128-never-truncates).
type AssetAmount struct {
	// Asset is the canonical asset (Soroban contract address) for
	// the entry. Native-XLM SAC contracts surface as a SorobanAsset
	// — Blend treats every asset uniformly via its contract id.
	Asset canonical.Asset
	// Amount is the i128 quantity, stored untransformed. Its meaning depends on auction_type
	// (topic[1]): UserLiquidation bid is dTokens and lot is bTokens; BadDebt bid is dTokens and
	// lot is underlying (backstop); Interest bid and lot are both underlying.
	Amount *big.Int
}

// AuctionData mirrors `pool/src/auctions/auction.rs::AuctionData`.
// Blend's struct has NAMED fields, so on the wire it is an ScvMap of three symbol-keyed entries
// (bid, lot, block), not a tuple. Shape is pinned by docs/operations/wasm-audits/blend.md
// ("AuctionData shape") and the v1-pool-auctions fixtures in v1_pool_auction_test.go.
type AuctionData struct {
	// Bid is the map of (asset, amount) pairs the filler spends
	// to clear the auction.
	Bid []AssetAmount
	// Lot is the map of (asset, amount) pairs the filler receives.
	Lot []AssetAmount
	// Block is the ledger block the auction begins on (used by the
	// Blend contract to scale auction prices over time). Stored
	// for analytics; we don't act on it at decode time.
	Block uint32
}

// NewAuctionEvent is the decoded form of Blend's `new_auction`
// event. One emitted per liquidation / bad-debt / interest auction
// when it's first announced.
type NewAuctionEvent struct {
	Pool        string // pool contract C-strkey (matched at dispatch)
	AuctionType uint32 // 0=UserLiquidation, 1=BadDebt, 2=Interest
	User        string // address whose position is being auctioned (G or C)
	Percent     uint32 // % of assets being auctioned off, 0-100 (not bps); DB CHECK enforces the range
	Data        AuctionData
	Ledger      uint32
	TxHash      string
	OpIndex     uint32
	// EventIndex is the contract event's index within its operation — the
	// per-event discriminator in the blend_auctions PK (migration 0058), so
	// multiple auction events from one op don't collide on ON CONFLICT DO
	// NOTHING.
	EventIndex uint32
	Timestamp  time.Time
}

// FillAuctionEvent is the decoded form of `fill_auction`. An auction may emit several fills as
// fillers take partial percentages; `fill_percent` over (auction_type, user, pool) sums to 100%
// by close.
type FillAuctionEvent struct {
	Pool        string
	AuctionType uint32
	User        string
	Filler      string   // who paid the bid + received the lot
	FillPercent *big.Int // i128 — fraction of the remaining auction filled
	// Data is nil for V1 pools: their fill_auction body carries no
	// filled auction data, so bid / lot / block are unknown, not zero.
	Data    *AuctionData
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// EventIndex is the per-event discriminator in the blend_auctions PK: one op can emit several
	// fill_auction events when a liquidation fills multiple positions.
	EventIndex uint32
	Timestamp  time.Time
}

// DeleteAuctionEvent is the decoded form of `delete_auction` —
// emitted when an auction is admin-deleted (e.g. position recovered
// before fill). Body is empty.
type DeleteAuctionEvent struct {
	Pool        string
	AuctionType uint32
	User        string
	Ledger      uint32
	TxHash      string
	OpIndex     uint32
	// EventIndex — per-event discriminator (blend_auctions PK, migration 0058).
	EventIndex uint32
	Timestamp  time.Time
}

// decodeNewAuction parses a `new_auction` event.
//
// Topics (3):
//
//	topic[0] = Symbol("new_auction")
//	topic[1] = u32(auction_type)
//	topic[2] = Address(user)
//
// Body: 2-tuple of (percent: u32, auction_data: AuctionData).
func decodeNewAuction(e *events.Event, closedAt time.Time) (NewAuctionEvent, error) {
	if len(e.Topic) != auctionTopicArity {
		return NewAuctionEvent{}, fmt.Errorf("%w: new_auction expected %d topics, got %d",
			ErrMalformedPayload, auctionTopicArity, len(e.Topic))
	}
	auctionType, err := decodeAuctionType(e.Topic[1])
	if err != nil {
		return NewAuctionEvent{}, err
	}
	user, err := decodeAddressTopic(e.Topic[2])
	if err != nil {
		return NewAuctionEvent{}, fmt.Errorf("%w: user: %w", ErrMalformedPayload, err)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return NewAuctionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	tuple, err := scval.AsTupleN(body, 2)
	if err != nil {
		return NewAuctionEvent{}, fmt.Errorf("%w: body shape: %w", ErrMalformedPayload, err)
	}
	percent, err := scval.AsU32(tuple[0])
	if err != nil {
		return NewAuctionEvent{}, fmt.Errorf("%w: percent: %w", ErrMalformedPayload, err)
	}
	data, err := decodeAuctionData(tuple[1])
	if err != nil {
		return NewAuctionEvent{}, fmt.Errorf("%w: auction_data: %w", ErrMalformedPayload, err)
	}

	return NewAuctionEvent{
		Pool:        e.ContractID,
		AuctionType: auctionType,
		User:        user,
		Percent:     percent,
		Data:        data,
		Ledger:      e.Ledger,
		TxHash:      e.TxHash,
		OpIndex:     uint32(e.OperationIndex),
		EventIndex:  uint32(e.EventIndex), //nolint:gosec // EventIndex is non-negative by Soroban spec.
		Timestamp:   closedAt,
	}, nil
}

// decodeFillAuction parses a `fill_auction` event.
//
// Body: 3-tuple of (filler: Address, fill_percent: i128,
// filled_auction_data: AuctionData).
func decodeFillAuction(e *events.Event, closedAt time.Time) (FillAuctionEvent, error) {
	if len(e.Topic) != auctionTopicArity {
		return FillAuctionEvent{}, fmt.Errorf("%w: fill_auction expected %d topics, got %d",
			ErrMalformedPayload, auctionTopicArity, len(e.Topic))
	}
	auctionType, err := decodeAuctionType(e.Topic[1])
	if err != nil {
		return FillAuctionEvent{}, err
	}
	user, err := decodeAddressTopic(e.Topic[2])
	if err != nil {
		return FillAuctionEvent{}, fmt.Errorf("%w: user: %w", ErrMalformedPayload, err)
	}

	body, err := scval.Parse(e.Value)
	if err != nil {
		return FillAuctionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	tuple, err := scval.AsTupleN(body, 3)
	if err != nil {
		return FillAuctionEvent{}, fmt.Errorf("%w: body shape: %w", ErrMalformedPayload, err)
	}
	filler, err := scval.AsAddressStrkey(tuple[0])
	if err != nil {
		return FillAuctionEvent{}, fmt.Errorf("%w: filler: %w", ErrMalformedPayload, err)
	}
	fillPercentAmt, err := scval.AsAmountFromI128(tuple[1])
	if err != nil {
		return FillAuctionEvent{}, fmt.Errorf("%w: fill_percent: %w", ErrMalformedPayload, err)
	}
	data, err := decodeAuctionData(tuple[2])
	if err != nil {
		return FillAuctionEvent{}, fmt.Errorf("%w: filled_auction_data: %w", ErrMalformedPayload, err)
	}

	return FillAuctionEvent{
		Pool:        e.ContractID,
		AuctionType: auctionType,
		User:        user,
		Filler:      filler,
		FillPercent: fillPercentAmt.BigInt(),
		Data:        &data,
		Ledger:      e.Ledger,
		TxHash:      e.TxHash,
		OpIndex:     uint32(e.OperationIndex),
		EventIndex:  uint32(e.EventIndex), //nolint:gosec // EventIndex is non-negative by Soroban spec.
		Timestamp:   closedAt,
	}, nil
}

// decodeDeleteAuction parses a `delete_auction` event. Body is
// the unit value (); the only useful fields are topic-derived.
func decodeDeleteAuction(e *events.Event, closedAt time.Time) (DeleteAuctionEvent, error) {
	if len(e.Topic) != auctionTopicArity {
		return DeleteAuctionEvent{}, fmt.Errorf("%w: delete_auction expected %d topics, got %d",
			ErrMalformedPayload, auctionTopicArity, len(e.Topic))
	}
	auctionType, err := decodeAuctionType(e.Topic[1])
	if err != nil {
		return DeleteAuctionEvent{}, err
	}
	user, err := decodeAddressTopic(e.Topic[2])
	if err != nil {
		return DeleteAuctionEvent{}, fmt.Errorf("%w: user: %w", ErrMalformedPayload, err)
	}
	return DeleteAuctionEvent{
		Pool:        e.ContractID,
		AuctionType: auctionType,
		User:        user,
		Ledger:      e.Ledger,
		TxHash:      e.TxHash,
		OpIndex:     uint32(e.OperationIndex),
		EventIndex:  uint32(e.EventIndex), //nolint:gosec // EventIndex is non-negative by Soroban spec.
		Timestamp:   closedAt,
	}, nil
}

// decodeAuctionType reads topic[1], a u32 discriminator. Values outside the documented set are
// rejected so a contract upgrade adding an auction type fails loud instead of misrouting.
func decodeAuctionType(topicB64 string) (uint32, error) {
	sv, err := scval.Parse(topicB64)
	if err != nil {
		return 0, fmt.Errorf("%w: auction_type parse: %w", ErrMalformedPayload, err)
	}
	at, err := scval.AsU32(sv)
	if err != nil {
		return 0, fmt.Errorf("%w: auction_type type: %w", ErrMalformedPayload, err)
	}
	switch at {
	case AuctionTypeUserLiquidation, AuctionTypeBadDebt, AuctionTypeInterest:
		return at, nil
	default:
		return 0, fmt.Errorf("%w: %d", ErrUnknownAuctionType, at)
	}
}

// decodeAddressTopic decodes a topic-slot Address (G-strkey for
// accounts, C-strkey for contracts) as a string. Used for both
// the filler / user fields.
func decodeAddressTopic(topicB64 string) (string, error) {
	sv, err := scval.Parse(topicB64)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	return scval.AsAddressStrkey(sv)
}
