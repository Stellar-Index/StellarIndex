package oracleagg

import (
	"fmt"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// FeedState is a Reflector v7.1.0 feed's own instance storage, the state its price() and lastprice() read
// (prices.rs, mapping.rs, price_oracle.rs). Everything the recipe needs from the feed is in this one entry.
type FeedState struct {
	LastTimestampMs uint64 // "last_timestamp": the newest round, ms; 0 before the first round
	ResolutionMs    uint64 // "resolution": the live period length, ms
	Decimals        uint32
	Protocol        uint32
	Base            Asset
	Assets          []Asset // "assets": the feed's asset list in index order
	// Indexed holds the instance's asset → index entries, each checked against Assets. A listed asset without one is
	// resolved by code outside the audited source copy (assets.rs), so it is not priced.
	Indexed map[Asset]uint32
	History []byte // "history": 32 bytes per asset, bit p = priced p periods before LastTimestampMs
	Cache   []CachedRound
}

// CachedRound is one "cache" record: a recent round's update mask and its prices in mask order.
type CachedRound struct {
	TimestampMs uint64
	Mask        []byte
	Prices      []*big.Int
}

// ReflectorProtocol is the storage protocol the recipe models; below it retrieve_asset_price_data takes the
// legacy per-price path (prices.rs:34), which is not reproduced.
const ReflectorProtocol = 2

var feedStringKeys = []string{
	"admin", "assets", "base_asset", "cache", "decimals", "expiration", "history", "last_timestamp",
	"period", "protocol", "protocol_update", "resolution", "retention",
}

// DecodeFeedState decodes a Reflector feed's instance storage. String keys outside the v7.1.0 set are an error;
// Symbol and Address keys are the asset-index entries and must agree with the "assets" order.
func DecodeFeedState(storage []scval.ScMapEntry) (FeedState, error) {
	str, index, err := splitFeedKeys(storage)
	if err != nil {
		return FeedState{}, err
	}
	var s FeedState
	for _, req := range []string{"assets", "base_asset", "decimals", "protocol", "resolution"} {
		if _, ok := str[req]; !ok {
			return FeedState{}, fmt.Errorf("%w: feed key %q missing", ErrConfig, req)
		}
	}
	if err := decodeFeedScalars(str, &s); err != nil {
		return FeedState{}, err
	}
	if s.Base, err = DecodeAsset(str["base_asset"]); err != nil {
		return FeedState{}, fmt.Errorf("%w: feed base_asset: %w", ErrConfig, err)
	}
	if s.Assets, err = decodeAssetVec(str["assets"]); err != nil {
		return FeedState{}, fmt.Errorf("%w: feed assets: %w", ErrConfig, err)
	}
	for a, idx := range index {
		if int(idx) >= len(s.Assets) || s.Assets[idx] != a {
			return FeedState{}, fmt.Errorf("%w: feed index entry %s = %d does not match the assets list", ErrConfig, a, idx)
		}
	}
	s.Indexed = index
	if v, ok := str["history"]; ok {
		if s.History, err = scval.AsBytes(v); err != nil {
			return FeedState{}, fmt.Errorf("%w: feed history: %w", ErrConfig, err)
		}
	}
	if v, ok := str["cache"]; ok {
		if s.Cache, err = decodeFeedCache(v); err != nil {
			return FeedState{}, err
		}
	}
	return s, nil
}

func splitFeedKeys(storage []scval.ScMapEntry) (map[string]scval.ScVal, map[Asset]uint32, error) {
	str := map[string]scval.ScVal{}
	index := map[Asset]uint32{}
	for _, e := range storage {
		if name, err := scval.AsString(e.Key); err == nil {
			if !contains(feedStringKeys, name) {
				return nil, nil, fmt.Errorf("%w: unknown feed key %q", ErrConfig, name)
			}
			str[name] = e.Val
			continue
		}
		var a Asset
		if sym, err := scval.AsSymbol(e.Key); err == nil {
			a = Asset{Symbol: sym}
		} else if addr, err := scval.AsAddressStrkey(e.Key); err == nil {
			a = Asset{Address: addr}
		} else {
			return nil, nil, fmt.Errorf("%w: feed key %s is neither String, Symbol nor Address", ErrConfig, scval.Display(e.Key))
		}
		idx, err := scval.AsU32(e.Val)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: feed asset index %s: %w", ErrConfig, a, err)
		}
		index[a] = idx
	}
	return str, index, nil
}

func decodeFeedScalars(str map[string]scval.ScVal, s *FeedState) error {
	var err error
	if v, ok := str["last_timestamp"]; ok {
		if s.LastTimestampMs, err = scval.AsU64(v); err != nil {
			return fmt.Errorf("%w: feed last_timestamp: %w", ErrConfig, err)
		}
	}
	res, err := scval.AsU32(str["resolution"])
	if err != nil {
		return fmt.Errorf("%w: feed resolution: %w", ErrConfig, err)
	}
	s.ResolutionMs = uint64(res)
	if s.Decimals, err = scval.AsU32(str["decimals"]); err != nil {
		return fmt.Errorf("%w: feed decimals: %w", ErrConfig, err)
	}
	if s.Protocol, err = scval.AsU32(str["protocol"]); err != nil {
		return fmt.Errorf("%w: feed protocol: %w", ErrConfig, err)
	}
	return nil
}

// decodeFeedCache decodes Vec<(u64, PriceUpdate{mask: Bytes, prices: Vec<i128>})>.
func decodeFeedCache(v scval.ScVal) ([]CachedRound, error) {
	vec, err := scval.AsVec(v)
	if err != nil {
		return nil, fmt.Errorf("%w: feed cache: %w", ErrConfig, err)
	}
	out := make([]CachedRound, 0, len(vec))
	for i, rec := range vec {
		t, err := scval.AsTupleN(rec, 2)
		if err != nil {
			return nil, fmt.Errorf("%w: feed cache[%d]: %w", ErrConfig, i, err)
		}
		var r CachedRound
		if r.TimestampMs, err = scval.AsU64(t[0]); err != nil {
			return nil, fmt.Errorf("%w: feed cache[%d] timestamp: %w", ErrConfig, i, err)
		}
		m, err := structFields(t[1], "mask", "prices")
		if err != nil {
			return nil, fmt.Errorf("%w: feed cache[%d]: %w", ErrConfig, i, err)
		}
		if r.Mask, err = scval.AsBytes(m["mask"]); err != nil {
			return nil, fmt.Errorf("%w: feed cache[%d] mask: %w", ErrConfig, i, err)
		}
		prices, err := scval.AsVec(m["prices"])
		if err != nil {
			return nil, fmt.Errorf("%w: feed cache[%d] prices: %w", ErrConfig, i, err)
		}
		for _, p := range prices {
			amt, err := scval.AsAmountFromI128(p)
			if err != nil {
				return nil, fmt.Errorf("%w: feed cache[%d] price: %w", ErrConfig, i, err)
			}
			r.Prices = append(r.Prices, amt.BigInt())
		}
		out = append(out, r)
	}
	return out, nil
}

// AssetIndex is the asset's index from its instance index entry; listed reports whether the asset is in the feed's
// list at all, so a listed asset with no entry can be told from one the feed does not quote.
func (s FeedState) AssetIndex(a Asset) (index uint32, indexed, listed bool) {
	if idx, ok := s.Indexed[a]; ok {
		return idx, true, true
	}
	return 0, false, containsAsset(s.Assets, a)
}

// HasPrice is mapping.rs::check_history_updated: whether the asset was priced `period` periods before the last round.
func (s FeedState) HasPrice(index, period uint32) bool {
	from := int(index*32 + (31 - period/8))
	if from >= len(s.History) {
		return false
	}
	bit := byte(1) << (period % 8)
	return s.History[from]&bit == bit
}

// RoundPriced reports whether any asset was priced `period` periods before the last round.
func (s FeedState) RoundPriced(period uint32) bool {
	for i := range s.Assets {
		if s.HasPrice(uint32(i), period) {
			return true
		}
	}
	return false
}

// CachedPrice is prices.rs::extract_single_update_record_price over the cached round at tsMs, if cached.
func (s FeedState) CachedPrice(index uint32, tsMs uint64) (*big.Int, bool) {
	for _, r := range s.Cache {
		if r.TimestampMs != tsMs {
			continue
		}
		n := 0
		for a := uint32(0); a <= index; a++ {
			byteAt := int(a / 8)
			set := byteAt < len(r.Mask) && r.Mask[byteAt]&(1<<(a%8)) != 0
			if !set {
				continue
			}
			if a == index {
				if n < len(r.Prices) {
					return r.Prices[n], true
				}
				return nil, false
			}
			n++
		}
		return nil, false
	}
	return nil, false
}
