package oracleagg

import (
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// FeedAssetKey maps a SEP-40 feed asset to the identity the Reflector decoder stores its rows under
// (reflector/decode.go decodeUpdateDataEntry): Stellar(address) is the Soroban asset, Other(symbol) goes through
// canonical.MapOracleSymbol. An unmapped symbol is stored as raw:<symbol>, which never prices, so it is an error.
func FeedAssetKey(a Asset) (canonical.Asset, error) {
	if a.Address != "" {
		return canonical.NewSorobanAsset(a.Address)
	}
	k, err := canonical.MapOracleSymbol(a.Symbol)
	if err != nil {
		return canonical.Asset{}, err
	}
	if !k.IsMapped() {
		return canonical.Asset{}, fmt.Errorf("symbol %q is unmapped (stored as %s)", a.Symbol, k)
	}
	return k, nil
}

// FeedFromRounds builds the recipe's Feed from a feed's stored rounds: lastRound is its newest round (zero when
// none), quote its stored quote (its SEP-40 base), key the feed asset's identity and prices its raw integers by
// round timestamp in Unix ms.
func FeedFromRounds(program FeedProgram, resolutionSeconds uint64, lastRound time.Time, quote, key canonical.Asset, prices map[int64]canonical.Amount) Feed {
	f := Feed{
		Program:      program,
		ResolutionMs: resolutionSeconds * 1000,
		Prices:       make(map[uint64]*big.Int, len(prices)),
		AssetIsBase:  !quote.IsZero() && canonical.CanonicalAsset(quote) == canonical.CanonicalAsset(key),
	}
	if !lastRound.IsZero() {
		f.LastRoundMs = uint64(lastRound.UnixMilli())
	}
	for ms, p := range prices {
		f.Prices[uint64(ms)] = p.BigInt()
	}
	return f
}
