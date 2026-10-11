package binance

import (
	"bytes"
	_ "embed"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// pairsYAML is the declarative venue-pair table (see pairs.yaml for the per-pair rationale),
// embedded at compile time, so editing it needs a rebuild and redeploy.
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
// every code passes the same validation as calling them directly.
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

// parsePairsYAML decodes a pairs.yaml document with strict field checking, so an unknown or
// renamed key fails the load instead of silently dropping to the zero value. Separate from
// DefaultPairs so it is testable against arbitrary bytes.
func parsePairsYAML(b []byte) (pairsFile, error) {
	var f pairsFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return pairsFile{}, fmt.Errorf("binance pairs.yaml: %w", err)
	}
	return f, nil
}

// DefaultPairs returns the built-in Binance pair map, keyed by Binance symbol (uppercase, no
// separator) to canonical.Pair, for NewStreamer. Extending it is one entry in pairs.yaml.
func DefaultPairs() (map[string]canonical.Pair, error) {
	f, err := parsePairsYAML(pairsYAML)
	if err != nil {
		return nil, err
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
		// The candle seed is the longer synthesised identity, so a
		// symbol that fits it also fits formatTxHash's.
		if _, err := backfillTxHash(p.Symbol, 0, scale.LegacyCandleGranularity); err != nil {
			return nil, fmt.Errorf("binance pairs.yaml: %s: %w", p.Symbol, err)
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
