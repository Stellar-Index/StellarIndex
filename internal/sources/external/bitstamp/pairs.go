package bitstamp

import (
	"fmt"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// DefaultPairs returns the Bitstamp symbol to canonical.Pair map for the default markets.
// Symbols are lowercase concatenated ("xlmusd"); the streamer adds the "live_trades_" channel
// prefix when subscribing. XLM EUR/GBP quotes overlap Kraken so two venues corroborate; XLM/BTC
// has deep European-retail liquidity; BTC/USD and ETH/USD are reference anchors, BTC/EUR gives
// European depth for triangulation, and top-cap globals against USD widen cross-venue VWAP.
func DefaultPairs() (map[string]canonical.Pair, error) {
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		return nil, fmt.Errorf("XLM: %w", err)
	}
	btc, err := canonical.NewCryptoAsset("BTC")
	if err != nil {
		return nil, fmt.Errorf("BTC: %w", err)
	}
	eth, err := canonical.NewCryptoAsset("ETH")
	if err != nil {
		return nil, fmt.Errorf("ETH: %w", err)
	}

	fiats := map[string]canonical.Asset{}
	for _, code := range []string{"USD", "EUR", "GBP"} {
		a, err := canonical.NewFiatAsset(code)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", code, err)
		}
		fiats[code] = a
	}

	majors := []string{
		"ADA", "ATOM", "AVAX", "BCH", "BNB", "DASH", "DOGE", "DOT",
		"LINK", "LTC", "NEAR", "SHIB", "SOL", "TON", "TRX", "UNI", "XRP",
	}
	majorAssets := make(map[string]canonical.Asset, len(majors))
	for _, code := range majors {
		a, err := canonical.NewCryptoAsset(code)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", code, err)
		}
		majorAssets[code] = a
	}

	spec := []struct {
		symbol string
		base   canonical.Asset
		quote  canonical.Asset
	}{
		{"xlmusd", xlm, fiats["USD"]},
		{"xlmeur", xlm, fiats["EUR"]},
		{"xlmgbp", xlm, fiats["GBP"]},
		{"xlmbtc", xlm, btc},
		// BTC + ETH cross-fiat: btcgbp, etheur, ethgbp
		// so the cross-venue VWAP set has full BTC+ETH × {USD,
		// EUR, GBP} parity across all 4 CEXes (binance, coinbase,
		// kraken, bitstamp).
		{"btcusd", btc, fiats["USD"]},
		{"btceur", btc, fiats["EUR"]},
		{"btcgbp", btc, fiats["GBP"]},
		{"ethusd", eth, fiats["USD"]},
		{"etheur", eth, fiats["EUR"]},
		{"ethgbp", eth, fiats["GBP"]},
	}
	for _, code := range majors {
		// Bitstamp uses lowercase concatenated symbols.
		sym := strings.ToLower(code) + "usd"
		spec = append(spec, struct {
			symbol string
			base   canonical.Asset
			quote  canonical.Asset
		}{sym, majorAssets[code], fiats["USD"]})
	}

	out := make(map[string]canonical.Pair, len(spec))
	for _, s := range spec {
		pair, err := canonical.NewPair(s.base, s.quote)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.symbol, err)
		}
		out[s.symbol] = pair
	}
	return out, nil
}

// DefaultPairList returns the same set as DefaultPairs as a slice.
func DefaultPairList() ([]canonical.Pair, error) {
	m, err := DefaultPairs()
	if err != nil {
		return nil, err
	}
	out := make([]canonical.Pair, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	return out, nil
}
