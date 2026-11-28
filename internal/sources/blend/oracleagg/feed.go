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

// FeedFromRounds builds the recipe's Feed from the feed's on-chain state and our stored rounds: storedLast is our
// newest stored round of the feed (zero when none) and prices the asset's raw integers by round timestamp (Unix ms).
func FeedFromRounds(program FeedProgram, state FeedState, storedLast time.Time, prices map[int64]canonical.Amount) Feed {
	f := Feed{Program: program, State: state, Prices: make(map[uint64]*big.Int, len(prices))}
	if !storedLast.IsZero() {
		f.StoredLastRoundMs = uint64(storedLast.UnixMilli())
	}
	for ms, p := range prices {
		f.Prices[uint64(ms)] = p.BigInt()
	}
	return f
}
