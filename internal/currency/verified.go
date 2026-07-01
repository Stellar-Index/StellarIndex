package currency

import (
	_ "embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed data/seed.yaml
var seedYAML []byte

// AssetClass classifies a verified currency for the
// everything-is-an-asset routing model (operator decision 2026-05-11).
// Crypto is the default; new classes plug in as ingestion lights up
// for them. Stocks / metals / commodities / funds are future-scope
// placeholders.
type AssetClass string

const (
	// ClassCrypto — non-pegged cryptocurrencies (BTC, ETH, XLM, AQUA, …).
	// Default for catalogue entries without an explicit class.
	ClassCrypto AssetClass = "crypto"
	// ClassStablecoin — fiat-pegged crypto (USDC, USDT, EURC, PYUSD, …).
	// Distinct from `fiat` so the explorer can render the peg
	// relationship + depeg-risk surface differently from a pure fiat rate.
	ClassStablecoin AssetClass = "stablecoin"
	// ClassFiat — sovereign currencies (USD, EUR, GBP, JPY, CNY, …).
	// Circulating supply (M2; see seed.yaml comment) lets the
	// explorer surface a fiat market cap (M2 × FX rate).
	ClassFiat AssetClass = "fiat"
)

// IsKnownClass returns true when c is one of the values defined
// above. Used by the loader's validation step to fail loudly on a
// typo'd class rather than silently treating an unknown class as
// "crypto".
func IsKnownClass(c AssetClass) bool {
	switch c {
	case ClassCrypto, ClassStablecoin, ClassFiat:
		return true
	default:
		return false
	}
}

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
	// Class places this currency in one of the asset-class buckets
	// (crypto / stablecoin / fiat). Drives explorer rendering and
	// the asset-listing taxonomy. Defaults to ClassCrypto when the
	// seed entry omits it.
	Class AssetClass
	// CirculatingSupply is the total amount in circulation, expressed
	// in the natural unit of the currency: stroops for XLM (10⁷),
	// dollars for USD / EUR / etc. (10⁰), and so on per
	// SupplyDecimals below. Operator-curated and approximate — the
	// per-class semantic is documented per entry in seed.yaml:
	//   - Crypto / stablecoin: actual on-chain circulating supply
	//     (today usually unset; the per-Stellar-asset F2 fields
	//     supersede when available).
	//   - Fiat: M2 (broad money supply: physical cash + checkable
	//     deposits + savings + money-market funds), per central-bank
	//     reporting. Operators should refresh quarterly.
	// Empty string means "no supply data available".
	CirculatingSupply string
	// SupplyDecimals is the divisor exponent that maps
	// CirculatingSupply (smallest integer unit) to display value.
	// 7 for XLM (stroops → XLM); 0 for fiat (raw dollars / yen /
	// yuan). Zero default works for fiat.
	SupplyDecimals int
	// Networks holds this currency's Stellar issuance identity. After
	// the Stellar-focus refactor each browseable entry has at most one
	// entry — the `stellar` one (code / issuer / asset_id; native XLM
	// uses asset_id "native"). Fiat + reference_only entries carry no
	// network entries. Iterated by indexStellarEntries /
	// LookupByStellarAssetID / StellarCollision to map an asset_id to
	// its verified currency.
	Networks []NetworkEntry
	// ReferenceOnly marks a non-Stellar entry that exists solely as a
	// pricing cross-check reference (its coingecko_id / coinmarketcap_id
	// feed the divergence/aggregator pair set), NOT as a browseable
	// Stellar asset. These are excluded from the /v1/assets browse
	// listings + /v1/assets/verified, but KEPT in CoinGeckoIDs() /
	// CoinMarketCapIDs() so the protected reference-price pipeline is
	// unaffected. Set via `reference_only: true` in seed.yaml.
	ReferenceOnly bool
}

// NetworkEntry is the Stellar issuance identity for a verified
// currency. After the Stellar-focus refactor `Network` is always
// "stellar" for any populated entry.
type NetworkEntry struct {
	// Network identifier — short, lowercase, stable across versions.
	// Always "stellar" for browseable entries.
	Network string
	// Stellar issuance fields. Populated when Network == "stellar"
	// for classic assets. For native XLM, AssetID == "native" and
	// Code / Issuer are empty.
	Code    string
	Issuer  string
	AssetID string
	// Contract / ExternalLink are vestigial fields retained on the
	// raw shape for backward-compatible YAML parsing; the
	// Stellar-focus refactor stripped all non-Stellar network
	// entries from the seed, so these are unused on populated
	// (stellar) entries.
	Contract     string
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
	Class               string       `yaml:"class"`
	CirculatingSupply   string       `yaml:"circulating_supply"`
	SupplyDecimals      int          `yaml:"supply_decimals"`
	Networks            []rawNetwork `yaml:"networks"`
	ReferenceOnly       bool         `yaml:"reference_only"`
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
// the production wiring in cmd/stellarindex-api/main.go.
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
	case len(rc.Networks) == 0 && rc.Class != string(ClassFiat) && !rc.ReferenceOnly:
		// Fiat entries are network-agnostic (sovereign currencies
		// have no on-chain issuance). reference_only coins (BTC/ETH/…)
		// have no Stellar issuance — they exist solely as a pricing
		// cross-check reference, so they're allowed zero networks. Every
		// other class is a browseable Stellar asset and MUST carry its
		// Stellar network entry.
		return fmt.Errorf("currency: entry %d (%s): a Stellar network entry is required (non-fiat, non-reference_only)", i, rc.Ticker)
	}
	return nil
}

func buildVerifiedCurrency(rc rawCurrency) (*VerifiedCurrency, error) {
	class := AssetClass(rc.Class)
	if class == "" {
		class = ClassCrypto
	}
	if !IsKnownClass(class) {
		return nil, fmt.Errorf(
			"currency: %s: unknown class %q (allowed: crypto, stablecoin, fiat)",
			rc.Ticker, rc.Class)
	}
	vc := &VerifiedCurrency{
		Ticker:              rc.Ticker,
		Slug:                strings.ToLower(rc.Slug),
		Name:                rc.Name,
		Description:         rc.Description,
		CoinGeckoID:         rc.CoinGeckoID,
		CoinMarketCapID:     rc.CoinMarketCapID,
		VerifiedIssuerLabel: rc.VerifiedIssuerLabel,
		Class:               class,
		CirculatingSupply:   rc.CirculatingSupply,
		SupplyDecimals:      rc.SupplyDecimals,
		ReferenceOnly:       rc.ReferenceOnly,
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

// Browseable returns the catalogue entries that are browseable Stellar
// assets — i.e. every entry EXCEPT those marked ReferenceOnly (pure
// pricing-reference coins like BTC / ETH that have no Stellar issuance
// and exist only to feed the divergence/aggregator cross-check). This
// is the set the /v1/assets browse listings + /v1/assets/verified
// surface; it is the Stellar-focus filter. CoinGeckoIDs() /
// CoinMarketCapIDs() deliberately do NOT use it — the reference coins
// must still drive the protected reference-price pipeline. Order is
// preserved; the returned slice is freshly allocated.
func (c *Catalogue) Browseable() []*VerifiedCurrency {
	if c == nil {
		return nil
	}
	out := make([]*VerifiedCurrency, 0, len(c.entries))
	for _, vc := range c.entries {
		if vc.ReferenceOnly {
			continue
		}
		out = append(out, vc)
	}
	return out
}

// StellarIssued returns the catalogue entries that have a Stellar on-chain
// issuance (StellarEntry() != nil) — the set that belongs on /v1/assets after
// the Stellar/external split (LC-001). This EXCLUDES both reference-only coins
// (BTC/ETH) AND fiat currencies (USD/EUR) — both have no Stellar issuance, so
// they live on /v1/external/assets. (Browseable only drops reference-only, so
// it still leaks fiat into the Stellar listing — do not use it for the browse
// surface.) Order preserved; freshly allocated.
func (c *Catalogue) StellarIssued() []*VerifiedCurrency {
	if c == nil {
		return nil
	}
	out := make([]*VerifiedCurrency, 0, len(c.entries))
	for _, vc := range c.entries {
		if vc.StellarEntry() != nil {
			out = append(out, vc)
		}
	}
	return out
}

// External returns the catalogue entries with NO Stellar issuance
// (StellarEntry() == nil): fiat currencies + reference-only coins. This is the
// set for /v1/external/assets (LC-001). Order preserved; freshly allocated.
func (c *Catalogue) External() []*VerifiedCurrency {
	if c == nil {
		return nil
	}
	out := make([]*VerifiedCurrency, 0, len(c.entries))
	for _, vc := range c.entries {
		if vc.StellarEntry() == nil {
			out = append(out, vc)
		}
	}
	return out
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

// ByClass returns every catalogue entry of the given class, in
// seed order. Returns nil for the unknown / empty class.
func (c *Catalogue) ByClass(class AssetClass) []*VerifiedCurrency {
	if c == nil {
		return nil
	}
	out := make([]*VerifiedCurrency, 0, len(c.entries))
	for _, vc := range c.entries {
		if vc.Class == class {
			out = append(out, vc)
		}
	}
	return out
}
