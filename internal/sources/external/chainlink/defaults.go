package chainlink

import (
	"fmt"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Staleness budgets per feed class, derived from the Ethereum-mainnet
// heartbeats: crypto/USD feeds heartbeat at 1h (3h = two missed
// heartbeats plus slack); FX feeds heartbeat at 24h and pause over
// market closes, so a Friday-close round is ~72h old by Sunday night.
const (
	DefaultMaxAgeCrypto = 3 * time.Hour
	DefaultMaxAgeFX     = 76 * time.Hour
)

// DefaultFeedMap returns the built-in default seed, the sole source of these addresses and staleness budgets (internal/divergence/chainlink.go derives its map from it so ingest and divergence cannot drift); it applies only when feed_map is empty.
func DefaultFeedMap() map[string]FeedSpec {
	return map[string]FeedSpec{
		"crypto:BTC/fiat:USD":  {Address: "0xF4030086522a5bEEa4988F8cA5B36dbC97BeE88c", Decimals: 8, MaxAge: DefaultMaxAgeCrypto},
		"crypto:ETH/fiat:USD":  {Address: "0x5f4eC3Df9cbd43714FE2740f5E3616155c5b8419", Decimals: 8, MaxAge: DefaultMaxAgeCrypto},
		"crypto:LINK/fiat:USD": {Address: "0x2c1d072e956AFFC0D435Cb7AC38EF18d24d9127c", Decimals: 8, MaxAge: DefaultMaxAgeCrypto},
		"fiat:EUR/fiat:USD":    {Address: "0xb49f677943BC038e9857d61E7d053CaA2C1734C1", Decimals: 8, MaxAge: DefaultMaxAgeFX},
		"fiat:GBP/fiat:USD":    {Address: "0x5c0Ab2d9b5a7ed9f470386e82BB36A3613cDd4b5", Decimals: 8, MaxAge: DefaultMaxAgeFX},
		"fiat:JPY/fiat:USD":    {Address: "0xBcE206caE7f0ec07b545EddE332A47C2F75bbeb3", Decimals: 8, MaxAge: DefaultMaxAgeFX},
	}
}

// DefaultMaxAge is the staleness budget for a feed whose config omits one: built-in pairs keep theirs, other fiat/fiat pairs get the FX budget, everything else the crypto budget.
func DefaultMaxAge(pair string) time.Duration {
	if b, ok := DefaultFeedMap()[pair]; ok && b.MaxAge > 0 {
		return b.MaxAge
	}
	base, quote, ok := strings.Cut(pair, "/")
	if ok && strings.HasPrefix(base, "fiat:") && strings.HasPrefix(quote, "fiat:") {
		return DefaultMaxAgeFX
	}
	return DefaultMaxAgeCrypto
}

// BuildFeedSet parses operator feed-map entries into a runtime feed map and canonical pair list; a pair failing canonical parsing is an error, and Decimals is carried VERBATIM (0 means adopt on-chain decimals(), never substitute DefaultDecimals).
func BuildFeedSet(operatorMap map[string]FeedSpec) (map[string]FeedSpec, []canonical.Pair, error) {
	source := operatorMap
	if len(source) == 0 {
		source = DefaultFeedMap()
	}
	out := make(map[string]FeedSpec, len(source))
	pairs := make([]canonical.Pair, 0, len(source))
	for pairStr, setting := range source {
		p, err := canonical.ParsePair(pairStr)
		if err != nil {
			return nil, nil, fmt.Errorf("feed_map key %q: %w", pairStr, err)
		}
		if setting.MaxAge < 0 {
			return nil, nil, fmt.Errorf("feed_map key %q: negative max age %s", pairStr, setting.MaxAge)
		}
		maxAge := setting.MaxAge
		if maxAge == 0 {
			maxAge = DefaultMaxAge(p.String())
		}
		out[p.String()] = FeedSpec{
			Address:  setting.Address,
			Decimals: setting.Decimals,
			Invert:   setting.Invert,
			MaxAge:   maxAge,
		}
		pairs = append(pairs, p)
	}
	return out, pairs, nil
}
