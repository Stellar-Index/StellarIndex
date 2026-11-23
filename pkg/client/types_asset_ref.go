package client

import "time"

// GlobalAssetView is the [AssetLookup] branch [Client.Asset] returns for a verified-currency slug, distinct from the per-asset [AssetDetail].
type GlobalAssetView struct {
	// Kind is the wire-shape discriminator for the /v1/assets/{asset_id}
	// oneOf (ADR-0042). Always "catalogue" on this struct.
	Kind        string `json:"kind"`
	Ticker      string `json:"ticker"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Class is one of "crypto" / "stablecoin" / "fiat" — drives
	// listing taxonomies on consumers.
	Class          string `json:"class"`
	VerifiedIssuer string `json:"verified_issuer,omitempty"`
	CoinGeckoID    string `json:"coingecko_id,omitempty"`
	CMCID          string `json:"coinmarketcap_id,omitempty"`

	// Headline price block — all nil/empty together when no tier of the fallback chain produced a price.
	PriceUSD       *string    `json:"price_usd,omitempty"`
	PriceAuthority string     `json:"price_authority,omitempty"` // "vwap_native" | "aggregator_avg" | "triangulated" | "reference_rate" | "identity" | "onchain_listing"
	PriceSources   []string   `json:"price_sources,omitempty"`
	PriceAsOf      *time.Time `json:"price_as_of,omitempty"`
	// PriceCredit names the vendor when PriceSources is exactly one
	// licensed off-chain feed; a composite or on-chain price carries none.
	PriceCredit string `json:"price_credit,omitempty"`
	// PriceWithheldReason is set when PriceUSD is absent because a price gate refused it, not because no tier had data.
	PriceWithheldReason string `json:"price_withheld_reason,omitempty"`

	// Supply + market cap. Populated for fiat (M2 × FX rate).
	// Crypto/stablecoin market cap stays on /v1/assets/{asset_id}'s
	// F2 fields.
	CirculatingSupply *string `json:"circulating_supply,omitempty"`
	SupplyDecimals    int     `json:"supply_decimals,omitempty"`
	MarketCapUSD      *string `json:"market_cap_usd,omitempty"`

	// MarketCapLowLiquidity is true when market_cap_usd was suppressed
	// because the backing price came from negligible liquidity. Present for
	// wire parity with AssetDetail; the fiat market cap (M2 × deep FX rate)
	// is never dust-gated, so this stays false on catalogue rows today.
	MarketCapLowLiquidity bool `json:"market_cap_low_liquidity,omitempty"`
}

// AssetGlobalMarket is a vetted same-asset token's global-market USD price and the signed percentage its Stellar price diverges from it.
type AssetGlobalMarket struct {
	Asset                string    `json:"asset"`
	PriceUSD             string    `json:"price_usd"`
	Source               string    `json:"source"`
	Credit               string    `json:"credit,omitempty"`
	AsOf                 time.Time `json:"as_of"`
	StellarDivergencePct *string   `json:"stellar_divergence_pct,omitempty"`
	DepegWarning         bool      `json:"depeg_warning,omitempty"`
	// IssuerSignals names the issuer behaviours present beside a depeg
	// warning: "auth_clawback_enabled", "auth_revocable", "clawback_observed".
	IssuerSignals []string `json:"issuer_signals,omitempty"`
}

// AssetIssuerBehaviour is what a classic asset's issuer can do to holders (live account flags) and has done to supply; totals are integer strings in the asset's smallest unit.
type AssetIssuerBehaviour struct {
	AuthRequired        *bool   `json:"auth_required,omitempty"`
	AuthRevocable       *bool   `json:"auth_revocable,omitempty"`
	AuthClawbackEnabled *bool   `json:"auth_clawback_enabled,omitempty"`
	AuthImmutable       *bool   `json:"auth_immutable,omitempty"`
	FlagsAsOfLedger     *uint32 `json:"flags_as_of_ledger,omitempty"`
	MintTotal           *string `json:"mint_total,omitempty"`
	BurnTotal           *string `json:"burn_total,omitempty"`
	ClawbackTotal       *string `json:"clawback_total,omitempty"`
	SupplyFlowCount     *uint64 `json:"supply_flow_count,omitempty"`
}

// AssetListingReference is a listing platform's own USD price for the EXACT Stellar address of this asset, NOT this index's price, matched by classic id or derived SAC address and never by code alone.
type AssetListingReference struct {
	PriceUSD  string `json:"price_usd"`
	Source    string `json:"source"`
	ListingID string `json:"listing_id"`
	Quote     string `json:"quote"`
	// Address is the exact Stellar address the platform named — the whole
	// of the binding, published so a reader can check the join.
	Address string `json:"address"`
	// AddressForm is "classic" or "sac". A "sac" match is a DERIVED
	// address and exact rather than probable: SAC derivation is a pure
	// function of the asset and the network passphrase.
	AddressForm string `json:"address_form"`
	// AsOf is the PLATFORM's publication time, not when this index read
	// it — which is why a sync that succeeded minutes ago cannot launder
	// a price that froze weeks ago.
	AsOf       string `json:"as_of"`
	Stale      bool   `json:"stale,omitempty"`
	Provenance string `json:"provenance"`
}

// AssetListingValuation is circulating supply valued at a third party's listing price, NOT a market cap; never sum it with MarketCap without saying the total mixes two bases.
type AssetListingValuation struct {
	// Status is the single authority on why there is or is not a figure.
	// "listing_unavailable" means nobody looked; "not_listed" means the
	// directory was read and does not name it — the ordinary state of
	// almost every asset here, and not an accusation.
	Status   string  `json:"status"`
	ValueUSD *string `json:"value_usd,omitempty"`
	// CirculatingSupply is the multiplicand, in the asset's smallest
	// unit. Published here rather than inferred from the row's own field
	// because the two can legitimately differ.
	CirculatingSupply *string `json:"circulating_supply,omitempty"`
	// SupplyBasis is "lake_flows" or "served". The difference is
	// material: USDT0's trustline-visible supply is 6,469 tokens against
	// 2,581,052 by mint minus burn.
	SupplyBasis string `json:"supply_basis,omitempty"`
	// ValueLowerBound is true when the supply was a provable floor, so ValueUSD is a minimum, not the valuation.
	ValueLowerBound bool `json:"value_lower_bound,omitempty"`
}

// AssetTrust is the output-only trust score on [AssetDetail]; a factor with no evidence is "unknown" with nil Points and is excluded from Score, which is nil when no factor has evidence.
type AssetTrust struct {
	FormulaVersion int           `json:"formula_version"`
	Score          *int          `json:"score"`
	Band           string        `json:"band"` // "high" / "medium" / "low" / "unknown"
	CoveragePct    int           `json:"coverage_pct"`
	Factors        []TrustFactor `json:"factors"`
}

// TrustFactor is one weighted input to [AssetTrust].
type TrustFactor struct {
	ID       string `json:"id"`
	Band     string `json:"band"`
	Points   *int   `json:"points"`
	Weight   int    `json:"weight"`
	Source   string `json:"source"`
	Observed string `json:"observed"`
}

// VerifiedCurrencyListItem is one identity-only row of `GET /v1/assets/verified` (no Client method yet); pricing needs [Client.Asset] with its Slug.
type VerifiedCurrencyListItem struct {
	Ticker string `json:"ticker"`
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	// Class is one of "crypto" / "stablecoin" / "fiat" — drives the
	// listing taxonomy on consumers.
	Class          string `json:"class"`
	VerifiedIssuer string `json:"verified_issuer,omitempty"`
	// Image is the asset's logo URL from the issuer's SEP-1 TOML
	// (https-only, sanitized). Empty when unavailable.
	Image             string `json:"image,omitempty"`
	CoinGeckoID       string `json:"coingecko_id,omitempty"`
	CMCID             string `json:"coinmarketcap_id,omitempty"`
	CirculatingSupply string `json:"circulating_supply,omitempty"`
	SupplyDecimals    int    `json:"supply_decimals,omitempty"`
	// MarketCapUSD is computed for fiat rows only (M2 × current FX
	// rate). Empty for crypto/stablecoin rows. Decimal string with
	// 2 fractional digits.
	MarketCapUSD string `json:"market_cap_usd,omitempty"`
}

// PricePoint is one sparkline sample in [AssetDetail]'s price_history_24h / price_history_7d arrays.
type PricePoint struct {
	T time.Time `json:"t"`
	P *string   `json:"p,omitempty"`
}

// ATHPoint is [AssetDetail]'s all-time-high marker.
type ATHPoint struct {
	USD string    `json:"usd"`
	At  time.Time `json:"at"`
}

// AssetTopMarket is one row of [AssetDetail].TopMarkets.
type AssetTopMarket struct {
	Counterparty  string  `json:"counterparty"`
	Side          string  `json:"side"`
	Volume24hUSD  *string `json:"volume_24h_usd,omitempty"`
	TradeCount24h int     `json:"trade_count_24h"`
}
