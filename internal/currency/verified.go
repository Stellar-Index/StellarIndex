package currency

import (
	_ "embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed data/seed.yaml
var seedYAML []byte

// VerifiedCurrency is one entry in the verified-currency catalogue.
// Pointer-shared across every index in the *Catalogue so callers can
// rely on value-equality (the *VerifiedCurrency returned from
// LookupBySlug is the same instance returned by LookupByTicker).
type VerifiedCurrency struct {
	Ticker              string
	Slug                string
	Name                string
	Description         string
	CoinGeckoID         string
	CoinMarketCapID     string
	VerifiedIssuerLabel string
	Networks            []NetworkEntry
}

// NetworkEntry is one per-network identity for a verified currency.
type NetworkEntry struct {
	// Network identifier — short, lowercase, stable across versions.
	// Examples: "stellar", "ethereum", "solana", "bitcoin", "tron",
	// "polygon", "base", "arbitrum", "bsc", "avalanche", "xrpl".
	Network string
	// Stellar-specific fields. Populated when Network == "stellar"
	// for classic assets. For native XLM, AssetID == "native" and
	// Code / Issuer are empty.
	Code    string
	Issuer  string
	AssetID string
	// Non-Stellar contract address (token address on the source
	// chain). Empty for native assets of their own chain (BTC on
	// bitcoin, ETH on ethereum) and for Stellar entries.
	Contract string
	// ExternalLink is an optional override for the explorer's
	// drill-out link. Empty falls back to a network-default
	// (etherscan.io / solscan.io / etc) at render time.
	ExternalLink string
}

// Catalogue indexes a loaded set of verified currencies for the
// per-handler lookups the API needs. All lookups are read-only and
// safe for concurrent use — the catalogue is constructed once at
// binary startup and never mutated.
type Catalogue struct {
	entries          []*VerifiedCurrency
	bySlug           map[string]*VerifiedCurrency // lowercase
	byTicker         map[string]*VerifiedCurrency // uppercase
	byStellarAssetID map[string]*VerifiedCurrency // exact-match
	// byStellarCode maps an uppercase classic code to the verified
	// currency that holds that code on Stellar. Used by
	// StellarCollision — given a (code, issuer) pair we look up by
	// code and check whether the issuer matches the verified entry.
	byStellarCode map[string]*VerifiedCurrency
}

// rawCatalogue is the on-disk shape of seed.yaml; unmarshalling
// happens here so callers can keep the typed Catalogue API clean.
type rawCatalogue struct {
	VerifiedCurrencies []rawCurrency `yaml:"verified_currencies"`
}

type rawCurrency struct {
	Ticker              string       `yaml:"ticker"`
	Slug                string       `yaml:"slug"`
	Name                string       `yaml:"name"`
	Description         string       `yaml:"description"`
	CoinGeckoID         string       `yaml:"coingecko_id"`
	CoinMarketCapID     string       `yaml:"coinmarketcap_id"`
	VerifiedIssuerLabel string       `yaml:"verified_issuer_label"`
	Networks            []rawNetwork `yaml:"networks"`
}

type rawNetwork struct {
	Network      string `yaml:"network"`
	Code         string `yaml:"code"`
	Issuer       string `yaml:"issuer"`
	AssetID      string `yaml:"asset_id"`
	Contract     string `yaml:"contract"`
	ExternalLink string `yaml:"external_link"`
}

// LoadEmbedded parses the binary-embedded seed catalogue. Used by
// the production wiring in cmd/ratesengine-api/main.go.
func LoadEmbedded() (*Catalogue, error) {
	return LoadFromBytes(seedYAML)
}

// LoadFromBytes parses an arbitrary YAML blob. Used by tests and by
// any future operator-config override path.
func LoadFromBytes(b []byte) (*Catalogue, error) {
	var raw rawCatalogue
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("currency: yaml parse: %w", err)
	}

	cat := &Catalogue{
		bySlug:           make(map[string]*VerifiedCurrency, len(raw.VerifiedCurrencies)),
		byTicker:         make(map[string]*VerifiedCurrency, len(raw.VerifiedCurrencies)),
		byStellarAssetID: make(map[string]*VerifiedCurrency),
		byStellarCode:    make(map[string]*VerifiedCurrency),
	}

	for i, rc := range raw.VerifiedCurrencies {
		if err := cat.append(i, rc); err != nil {
			return nil, err
		}
	}
	return cat, nil
}

// append validates a single raw entry, builds the typed
// *VerifiedCurrency, and threads it through the Catalogue's indexes.
// Pulled out of LoadFromBytes to keep the per-entry control flow
// linear-readable.
func (cat *Catalogue) append(i int, rc rawCurrency) error {
	if err := validateRawEntry(i, rc); err != nil {
		return err
	}
	vc, err := buildVerifiedCurrency(rc)
	if err != nil {
		return err
	}

	slugKey := strings.ToLower(vc.Slug)
	if _, dup := cat.bySlug[slugKey]; dup {
		return fmt.Errorf("currency: duplicate slug %q", vc.Slug)
	}
	cat.bySlug[slugKey] = vc

	tickerKey := strings.ToUpper(vc.Ticker)
	if _, dup := cat.byTicker[tickerKey]; dup {
		return fmt.Errorf("currency: duplicate ticker %q", vc.Ticker)
	}
	cat.byTicker[tickerKey] = vc

	if err := cat.indexStellarEntries(vc); err != nil {
		return err
	}

	cat.entries = append(cat.entries, vc)
	return nil
}

func validateRawEntry(i int, rc rawCurrency) error {
	switch {
	case rc.Ticker == "":
		return fmt.Errorf("currency: entry %d: ticker is required", i)
	case rc.Slug == "":
		return fmt.Errorf("currency: entry %d (%s): slug is required", i, rc.Ticker)
	case rc.Name == "":
		return fmt.Errorf("currency: entry %d (%s): name is required", i, rc.Ticker)
	case len(rc.Networks) == 0:
		return fmt.Errorf("currency: entry %d (%s): at least one network entry required", i, rc.Ticker)
	}
	return nil
}

func buildVerifiedCurrency(rc rawCurrency) (*VerifiedCurrency, error) {
	vc := &VerifiedCurrency{
		Ticker:              rc.Ticker,
		Slug:                strings.ToLower(rc.Slug),
		Name:                rc.Name,
		Description:         rc.Description,
		CoinGeckoID:         rc.CoinGeckoID,
		CoinMarketCapID:     rc.CoinMarketCapID,
		VerifiedIssuerLabel: rc.VerifiedIssuerLabel,
		Networks:            make([]NetworkEntry, 0, len(rc.Networks)),
	}
	for _, rn := range rc.Networks {
		if rn.Network == "" {
			return nil, fmt.Errorf("currency: %s: network entry missing `network`", rc.Ticker)
		}
		vc.Networks = append(vc.Networks, NetworkEntry{
			Network:      strings.ToLower(rn.Network),
			Code:         rn.Code,
			Issuer:       rn.Issuer,
			AssetID:      rn.AssetID,
			Contract:     rn.Contract,
			ExternalLink: rn.ExternalLink,
		})
	}
	return vc, nil
}

// indexStellarEntries threads every Stellar network entry into the
// asset_id + code indexes, surfacing collisions as errors.
func (cat *Catalogue) indexStellarEntries(vc *VerifiedCurrency) error {
	for _, n := range vc.Networks {
		if n.Network != "stellar" {
			continue
		}
		if n.AssetID != "" {
			if _, dup := cat.byStellarAssetID[n.AssetID]; dup {
				return fmt.Errorf("currency: duplicate stellar asset_id %q", n.AssetID)
			}
			cat.byStellarAssetID[n.AssetID] = vc
		}
		if n.Code != "" {
			codeKey := strings.ToUpper(n.Code)
			if existing, dup := cat.byStellarCode[codeKey]; dup {
				return fmt.Errorf(
					"currency: code %q claimed by both %q and %q on Stellar",
					n.Code, existing.Ticker, vc.Ticker)
			}
			cat.byStellarCode[codeKey] = vc
		}
	}
	return nil
}

// All returns every verified currency in the catalogue. The slice
// is shared — callers MUST NOT mutate. Order matches the order of
// entries in the seed file (deterministic).
func (c *Catalogue) All() []*VerifiedCurrency {
	return c.entries
}

// LookupBySlug returns the verified currency for a URL slug
// ("usdc"). Case-insensitive.
func (c *Catalogue) LookupBySlug(slug string) (*VerifiedCurrency, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.bySlug[strings.ToLower(slug)]
	return v, ok
}

// LookupByTicker returns the verified currency for a ticker
// ("USDC"). Case-insensitive.
func (c *Catalogue) LookupByTicker(ticker string) (*VerifiedCurrency, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.byTicker[strings.ToUpper(ticker)]
	return v, ok
}

// LookupByStellarAssetID returns the verified currency that owns
// the given canonical asset_id on Stellar (exact-match — includes
// "native", "CODE-G…", etc.). Returns (nil, false) for any
// unverified Stellar asset.
func (c *Catalogue) LookupByStellarAssetID(assetID string) (*VerifiedCurrency, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.byStellarAssetID[assetID]
	return v, ok
}

// StellarCollision reports whether (code, issuer) is an unverified
// ticker collision on Stellar: a verified currency claims this code
// on Stellar but its registered issuer is different.
//
// Returns (verified, true) when the code matches a verified Stellar
// entry but the issuer does not — the caller surfaces an
// unverified-collision warning.
// Returns (verified, false) when the code matches AND the issuer
// matches — this IS the verified asset, no warning.
// Returns (nil, false) when the code is not claimed by any
// verified currency on Stellar.
//
// Soroban contracts and the native asset are out of scope here.
// Callers passing empty code or issuer get (nil, false).
func (c *Catalogue) StellarCollision(code, issuer string) (*VerifiedCurrency, bool) {
	if c == nil || code == "" || issuer == "" {
		return nil, false
	}
	v, ok := c.byStellarCode[strings.ToUpper(code)]
	if !ok {
		return nil, false
	}
	for _, n := range v.Networks {
		if n.Network != "stellar" {
			continue
		}
		if strings.EqualFold(n.Code, code) && n.Issuer == issuer {
			return v, false
		}
	}
	return v, true
}

// StellarEntry returns the Stellar network entry for a verified
// currency, or nil if the currency has no Stellar issuance.
func (v *VerifiedCurrency) StellarEntry() *NetworkEntry {
	if v == nil {
		return nil
	}
	for i := range v.Networks {
		if v.Networks[i].Network == "stellar" {
			return &v.Networks[i]
		}
	}
	return nil
}

// CoinGeckoIDs returns the catalogue's CG mapping as
// upper-cased-ticker → CG slug, restricted to entries with a
// non-empty CoinGeckoID. Drives the CG poller's id-lookup table
// (R-018 Phase 1.2): adding a verified currency with a coingecko_id
// in the seed automatically expands the poller's coverage.
func (c *Catalogue) CoinGeckoIDs() map[string]string {
	if c == nil {
		return nil
	}
	out := make(map[string]string, len(c.entries))
	for _, vc := range c.entries {
		if vc.CoinGeckoID == "" {
			continue
		}
		out[strings.ToUpper(vc.Ticker)] = vc.CoinGeckoID
	}
	return out
}

// CoinMarketCapIDs returns the catalogue's CMC mapping as
// upper-cased-ticker → CMC integer-id string. Restricted to
// entries with a non-empty CoinMarketCapID. CMC's REST API accepts
// either the ticker (most reliable) or the id; we surface the id
// so future per-symbol resolution can disambiguate when needed.
func (c *Catalogue) CoinMarketCapIDs() map[string]string {
	if c == nil {
		return nil
	}
	out := make(map[string]string, len(c.entries))
	for _, vc := range c.entries {
		if vc.CoinMarketCapID == "" {
			continue
		}
		out[strings.ToUpper(vc.Ticker)] = vc.CoinMarketCapID
	}
	return out
}

// Tickers returns every catalogue ticker, upper-cased, in seed
// order. Used by the indexer to build the aggregator pair set
// (a pair-set targeting every verified ticker × the operator's
// fiat list).
func (c *Catalogue) Tickers() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.entries))
	for _, vc := range c.entries {
		out = append(out, strings.ToUpper(vc.Ticker))
	}
	return out
}
