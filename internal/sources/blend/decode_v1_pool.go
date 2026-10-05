package blend

import (
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// V1 pool WASM (baf978f10efdbcd85747868bef8832845ea6809f7643b67a4ac0cd669327fc2c,
// the four V1-factory pools, never upgraded) emits three auction-family
// events in a different shape from V2. Verified against every such event in
// the r1 lake (737 fill / 435 new / 3 bad_debt, ledgers 51,612,222..62,625,124);
// samples in test/fixtures/blend/v1-pool-auctions/.
//
//	new_auction:  [Symbol, u32(auction_type)]            body AuctionData Map{bid, block, lot}
//	fill_auction: [Symbol, Address(user), u32(type)]     body (filler: Address, fill_percent: i128)
//	bad_debt:     [Symbol, Address(user)]                body (asset: Address, d_tokens: i128)
//
// V1 emits new_auction only for BadDebt and Interest auctions (user
// liquidations use new_liquidation_auction), and stores those auctions keyed
// on the pool's backstop, so the auctioned "user" is the V1 backstop: every
// V1 BadDebt/Interest fill_auction in the lake names it in topic[1]. V1 has
// no partial backstop auctions: the bid is the backstop's whole liability
// (the fixture's bad_debt d_tokens sum to the following new_auction bid), so
// percent is 100, as V2 emits for every BadDebt/Interest new_auction.
const (
	v1NewAuctionTopicArity = 2
	v1BadDebtTopicArity    = 2
	v1BackstopAuctionPct   = 100
)

// isV1FillAuction reports whether a fill_auction carries the V1 topic order
// (topic[1] is the user Address; V2 puts the u32 auction_type there).
func isV1FillAuction(e *events.Event) bool {
	if len(e.Topic) != auctionTopicArity {
		return false
	}
	_, err := decodeAddressTopic(e.Topic[1])
	return err == nil
}

// decodeNewAuctionV1 parses a V1 `new_auction` (BadDebt / Interest only).
func decodeNewAuctionV1(e *events.Event, closedAt time.Time) (NewAuctionEvent, error) {
	if len(e.Topic) != v1NewAuctionTopicArity {
		return NewAuctionEvent{}, fmt.Errorf("%w: V1 new_auction expected %d topics, got %d",
			ErrMalformedPayload, v1NewAuctionTopicArity, len(e.Topic))
	}
	auctionType, err := decodeAuctionType(e.Topic[1])
	if err != nil {
		return NewAuctionEvent{}, err
	}
	if auctionType == AuctionTypeUserLiquidation {
		// V1 announces user liquidations as new_liquidation_auction; a
		// UserLiquidation here is not keyed on the backstop.
		return NewAuctionEvent{}, fmt.Errorf("%w: V1 new_auction with auction_type %d",
			ErrMalformedPayload, auctionType)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return NewAuctionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	data, err := decodeAuctionData(body)
	if err != nil {
		return NewAuctionEvent{}, fmt.Errorf("%w: auction_data: %w", ErrMalformedPayload, err)
	}
	return NewAuctionEvent{
		Pool:        e.ContractID,
		AuctionType: auctionType,
		User:        MainnetBackstopV1, // every V1 pool comes from the mainnet V1 factory, which binds this backstop
		Percent:     v1BackstopAuctionPct,
		Data:        data,
		Ledger:      e.Ledger,
		TxHash:      e.TxHash,
		OpIndex:     uint32(e.OperationIndex),
		EventIndex:  uint32(e.EventIndex), //nolint:gosec // EventIndex is non-negative by Soroban spec.
		Timestamp:   closedAt,
	}, nil
}

// decodeFillAuctionV1 parses a V1 `fill_auction`. The body carries no filled
// auction data, so Data stays nil.
func decodeFillAuctionV1(e *events.Event, closedAt time.Time) (FillAuctionEvent, error) {
	if len(e.Topic) != auctionTopicArity {
		return FillAuctionEvent{}, fmt.Errorf("%w: V1 fill_auction expected %d topics, got %d",
			ErrMalformedPayload, auctionTopicArity, len(e.Topic))
	}
	user, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return FillAuctionEvent{}, fmt.Errorf("%w: user: %w", ErrMalformedPayload, err)
	}
	auctionType, err := decodeAuctionType(e.Topic[2])
	if err != nil {
		return FillAuctionEvent{}, err
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return FillAuctionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	tuple, err := scval.AsTupleN(body, 2)
	if err != nil {
		return FillAuctionEvent{}, fmt.Errorf("%w: body shape: %w", ErrMalformedPayload, err)
	}
	filler, err := scval.AsAddressStrkey(tuple[0])
	if err != nil {
		return FillAuctionEvent{}, fmt.Errorf("%w: filler: %w", ErrMalformedPayload, err)
	}
	fillPercent, err := scval.AsAmountFromI128(tuple[1])
	if err != nil {
		return FillAuctionEvent{}, fmt.Errorf("%w: fill_percent: %w", ErrMalformedPayload, err)
	}
	return FillAuctionEvent{
		Pool:        e.ContractID,
		AuctionType: auctionType,
		User:        user,
		Filler:      filler,
		FillPercent: fillPercent.BigInt(),
		Ledger:      e.Ledger,
		TxHash:      e.TxHash,
		OpIndex:     uint32(e.OperationIndex),
		EventIndex:  uint32(e.EventIndex), //nolint:gosec // EventIndex is non-negative by Soroban spec.
		Timestamp:   closedAt,
	}, nil
}

// decodeBadDebtV1 parses a V1 `bad_debt`: the asset moves from topic[2]
// (V2) into the body tuple.
func decodeBadDebtV1(e *events.Event, closedAt time.Time) (EmissionEvent, error) {
	if len(e.Topic) != v1BadDebtTopicArity {
		return EmissionEvent{}, fmt.Errorf("%w: V1 bad_debt expected %d topics, got %d",
			ErrMalformedPayload, v1BadDebtTopicArity, len(e.Topic))
	}
	user, err := decodeAddressTopic(e.Topic[1])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: user: %w", ErrMalformedPayload, err)
	}
	body, err := scval.Parse(e.Value)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: parse body: %w", ErrMalformedPayload, err)
	}
	tuple, err := scval.AsTupleN(body, 2)
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: body shape: %w", ErrMalformedPayload, err)
	}
	asset, err := scval.AsAddressStrkey(tuple[0])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: asset: %w", ErrMalformedPayload, err)
	}
	amt, err := scval.AsAmountFromI128(tuple[1])
	if err != nil {
		return EmissionEvent{}, fmt.Errorf("%w: d_tokens: %w", ErrMalformedPayload, err)
	}
	return EmissionEvent{
		Pool:      e.ContractID,
		Kind:      EventBadDebt,
		User:      user,
		Asset:     asset,
		Amount:    amt.BigInt(),
		Ledger:    e.Ledger,
		TxHash:    e.TxHash,
		OpIndex:   uint32(e.OperationIndex),
		Timestamp: closedAt,
	}, nil
}
