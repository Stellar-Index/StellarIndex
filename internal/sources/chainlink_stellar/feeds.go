package chainlink_stellar

import (
	"encoding/hex"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// feedEntry is what one data_id prices. Stablecoins keep their own identity (USDC/USD stays
// crypto:USDC vs fiat:USD): normalising at ingest would hide a depeg.
type feedEntry struct {
	Base  canonical.Asset
	Quote canonical.Asset
}

var quoteUSD = mustAsset(canonical.NewFiatAsset("USD"))

// feedRegistry maps the mainnet cache's data_ids (hex) to their pair, taken from each feed's
// FeedConfigSet description (ledgers 64442968, 64471781 and 65079956).
var feedRegistry = map[string]feedEntry{
	"01a0b4d920000332000000000000000000000000000000000000000000000000": {Base: mustAsset(canonical.NewCryptoAsset("BTC")), Quote: quoteUSD},     // BTC/USD
	"01d585327c000332000000000000000000000000000000000000000000000000": {Base: mustAsset(canonical.NewCryptoAsset("ETH")), Quote: quoteUSD},     // ETH/USD
	"016d06ebb6000332000000000000000000000000000000000000000000000000": {Base: mustAsset(canonical.NewCryptoAsset("USDT")), Quote: quoteUSD},    // USDT/USD
	"01a80ff216000332000000000000000000000000000000000000000000000000": {Base: mustAsset(canonical.NewCryptoAsset("USDC")), Quote: quoteUSD},    // USDC/USD
	"0101199b3b000332000000000000000000000000000000000000000000000000": {Base: mustAsset(canonical.NewCryptoAsset("LINK")), Quote: quoteUSD},    // LINK/USD
	"017be2f55a000332000000000000000000000000000000000000000000000000": {Base: mustAsset(canonical.NewCryptoAsset("XLM")), Quote: quoteUSD},     // XLM/USD
	"01962615c3000332000000000000000000000000000000000000000000000000": {Base: mustAsset(canonical.NewCryptoAsset("EURC")), Quote: quoteUSD},    // EURC/USD
	"0197aabf9c000332000000000000000000000000000000000000000000000000": {Base: mustAsset(canonical.NewRWAAsset("XAUm")), Quote: quoteUSD},       // XAUM Reference Rate
	"0142ac24bc000332000000000000000000000000000000000000000000000000": {Base: mustAsset(canonical.NewOracleRawAsset("FLHY")), Quote: quoteUSD}, // FLHY/USD
	"016e06407e000332000000000000000000000000000000000000000000000000": {Base: mustAsset(canonical.NewOracleRawAsset("USPC")), Quote: quoteUSD}, // USPC NAV
}

// resolveFeed returns the registered pair, else a raw:<data_id hex> row quoted in USD (every
// mainnet feed is), so an unregistered feed is recorded and promoted later, never dropped.
func resolveFeed(dataID []byte) (feedEntry, error) {
	id := hex.EncodeToString(dataID)
	if e, ok := feedRegistry[id]; ok {
		return e, nil
	}
	base, err := canonical.NewOracleRawAsset(id)
	if err != nil {
		return feedEntry{}, err
	}
	obs.SourceUnknownSymbolsTotal.WithLabelValues(SourceName).Inc()
	return feedEntry{Base: base, Quote: quoteUSD}, nil
}

func mustAsset(a canonical.Asset, err error) canonical.Asset {
	if err != nil {
		panic("chainlink_stellar: registry asset: " + err.Error())
	}
	return a
}
