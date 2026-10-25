package currency

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
)

//go:embed data/seed.yaml
var seedYAML []byte

// AssetClass classifies a verified currency for the everything-is-an-asset routing model; Crypto is the default, others are future-scope placeholders.
type AssetClass string

const (
	// ClassCrypto — non-pegged cryptocurrencies (BTC, ETH, XLM, AQUA, …); the default class.
	ClassCrypto AssetClass = "crypto"
	// ClassStablecoin — fiat-pegged crypto (USDC, USDT, EURC, …), distinct from fiat so the
	// explorer can render the peg and depeg risk.
	ClassStablecoin AssetClass = "stablecoin"
	// ClassFiat — sovereign currencies (USD, EUR, GBP, …); M2 supply gives a fiat market cap.
	ClassFiat AssetClass = "fiat"
)

// IsKnownClass reports whether c is a defined class, so the loader fails loudly on a typo instead of treating it as "crypto".
func IsKnownClass(c AssetClass) bool {
	switch c {
	case ClassCrypto, ClassStablecoin, ClassFiat:
		return true
	default:
		return false
	}
}

// VerifiedCurrency is one verified-currency catalogue entry, pointer-shared across every index so LookupBySlug and LookupByTicker return the same instance.
type VerifiedCurrency struct {
	Ticker              string
	Slug                string
	Name                string
	Description         string
	CoinGeckoID         string
	CoinMarketCapID     string
	VerifiedIssuerLabel string
	// Class is the asset-class bucket; defaults to ClassCrypto when the seed entry omits it.
	Class AssetClass
	// CirculatingSupply is a decimal string in the smallest integer unit, scaled per
	// SupplyDecimals; operator-curated and approximate. Crypto is usually unset (per-asset supply
	// supersedes it); fiat is M2 broad money. Empty means no supply data.
	CirculatingSupply string
	// SupplyDecimals is the exponent mapping CirculatingSupply to display value: 7 for XLM, 0 for fiat.
	SupplyDecimals int
	// Issuance holds this currency's Stellar issuance identity: at most one `stellar` entry
	// (code / issuer / asset_id; native XLM uses asset_id "native"). Fiat and reference_only
	// entries carry none. Iterated to map an asset_id to its verified currency.
	Issuance []IssuanceEntry
	// ReferenceOnly marks a non-Stellar entry that exists solely as a pricing cross-check. It is
	// excluded from browse listings and /v1/assets/verified, but kept in CoinGeckoIDs() /
	// CoinMarketCapIDs() so the reference-price pipeline still sees it.
	ReferenceOnly bool
	// KnownAnchorIssuers lists Stellar accounts recognised as SEP-1 anchors for this fiat
	// denomination (ClassFiat with no Stellar issuance only); empty means none curated yet, not
	// that no legitimate anchor exists. Consulted by FiatCodeAnchor, never by StellarCollision.
	KnownAnchorIssuers []string
}

// IssuanceEntry is the Stellar issuance identity of a verified currency; Network is always "stellar" when populated.
type IssuanceEntry struct {
	// Network is short, lowercase and stable.
	Network string
	// For native XLM, AssetID is "native" and Code / Issuer are empty.
	Code    string
	Issuer  string
	AssetID string
	// Contract / ExternalLink are vestigial fields kept so older YAML still parses; they are
	// unused on populated (stellar) entries.
	Contract     string
	ExternalLink string
}

// Catalogue indexes the verified currencies for API lookups; built once at startup and never mutated, so lookups are safe for concurrent use.
type Catalogue struct {
	entries          []*VerifiedCurrency
	bySlug           map[string]*VerifiedCurrency // lowercase
	byTicker         map[string]*VerifiedCurrency // uppercase
	byStellarAssetID map[string]*VerifiedCurrency // exact-match
	// byStellarCode maps an uppercase classic code to its verified currency; StellarCollision
	// looks up by code and compares the issuer.
	byStellarCode map[string]*VerifiedCurrency
	// byFiatCode maps an uppercase ISO-4217 code to its sovereign currency entry. Disjoint from
	// byStellarCode (see indexTickerOnlyEntry): a classic asset carrying a fiat ticker is
	// denominating, not impersonating, and must not go through StellarCollision.
	byFiatCode map[string]*VerifiedCurrency
}

// rawCatalogue is the on-disk shape of seed.yaml; unmarshalling
// happens here so callers can keep the typed Catalogue API clean.
type rawCatalogue struct {
	VerifiedCurrencies []rawCurrency `yaml:"verified_currencies"`
}

type rawCurrency struct {
	Ticker              string        `yaml:"ticker"`
	Slug                string        `yaml:"slug"`
	Name                string        `yaml:"name"`
	Description         string        `yaml:"description"`
	CoinGeckoID         string        `yaml:"coingecko_id"`
	CoinMarketCapID     string        `yaml:"coinmarketcap_id"`
	VerifiedIssuerLabel string        `yaml:"verified_issuer_label"`
	Class               string        `yaml:"class"`
	CirculatingSupply   string        `yaml:"circulating_supply"`
	SupplyDecimals      int           `yaml:"supply_decimals"`
	Issuance            []rawIssuance `yaml:"networks"`
	ReferenceOnly       bool          `yaml:"reference_only"`
	KnownAnchors        []string      `yaml:"known_anchors"`
}

type rawIssuance struct {
	Network      string `yaml:"network"`
	Code         string `yaml:"code"`
	Issuer       string `yaml:"issuer"`
	AssetID      string `yaml:"asset_id"`
	Contract     string `yaml:"contract"`
	ExternalLink string `yaml:"external_link"`
}

// LoadEmbedded parses the binary-embedded seed catalogue (production wiring).
func LoadEmbedded() (*Catalogue, error) {
	return LoadFromBytes(seedYAML)
}

// LoadEmbeddedForNetwork is LoadEmbedded scoped to one network ([sourcenet] names): on a test net each entry's
// `network: stellar-<net>` issuance serves as its Stellar issuance, native XLM and entries with no issuance (fiat) are kept,
// and every other entry is dropped because its issuer is a pubnet account. Any other name loads the pubnet catalogue.
func LoadEmbeddedForNetwork(network string) (*Catalogue, error) {
	return LoadFromBytesForNetwork(seedYAML, network)
}

// LoadFromBytes parses an arbitrary YAML blob (tests, operator-config override).
func LoadFromBytes(b []byte) (*Catalogue, error) {
	return LoadFromBytesForNetwork(b, sourcenet.Pubnet)
}

// LoadFromBytesForNetwork parses a YAML blob scoped to network (see [LoadEmbeddedForNetwork]).
func LoadFromBytesForNetwork(b []byte, network string) (*Catalogue, error) {
	var raw rawCatalogue
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true) // reject a misspelled/renamed key instead of silently dropping it
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("currency: yaml parse: %w", err)
	}
	var entries []rawCurrency
	if network == sourcenet.Testnet || network == sourcenet.Futurenet {
		entries = scopeToTestNet(raw.VerifiedCurrencies, network)
	} else {
		entries = dropTestNetIssuances(raw.VerifiedCurrencies)
	}

	cat := &Catalogue{
		bySlug:           make(map[string]*VerifiedCurrency, len(entries)),
		byTicker:         make(map[string]*VerifiedCurrency, len(entries)),
		byStellarAssetID: make(map[string]*VerifiedCurrency),
		byStellarCode:    make(map[string]*VerifiedCurrency),
		byFiatCode:       make(map[string]*VerifiedCurrency),
	}

	for i, rc := range entries {
		if err := cat.append(i, rc); err != nil {
			return nil, err
		}
	}
	return cat, nil
}

// dropTestNetIssuances strips every `stellar-<test net>` issuance, leaving the pubnet catalogue.
func dropTestNetIssuances(entries []rawCurrency) []rawCurrency {
	out := make([]rawCurrency, len(entries))
	for i, rc := range entries {
		kept := make([]rawIssuance, 0, len(rc.Issuance))
		for _, n := range rc.Issuance {
			if !strings.HasPrefix(strings.ToLower(n.Network), "stellar-") {
				kept = append(kept, n)
			}
		}
		if len(rc.Issuance) > 0 {
			rc.Issuance = kept
		}
		out[i] = rc
	}
	return out
}

// scopeToTestNet keeps the entries that exist on a test net, each with that net's issuance relabelled `stellar`.
func scopeToTestNet(entries []rawCurrency, network string) []rawCurrency {
	want := "stellar-" + network
	out := make([]rawCurrency, 0, len(entries))
	for _, rc := range entries {
		if len(rc.Issuance) == 0 {
			out = append(out, rc)
			continue
		}
		var keep *rawIssuance
		for i := range rc.Issuance {
			n := rc.Issuance[i]
			switch {
			case strings.EqualFold(n.Network, want):
				keep = &n
			case keep == nil && strings.EqualFold(n.Network, "stellar") && n.AssetID == nativeAssetID:
				keep = &n
			}
		}
		if keep == nil {
			continue
		}
		keep.Network = "stellar"
		rc.Issuance = []rawIssuance{*keep}
		out = append(out, rc)
	}
	return out
}

// append validates one raw entry, builds the typed *VerifiedCurrency and indexes it.
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
	case len(rc.Issuance) == 0 && rc.Class != string(ClassFiat) && !rc.ReferenceOnly:
		// Fiat and reference_only entries have no Stellar issuance; every other class MUST carry one.
		return fmt.Errorf("currency: entry %d (%s): a Stellar network entry is required (non-fiat, non-reference_only)", i, rc.Ticker)
	case len(rc.KnownAnchors) > 0 && (rc.Class != string(ClassFiat) || len(rc.Issuance) > 0):
		// Only FiatCodeAnchor reads known_anchors; anywhere else it would look
		// like an impersonation allowlist while StellarCollision ignores it.
		return fmt.Errorf("currency: entry %d (%s): known_anchors is only valid on a fiat entry with no networks; "+
			"vet a genuine Stellar issuance as a `network: stellar` entry instead", i, rc.Ticker)
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
		KnownAnchorIssuers:  rc.KnownAnchors,
		Issuance:            make([]IssuanceEntry, 0, len(rc.Issuance)),
	}
	for _, rn := range rc.Issuance {
		if rn.Network == "" {
			return nil, fmt.Errorf("currency: %s: network entry missing `network`", rc.Ticker)
		}
		vc.Issuance = append(vc.Issuance, IssuanceEntry{
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

// nativeAssetID is XLM's canonical asset_id. It is kept local to avoid an import cycle with
// internal/canonical.
const nativeAssetID = "native"

// indexStellarEntries threads every Stellar network entry into the
// asset_id + code indexes, surfacing collisions as errors.
func (cat *Catalogue) indexStellarEntries(vc *VerifiedCurrency) error {
	indexed := false
	for _, n := range vc.Issuance {
		if n.Network != "stellar" {
			continue
		}
		if n.AssetID != "" {
			if _, dup := cat.byStellarAssetID[n.AssetID]; dup {
				return fmt.Errorf("currency: duplicate stellar asset_id %q", n.AssetID)
			}
			cat.byStellarAssetID[n.AssetID] = vc
		}
		codeKey := strings.ToUpper(n.Code)
		if n.Code == "" && n.AssetID == nativeAssetID {
			// Native XLM has no classic issuer, so it would never enter this index, yet every
			// classic "XLM-G…" is an impersonator and would wear the "Verified currency" badge
			// unwarned. Index the TICKER so StellarCollision flags every issuer.
			codeKey = strings.ToUpper(vc.Ticker)
		}
		if codeKey != "" {
			if existing, dup := cat.byStellarCode[codeKey]; dup {
				return fmt.Errorf(
					"currency: code %q claimed by both %q and %q on Stellar",
					codeKey, existing.Ticker, vc.Ticker)
			}
			cat.byStellarCode[codeKey] = vc
			indexed = true
		}
	}
	if !indexed {
		return cat.indexTickerOnlyEntry(vc)
	}
	return nil
}

// indexTickerOnlyEntry indexes, by ticker, an entry with no classic Stellar code (no issuance, or a Soroban-only one), so
// StellarCollision can speak about `reference_only` entries (USDT, BTC): no legitimate classic asset can bear a ticker verified as
// off-Stellar. A wrapper gets no ticker allowlist (see docs/architecture/domain-traps.md); add its issuance as a `network: stellar`
// entry on the SAME seed.yaml entry. A SOVEREIGN CURRENCY (ClassFiat) lands in byFiatCode: a regulated `USD-G...` is no impersonator.
func (cat *Catalogue) indexTickerOnlyEntry(vc *VerifiedCurrency) error {
	codeKey := strings.ToUpper(vc.Ticker)
	if codeKey == "" {
		return nil
	}
	// A code-less (Soroban-only) issuance is a Stellar identity; only an issuance-free fiat entry
	// is a denomination.
	if vc.Class == ClassFiat && vc.StellarEntry() == nil {
		if existing, dup := cat.byFiatCode[codeKey]; dup {
			return fmt.Errorf(
				"currency: fiat code %q claimed by both %q and %q",
				codeKey, existing.Ticker, vc.Ticker)
		}
		cat.byFiatCode[codeKey] = vc
		return nil
	}
	if existing, dup := cat.byStellarCode[codeKey]; dup {
		return fmt.Errorf(
			"currency: ticker %q claimed by both %q and %q",
			codeKey, existing.Ticker, vc.Ticker)
	}
	cat.byStellarCode[codeKey] = vc
	return nil
}

// All returns every verified currency in seed-file order; the slice is shared, callers MUST NOT mutate.
func (c *Catalogue) All() []*VerifiedCurrency {
	return c.entries
}

// Browseable returns every entry except ReferenceOnly ones (the /v1/assets browse set), in order, freshly allocated; CoinGeckoIDs/CoinMarketCapIDs deliberately do NOT use it because reference coins must still drive reference pricing.
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

// StellarIssued returns entries with a Stellar on-chain issuance (the /v1/assets set), excluding reference-only coins and fiat (which live on /v1/external/assets); Browseable would leak fiat here. Order preserved, freshly allocated.
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

// External returns entries with NO Stellar issuance (fiat and reference-only coins), the /v1/external/assets set; order preserved, freshly allocated.
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

// LookupBySlug returns the verified currency for a URL slug, case-insensitive.
func (c *Catalogue) LookupBySlug(slug string) (*VerifiedCurrency, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.bySlug[strings.ToLower(slug)]
	return v, ok
}

// LookupByTicker returns the verified currency for a ticker, case-insensitive.
func (c *Catalogue) LookupByTicker(ticker string) (*VerifiedCurrency, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.byTicker[strings.ToUpper(ticker)]
	return v, ok
}

// LookupByStellarAssetID returns the verified currency owning the exact canonical Stellar asset_id (incl. "native"), or (nil, false) for any unverified asset.
func (c *Catalogue) LookupByStellarAssetID(assetID string) (*VerifiedCurrency, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.byStellarAssetID[assetID]
	return v, ok
}

// StellarCollision reports whether (code, issuer) is an unverified ticker collision: (verified, true) for a code match with a different issuer, (verified, false) when both match, (nil, false) if the code is unclaimed or either is empty; a classic "XLM" always collides, a fiat-only denomination never does.
func (c *Catalogue) StellarCollision(code, issuer string) (*VerifiedCurrency, bool) {
	if c == nil || code == "" || issuer == "" {
		return nil, false
	}
	v, ok := c.byStellarCode[strings.ToUpper(code)]
	if !ok {
		return nil, false
	}
	for _, n := range v.Issuance {
		if n.Network != "stellar" {
			continue
		}
		if strings.EqualFold(n.Code, code) && n.Issuer == issuer {
			return v, false
		}
	}
	return v, true
}

// FiatDenomination returns the sovereign-currency entry a classic code denominates in (case-insensitive); a denomination is not an identity claim, so it carries no impersonation verdict and must not gate a valuation; (nil, false) for unknown codes and fiat with a verified Stellar issuance (StellarCollision's).
func (c *Catalogue) FiatDenomination(code string) (*VerifiedCurrency, bool) {
	if c == nil || code == "" {
		return nil, false
	}
	v, ok := c.byFiatCode[strings.ToUpper(code)]
	return v, ok
}

// FiatCodeAnchor reports (denomination, knownAnchor, ok) for a fiat code and issuer: not a denomination gives (nil, false, false); an uncurated issuer gives a neutral "declared, unverified anchor" (denom, false, true), never an impersonation warning or market-cap suppression.
func (c *Catalogue) FiatCodeAnchor(code, issuer string) (denomination *VerifiedCurrency, knownAnchor, ok bool) {
	if c == nil || issuer == "" {
		return nil, false, false
	}
	v, ok := c.FiatDenomination(code)
	if !ok {
		return nil, false, false
	}
	for _, a := range v.KnownAnchorIssuers {
		if a == issuer {
			return v, true, true
		}
	}
	return v, false, true
}

// StellarEntry returns the Stellar issuance entry, or nil if the currency has none.
func (v *VerifiedCurrency) StellarEntry() *IssuanceEntry {
	if v == nil {
		return nil
	}
	for i := range v.Issuance {
		if v.Issuance[i].Network == "stellar" {
			return &v.Issuance[i]
		}
	}
	return nil
}

// CoinGeckoIDs returns upper-cased ticker to CG slug for entries with a CoinGeckoID; it drives the CG poller's id table, so a new seed coingecko_id expands coverage automatically.
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

// CoinMarketCapIDs returns upper-cased ticker to CMC integer-id string for entries with a CoinMarketCapID, so per-symbol resolution can disambiguate.
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

// Tickers returns every catalogue ticker, upper-cased, in seed order; the indexer builds the aggregator pair set from it.
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

// ByClass returns every entry of the given class in seed order, nil for an unknown or empty class.
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
