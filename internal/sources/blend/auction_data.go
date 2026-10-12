package blend

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// AuctionData wire-format keys (pool/src/auctions/auction.rs), looked up by name so map order is irrelevant.
const (
	auctionDataKeyBid   = "bid"
	auctionDataKeyLot   = "lot"
	auctionDataKeyBlock = "block"
)

// decodeAuctionData parses the AuctionData ScvMap {bid: Map<Address,i128>, block: u32, lot: Map<Address,i128>}. Asset keys must be
// C-strkey contracts: canonical.NewSorobanAsset rejects G-addresses, matching Blend's asset registries.
func decodeAuctionData(sv scval.ScVal) (AuctionData, error) {
	entries, err := scval.AsMap(sv)
	if err != nil {
		return AuctionData{}, fmt.Errorf("auction_data shape: %w", err)
	}

	bidEntry, ok := scval.MapField(entries, auctionDataKeyBid)
	if !ok {
		return AuctionData{}, fmt.Errorf("auction_data missing %q", auctionDataKeyBid)
	}
	bid, err := decodeAssetAmountMap(bidEntry, auctionDataKeyBid)
	if err != nil {
		return AuctionData{}, err
	}

	lotEntry, ok := scval.MapField(entries, auctionDataKeyLot)
	if !ok {
		return AuctionData{}, fmt.Errorf("auction_data missing %q", auctionDataKeyLot)
	}
	lot, err := decodeAssetAmountMap(lotEntry, auctionDataKeyLot)
	if err != nil {
		return AuctionData{}, err
	}

	blockEntry, ok := scval.MapField(entries, auctionDataKeyBlock)
	if !ok {
		return AuctionData{}, fmt.Errorf("auction_data missing %q", auctionDataKeyBlock)
	}
	block, err := scval.AsU32(blockEntry)
	if err != nil {
		return AuctionData{}, fmt.Errorf("%s: %w", auctionDataKeyBlock, err)
	}

	return AuctionData{Bid: bid, Lot: lot, Block: block}, nil
}

// decodeAssetAmountMap decodes a Map<Address, i128>; keeps on-wire order (no sort) for round-trip parity.
func decodeAssetAmountMap(sv scval.ScVal, fieldName string) ([]AssetAmount, error) {
	entries, err := scval.AsMap(sv)
	if err != nil {
		return nil, fmt.Errorf("%s: shape: %w", fieldName, err)
	}
	out := make([]AssetAmount, 0, len(entries))
	for i := range entries {
		strkey, err := scval.AsAddressStrkey(entries[i].Key)
		if err != nil {
			return nil, fmt.Errorf("%s[%d] key: %w", fieldName, i, err)
		}
		asset, err := canonical.NewSorobanAsset(strkey)
		if err != nil {
			return nil, fmt.Errorf("%s[%d] asset: %w", fieldName, i, err)
		}
		amount, err := scval.AsAmountFromI128(entries[i].Val)
		if err != nil {
			return nil, fmt.Errorf("%s[%d] amount: %w", fieldName, i, err)
		}
		out = append(out, AssetAmount{Asset: asset, Amount: amount.BigInt()})
	}
	return out, nil
}
