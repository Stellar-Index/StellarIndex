package external

import "github.com/Stellar-Index/StellarIndex/internal/canonical"

// aggregatorFiats is the divergence layer's fiat coverage for aggregator cross-check pairs.
var aggregatorFiats = []string{"USD", "EUR", "GBP"}

// AggregatorPairs is the pair set every aggregator poller serves, live or backfilled: each
// catalogue ticker with a CoinGecko id (cgIDs, ticker -> slug) crossed with [aggregatorFiats],
// unioned with [DefaultAggregatorPairs]. skipped names catalogue tickers off the crypto allow-list.
func AggregatorPairs(cgIDs map[string]string) (pairs []canonical.Pair, skipped []string) {
	catalogue, skipped := CatalogueAggregatorPairs(cgIDs)
	return MergeAggregatorPairs(catalogue, DefaultAggregatorPairs()), skipped
}

// CatalogueAggregatorPairs crosses every ticker in cgIDs with [aggregatorFiats]; a ticker
// canonical.NewCryptoAsset rejects is returned in skipped rather than silently dropped.
func CatalogueAggregatorPairs(cgIDs map[string]string) (pairs []canonical.Pair, skipped []string) {
	if len(cgIDs) == 0 {
		return nil, nil
	}
	out := make([]canonical.Pair, 0, len(cgIDs)*len(aggregatorFiats))
	for ticker := range cgIDs {
		ca, err := canonical.NewCryptoAsset(ticker)
		if err != nil {
			skipped = append(skipped, ticker)
			continue
		}
		out = append(out, crossFiats(ca)...)
	}
	return out, skipped
}

// MergeAggregatorPairs unions the catalogue pairs with the defaults, deduplicated by Pair.String();
// the catalogue supplements the defaults, so a ticker without a coingecko_id keeps its coverage.
func MergeAggregatorPairs(catalogue, defaults []canonical.Pair) []canonical.Pair {
	out := make([]canonical.Pair, 0, len(catalogue)+len(defaults))
	seen := make(map[string]struct{}, len(catalogue)+len(defaults))
	for _, sets := range [][]canonical.Pair{catalogue, defaults} {
		for _, p := range sets {
			key := p.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// DefaultAggregatorPairs is the fallback set aggregators query when the catalogue isn't wired; add
// entries to `internal/currency/data/seed.yaml`, the canonical source.
func DefaultAggregatorPairs() []canonical.Pair {
	// XLM first per its product-special status; the rest alphabetical to keep diffs minimal.
	cryptos := []string{
		"XLM", "BTC", "ETH",
		"ADA", "ATOM", "AVAX", "BCH", "BNB", "DASH", "DOGE", "DOT",
		"LINK", "LTC", "NEAR", "SHIB", "SOL", "TON", "TRX", "UNI", "XRP",
	}
	out := make([]canonical.Pair, 0, len(cryptos)*len(aggregatorFiats))
	for _, c := range cryptos {
		ca, err := canonical.NewCryptoAsset(c)
		if err != nil {
			continue
		}
		out = append(out, crossFiats(ca)...)
	}
	return out
}

func crossFiats(ca canonical.Asset) []canonical.Pair {
	out := make([]canonical.Pair, 0, len(aggregatorFiats))
	for _, f := range aggregatorFiats {
		fa, err := canonical.NewFiatAsset(f)
		if err != nil {
			continue
		}
		p, err := canonical.NewPair(ca, fa)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}
