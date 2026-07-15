package binance

import (
	_ "embed"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// pairsYAML is the declarative venue-pair table (see pairs.yaml for
// the per-pair rationale). Embedded at compile time — editing the
// YAML still means a rebuild + redeploy, exactly like the previous
// Go-literal table; the YAML form is the 45b per-venue pattern:
// pair set as reviewable data, constructor plumbing in one loader.
//
//go:embed pairs.yaml
var pairsYAML []byte

// pairsFile mirrors pairs.yaml's document shape.
type pairsFile struct {
	Pairs []pairSpec `yaml:"pairs"`
}

// pairSpec is one venue pair row: the Binance wire symbol plus the
// canonical identity of each side. Class is load-bearing — fiat vs
// crypto assets are distinct canonical types (e.g. BTCEUR quotes in
// fiat EUR, not a hypothetical crypto "EUR").
type pairSpec struct {
	Symbol string    `yaml:"symbol"`
	Base   assetSpec `yaml:"base"`
	Quote  assetSpec `yaml:"quote"`
}

type assetSpec struct {
	Code  string `yaml:"code"`
	Class string `yaml:"class"`
}

// asset materialises the spec through the canonical constructors so
// every code passes the same validation the old Go-literal table
// got from calling them directly.
func (a assetSpec) asset() (canonical.Asset, error) {
	switch a.Class {
	case "crypto":
		return canonical.NewCryptoAsset(a.Code)
	case "fiat":
		return canonical.NewFiatAsset(a.Code)
	default:
		return canonical.Asset{}, fmt.Errorf("%s: unknown asset class %q (want crypto|fiat)", a.Code, a.Class)
	}
}

// DefaultPairs returns the built-in pair map for Binance — the
// common set we stream when the operator enables Binance in config
// without specifying a pair list. Covers the largest XLM markets,
// the two reference crypto anchors, and the top-cap globals that
// every CoinGecko-class consumer expects.
//
// Returned map is Binance-symbol (uppercase, no separator) →
// canonical.Pair. Passed into NewStreamer. Extending the set is a
// one-entry addition to pairs.yaml; per-operator overrides land in
// config in a follow-up PR.
func DefaultPairs() (map[string]canonical.Pair, error) {
	var f pairsFile
	if err := yaml.Unmarshal(pairsYAML, &f); err != nil {
		return nil, fmt.Errorf("binance pairs.yaml: %w", err)
	}
	if len(f.Pairs) == 0 {
		return nil, fmt.Errorf("binance pairs.yaml: no pairs declared")
	}
	out := make(map[string]canonical.Pair, len(f.Pairs))
	for _, p := range f.Pairs {
		if p.Symbol == "" {
			return nil, fmt.Errorf("binance pairs.yaml: entry with empty symbol")
		}
		if _, dup := out[p.Symbol]; dup {
			return nil, fmt.Errorf("binance pairs.yaml: duplicate symbol %s", p.Symbol)
		}
		base, err := p.Base.asset()
		if err != nil {
			return nil, fmt.Errorf("%s base: %w", p.Symbol, err)
		}
		quote, err := p.Quote.asset()
		if err != nil {
			return nil, fmt.Errorf("%s quote: %w", p.Symbol, err)
		}
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Symbol, err)
		}
		out[p.Symbol] = pair
	}
	return out, nil
}

// DefaultPairList returns the same set as DefaultPairs but as a
// []canonical.Pair — the shape Streamer.Start expects. Convenience
// for callers that just want "stream everything the default config
// covers."
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
