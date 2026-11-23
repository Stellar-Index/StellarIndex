package client

import (
	"encoding/json"
	"time"
)

// Envelope is the shape of every 2xx JSON response, parameterised on the data type.
type Envelope[T any] struct {
	Data T         `json:"data"`
	AsOf time.Time `json:"as_of"`
	// CoverageFrom is the earliest instant the server holds price history for the requested pair. Present on the
	// windowed surfaces (History, Chart, OHLC) when the server could establish it. Nil means UNKNOWN, never "from the
	// beginning of time"; a consumer needs it to tell a dead market from a window that predates the server's data.
	CoverageFrom *time.Time `json:"coverage_from,omitempty"`
	// Withheld names the ids [Client.PriceBatch] omitted from Data because the server declined to publish their price (thin market,
	// flagged issuer, withheld upstream leg). An id in none of Data, Withheld and Unread has no price data. Batch only.
	Withheld []string `json:"withheld,omitempty"`
	// Unread names the ids [Client.PriceBatch] omitted from Data because their price could not be read: unknown, not absent.
	Unread []string `json:"unread,omitempty"`
	// Thin names the ids in Data served from a market below the
	// substance floor under the include_thin opt-in (batch only).
	Thin []string `json:"thin,omitempty"`
	// Stale and Triangulated name the ids in Data behind the OR-ed flags of
	// the same name (batch only).
	Stale        []string `json:"stale,omitempty"`
	Triangulated []string `json:"triangulated,omitempty"`
	// UnderReviewReason is the operator-supplied reason; present exactly when Flags.UnderReview is true.
	UnderReviewReason string   `json:"under_review_reason,omitempty"`
	Sources           []string `json:"sources,omitempty"`
	Flags             Flags    `json:"flags"`
	// Pagination is a POINTER to match the server's wire shape (nil means absent): a value type makes `omitempty`
	// a no-op, so re-encoding a non-list response would emit `"pagination":{}`. Consumers must nil-check before
	// reading `.Next`.
	Pagination *Pagination `json:"pagination,omitempty"`
}

// validateEnvelope checks the one invariant every server 2xx envelope satisfies: a populated as_of (stamped by
// internal/api/v1.writeEnvelopeStatus). A zero as_of means the body was not a real envelope. [doJSON] type-asserts this method.
func (e *Envelope[T]) validateEnvelope() error {
	if e.AsOf.IsZero() {
		return errNotAnEnvelope
	}
	return nil
}

// Flags are the advisory quality markers of the response envelope; unknown flags are ignored by the decoder, so new ones are non-breaking.
type Flags struct {
	Stale             bool `json:"stale"`
	ReducedRedundancy bool `json:"reduced_redundancy"`
	Triangulated      bool `json:"triangulated"`
	DivergenceWarning bool `json:"divergence_warning"`

	// DivergenceChecked reports whether the cross-reference divergence check reached a verdict at all. When false
	// the check was blind, so a false warning must not be read as "prices agree"; a true warning is the last
	// evaluated verdict carried forward, not a fresh one. A consumer gating only on !DivergenceWarning cannot
	// otherwise detect the blindness.
	DivergenceChecked bool `json:"divergence_checked"`
	// OutsideCoverage marks an empty answer whose requested range ends at or before the envelope's CoverageFrom:
	// the window predates the server's history for the pair, so there was nothing to return. Lets a consumer
	// distinguish the two meanings of an empty series.
	OutsideCoverage bool `json:"outside_coverage,omitempty"`
	Frozen          bool `json:"frozen,omitempty"`
	// FrozenChecked reports whether the freeze marker was actually read. When false, Frozen is NOT meaningful (the
	// looker is not wired or the read failed), so a false Frozen must not be read as "confirmed not frozen". Same
	// shape as DivergenceChecked.
	FrozenChecked bool `json:"frozen_checked,omitempty"`
	SingleSource  bool `json:"single_source,omitempty"`
	// Diverged is set on a TRIANGULATED /v1/price response when the
	// composite came from routes that disagreed (the aggregator's
	// router divergence signal). Mirrors the server's envelope flag;
	// omitempty hides it when false.
	Diverged bool `json:"diverged,omitempty"`
	// Rerouted is set on a TRIANGULATED /v1/price response when the
	// composite substituted around a dry configured chain leg — the
	// price came via an alternative path, not the documented direct
	// chain. Mirrors the server's envelope flag; omitempty hides it
	// when false.
	Rerouted bool `json:"rerouted,omitempty"`
	// PivotUnverified: a composite leg was all stablecoin prints at par, so a de-peg in it went unchecked.
	PivotUnverified bool `json:"pivot_unverified,omitempty"`
	// ThinMarket: a served price comes from a market below the substance floor (include_thin opt-in).
	ThinMarket bool `json:"thin_market,omitempty"`
	// UnderReview: an operator hold covers the asset, contract or ledger; figures are served unchanged but unconfirmed.
	UnderReview bool `json:"under_review,omitempty"`
	// ProxyDeviation: a triangulated fiat:USD price was served through a USD peg while a declared peg trades off $1. Read it only
	// when ProxyDeviationUnchecked is false: that marks a peg not observed (none in 24 h, withheld by a guard, or a failed read).
	ProxyDeviation          bool `json:"proxy_deviation,omitempty"`
	ProxyDeviationUnchecked bool `json:"proxy_deviation_unchecked,omitempty"`
	// UnverifiedTickerCollision fires on `/v1/assets/{id}` when the requested asset's code matches a verified currency's
	// Stellar ticker but its issuer does not match the verified entry, i.e. someone issued their own "USDC".
	UnverifiedTickerCollision bool `json:"unverified_ticker_collision,omitempty"`
	// DetectorsPartial fires on `/v1/mev` when the detection worker has not completed a full run within its heartbeat TTL,
	// so a quiet feed is a detection gap rather than a quiet network.
	DetectorsPartial bool `json:"detectors_partial,omitempty"`
	// Truncated: the list was capped and no cursor continues it, so its length is a lower bound.
	Truncated bool `json:"truncated,omitempty"`
	// FiltersIgnored names the row-narrowing query parameters the response did NOT apply, spelled as the caller
	// sent them ("type", "code", "issuer", "q"); omitted when everything supplied was applied. `/v1/assets` sets it
	// on listings that cannot narrow. A page that dropped a filter otherwise looks identical to one that matched.
	FiltersIgnored []string `json:"filters_ignored,omitempty"`
	// IncludesUnread names the `include` sections asked for and not read; their fields are absent, not zero (`/v1/sources`, `/v1/markets`).
	IncludesUnread []string `json:"includes_unread,omitempty"`
	// FillsWithheldReason is set when fills, or chart points built from them, were removed ("source_excluded": an operator
	// excluded their source): history/chart rows are missing, vwap/twap/ohlc recomputed from the other sources.
	FillsWithheldReason string `json:"fills_withheld_reason,omitempty"`
}

// Pagination is present on list endpoints when more results than the limit exist, and nil on the terminal page.
type Pagination struct {
	Next string `json:"next,omitempty"`
}

// PriceSnapshot is the data shape returned by [Client.Price].
type PriceSnapshot struct {
	AssetID       string    `json:"asset_id"`
	Quote         string    `json:"quote"`
	Price         string    `json:"price"`
	PriceType     string    `json:"price_type"`
	ObservedAt    time.Time `json:"observed_at"`
	WindowSeconds int       `json:"window_seconds,omitempty"`
	// Truncated (nil = unknown): the window read hit the row cap; see CoveredFrom.
	Truncated   *bool      `json:"truncated,omitempty"`
	CoveredFrom *time.Time `json:"covered_from,omitempty"`

	// FXRate, FXAsOf, FXSource and FXResolution describe the vendor FX
	// fixing a closed-surface fiat cross converted at; FXRate is quote
	// units per 1 USD. All omitted on a price that needed no conversion.
	FXRate       string     `json:"fx_rate,omitempty"`
	FXAsOf       *time.Time `json:"fx_as_of,omitempty"`
	FXSource     string     `json:"fx_source,omitempty"`
	FXResolution string     `json:"fx_resolution,omitempty"`
	// USDLeg is the USD price a closed-surface USD-anchored fiat cross
	// converted: USDLeg.Price × FXRate is Price.
	USDLeg *USDLeg `json:"usd_leg,omitempty"`

	// Change24hPct is the trailing-24h price change as a signed percentage with two fractional digits (e.g.
	// "+1.27", "-0.05", "0.00"). Null when no current USD price exists or there is no comparison bucket ~24h ago
	// (e.g. first traded < 24h ago). Clients should render "—" on null rather than fabricating "0%".
	Change24hPct *string `json:"change_24h_pct,omitempty"`
	// Confidence is the multi-factor confidence score per ADR-0019,
	// in [0, 1]. Populated only on /v1/price (the closed-bucket
	// surface) when the aggregator has cached a fresh score. Nil
	// means "unknown", NOT "low" — clients gating on confidence
	// must treat absence accordingly.
	Confidence *float64 `json:"confidence,omitempty"`
	// ConfidenceFactors is the per-factor decomposition that
	// accompanies Confidence; nil with the same semantics.
	ConfidenceFactors *ConfidenceFactors `json:"confidence_factors,omitempty"`
	// Substance is the measurement behind a Flags.ThinMarket price.
	Substance *SubstanceEvidence `json:"substance,omitempty"`
}

// SubstanceFloor is the substance policy a measurement was held to.
type SubstanceFloor struct {
	MinVolumeUSD   string `json:"min_volume_usd"`
	MinBuckets     int64  `json:"min_buckets"`
	MinSpanSeconds int64  `json:"min_span_seconds"`
}

// USDLeg is the USD price a derived fiat [PriceSnapshot] was converted from.
type USDLeg struct {
	Price      string    `json:"price"`
	ObservedAt time.Time `json:"observed_at"`
	Sources    []string  `json:"sources"`
}

// ConfidenceFactors is the per-factor decomposition of a [PriceSnapshot]'s Confidence score (ADR-0019).
type ConfidenceFactors struct {
	ZScore          float64 `json:"z_score"`
	SourceCount     float64 `json:"source_count"`
	Diversity       float64 `json:"diversity"`
	Liquidity       float64 `json:"liquidity"`
	CrossOracle     float64 `json:"cross_oracle"`
	BaselineQuality float64 `json:"baseline_quality"`
	// CrossOracleChecked is true only when real cross-oracle
	// reference data fed the cross_oracle factor. False means
	// "could not verify" — NOT "references agree".
	CrossOracleChecked bool `json:"cross_oracle_checked"`
	// CrossOracleAgreement counts the independent external
	// references that corroborated the price within the divergence
	// threshold (ADR-0019 Phase 3). Always 0 when unchecked.
	CrossOracleAgreement int `json:"cross_oracle_agreement"`
	// LiquidityMeasured is true only when a real USD volume fed the
	// Liquidity factor. False means the neutral no-information value
	// was used — NOT "liquidity is mid-range".
	LiquidityMeasured bool `json:"liquidity_measured"`
	// TriangulationAgreement scores agreement between the direct
	// price and the configured triangulation composite (1.0 agrees;
	// 0.7 neutral). TriangulationChecked false means no composite
	// was available — NOT "the composite agrees"; the factor carries
	// zero weight in the score when unchecked.
	TriangulationAgreement float64 `json:"triangulation_agreement"`
	TriangulationChecked   bool    `json:"triangulation_checked"`
	// BaselineAgeDays is the 30d baseline density in days-equivalent of
	// 1-minute buckets (at most 30; negative = no usable baseline), not
	// calendar age. BootstrapCapped true means the 0.5 bootstrap ceiling
	// bounded Confidence — a 0.5 may be the cap, not the evidence.
	BaselineAgeDays float64 `json:"baseline_age_days"`
	BootstrapCapped bool    `json:"bootstrap_capped"`
}

// PriceChangeHorizon is one trailing-window delta on [Client.PriceChanges]; every pointer field is nil and Available false when no closed bucket reaches that far back (a per-horizon miss, not an error).
type PriceChangeHorizon struct {
	// ChangePct is the signed percentage move of the current price vs
	// ReferencePrice, 2dp with an explicit "+" on gains. Nil when
	// unavailable.
	ChangePct *string `json:"change_pct"`
	// ReferencePrice is the closed VWAP at-or-before now−horizon (decimal
	// string). Nil when unavailable.
	ReferencePrice *string `json:"reference_price"`
	// ReferenceAt is the close time of the reference bucket (RFC 3339),
	// never the exact horizon instant. Nil when unavailable.
	ReferenceAt *time.Time `json:"reference_at"`
	// Resolution is the CAGG that served the reference bucket
	// ("1m" | "15m" | "1h" | "4h" | "1d"). Nil when unavailable.
	Resolution *string `json:"resolution"`
	// Available is false when the horizon has no reference price (all
	// pointer fields nil): no data that far back, or a withheld reference.
	Available bool `json:"available"`
	// Withheld is true when the reference bucket exists but a serving
	// gate refused to publish it; always false when Available is true.
	Withheld bool `json:"withheld"`
	// ThinMarket: the reference price comes from a market below the substance floor (include_thin opt-in).
	ThinMarket bool `json:"thin_market,omitempty"`
}

// PriceChanges is the data shape of [Client.PriceChanges]: the current closed price plus signed 1h/24h/7d/30d change.
type PriceChanges struct {
	AssetID          string    `json:"asset_id"`
	Quote            string    `json:"quote"`
	CurrentPrice     string    `json:"current_price"`
	CurrentPriceType string    `json:"current_price_type"`
	ObservedAt       time.Time `json:"observed_at"`
	// Resolution is the CAGG that served the current-price bucket.
	Resolution string `json:"resolution"`

	H1  PriceChangeHorizon `json:"1h"`
	H24 PriceChangeHorizon `json:"24h"`
	D7  PriceChangeHorizon `json:"7d"`
	D30 PriceChangeHorizon `json:"30d"`

	// Substance is the measurement behind a thin current price.
	Substance *SubstanceEvidence `json:"substance,omitempty"`
}

// HistorySeries is the data shape of [Client.HistorySinceInception]; Discontinuous reports that Points skips at least one whole bucket, with GapStartsAt + GapEndsAt bounding the widest gap, as in [ChartSeries].
type HistorySeries struct {
	AssetID string `json:"asset_id"`
	Quote   string `json:"quote"`
	// PriceType names the aggregation each point carries — "vwap"
	// today; TWAP planned.
	PriceType string `json:"price_type"`
	// Granularity is the bucket width this series is on. On this
	// endpoint it always equals the width requested — there is no
	// window to fit a narrower one against, so an over-fine request is
	// truncated at 50,000 buckets rather than coarsened.
	Granularity   string         `json:"granularity"`
	Points        []HistoryPoint `json:"points"`
	Discontinuous bool           `json:"discontinuous"`
	GapStartsAt   *time.Time     `json:"gap_starts_at,omitempty"`
	GapEndsAt     *time.Time     `json:"gap_ends_at,omitempty"`
	// RowCapTruncated is true when the series hit the row cap before
	// `to`; DataEndsAt is then the last returned bucket.
	RowCapTruncated bool       `json:"row_cap_truncated"`
	DataEndsAt      *time.Time `json:"data_ends_at,omitempty"`
}

// HistoryPoint is one row of a [HistorySeries].
type HistoryPoint struct {
	T         time.Time `json:"t"`
	P         string    `json:"p"`
	VolumeUSD *string   `json:"v_usd,omitempty"`
	// Sources is set only when a non-VWAP venue (a derived series)
	// contributed to the point.
	Sources []string `json:"sources,omitempty"`
}

// AssetDetail is the data shape of [Client.Assets] (listing) and, as one branch of [AssetLookup], of [Client.Asset].
type AssetDetail struct {
	// Kind vs Type: Kind is the WIRE SHAPE discriminator ("stellar_asset" on this struct) that [AssetLookup]
	// switches on for the /v1/assets/{asset_id} dual response (ADR-0042). Type is the STELLAR ASSET CLASS within
	// that shape (native / classic / soroban / fiat / global / external). Kind never varies here; Type does.
	Kind string `json:"kind"`

	// Identity
	AssetID    string  `json:"asset_id"`
	Type       string  `json:"type"` // "native" / "classic" / "soroban" / "fiat" / "global" / "external"
	Code       string  `json:"code,omitempty"`
	Issuer     string  `json:"issuer,omitempty"`
	ContractID string  `json:"contract_id,omitempty"`
	HomeDomain *string `json:"home_domain,omitempty"`
	// Decimals is the smallest-unit-to-display divisor power: classic + native = 7, a SEP-41 contract's
	// own `decimals()`. Nil when the server could not read the token's scale; amounts are then base units.
	Decimals *int `json:"decimals,omitempty"`
	// DisplayDecimals is the issuer's SEP-1 `display_decimals` rounding hint (USDC declares 2): how many
	// fractional digits to show. It is NOT a unit scale and must never enter amount math: divide by
	// 10^[AssetDetail.Decimals], then round for display to this. Deriving the unit scale from this issuer-controlled
	// hint would inflate market_cap_usd. Nil when the issuer's SEP-1 overlay declares none.
	DisplayDecimals *int `json:"display_decimals,omitempty"`

	// Sep1Status is one of "not_applicable" / "not_fetched" /
	// "verified" / "no_match" / "unreachable" — see the API design
	// reference for the full state machine.
	Sep1Status string `json:"sep1_status"`

	// ─── SEP-1 overlay (populated when Sep1Status == "verified") ──

	// Name is the human-readable currency name (e.g. "USD Coin")
	// from the issuer's stellar.toml [[CURRENCIES]] entry.
	Name *string `json:"name,omitempty"`
	// Description is the currency's `desc` field.
	Description *string `json:"description,omitempty"`
	// Image is an absolute URL to the asset logo.
	Image *string `json:"image,omitempty"`
	// OrgName is the issuer organisation's name (DOCUMENTATION.ORG_NAME).
	OrgName *string `json:"org_name,omitempty"`
	// AnchorAsset is the off-chain asset this token anchors to
	// (e.g. "USD"). Empty for non-anchored tokens.
	AnchorAsset *string `json:"anchor_asset,omitempty"`
	// AnchorAssetType classifies the anchor (fiat, crypto, stock, …).
	AnchorAssetType *string `json:"anchor_asset_type,omitempty"`

	// ─── F2 fields (Freighter V2 / ADR-0011 supply derivation) ────

	// CirculatingSupply / TotalSupply / MaxSupply are decimal
	// strings in the asset's smallest integer unit (stroops for
	// XLM / classic; contract-defined for SEP-41). Consumers divide
	// by 10^Decimals for display. Null when no supply snapshot
	// exists for this asset.
	CirculatingSupply *string `json:"circulating_supply,omitempty"`
	TotalSupply       *string `json:"total_supply,omitempty"`
	MaxSupply         *string `json:"max_supply,omitempty"`

	// PriceUSD is the current per-asset USD price as a fixed-precision decimal string, the same
	// value `/v1/price?asset=…&quote=fiat:USD` returns, inlined to save a round-trip. Null when no
	// USD price can be derived (no on-chain trades, no prices_1m row, or stablecoin-fiat proxy not
	// enabled).
	PriceUSD *string `json:"price_usd,omitempty"`

	// PriceBasis identifies a PriceUSD that is not a direct market observation. "global_market": the vetted
	// global ticker's cross-venue price (see GlobalMarket). "declared_peg": an operator-declared 1:1 fiat peg times
	// the current fiat-to-USD rate, used because no market-derived price survived the server's substance gate.
	// "transitive": derived through one intermediate hop (asset, hop, USD proxy). Empty means market-derived.
	// Peg-priced rows carry no change pills or market-history-derived valuation fields.
	PriceBasis string `json:"price_basis,omitempty"`

	// Price provenance on a listing row whose price is a published rate
	// rather than a Stellar market observation: the same tiers as
	// GlobalAssetView.PriceAuthority, the contributing sources, the
	// observation time, and the vendor credit when exactly one licensed
	// off-chain feed produced it.
	PriceAuthority string     `json:"price_authority,omitempty"`
	PriceSources   []string   `json:"price_sources,omitempty"`
	PriceAsOf      *time.Time `json:"price_as_of,omitempty"`
	PriceCredit    string     `json:"price_credit,omitempty"`

	// PriceWithheldReason is set only when PriceUSD is nil because the price was withheld rather than never
	// observed: "substance", "scam_issuer", "upstream_leg" or "unattributed" (the `price-withheld` 404's vocabulary),
	// or "decimals_mismatch" (asset payloads only: a read token scale contradicts the one the price was normalised on).
	PriceWithheldReason string `json:"price_withheld_reason,omitempty"`

	// GlobalMarket is the global-market reference for a classic asset the
	// verified catalogue binds to a global ticker on its exact (code,
	// issuer), with the Stellar price's divergence from it.
	GlobalMarket *AssetGlobalMarket `json:"global_market,omitempty"`

	// IssuerBehaviour is a classic asset's issuer flags and lifetime
	// mint/burn/clawback totals; asset detail only.
	IssuerBehaviour *AssetIssuerBehaviour `json:"issuer_behaviour,omitempty"`

	// ThinMarket: PriceUSD comes from a market below the substance floor
	// (include_thin opt-in); no valuation or series derives from it.
	ThinMarket bool `json:"thin_market,omitempty"`
	// Substance is the measurement behind a thin price (detail only).
	Substance *SubstanceEvidence `json:"substance,omitempty"`

	// MarketCapUSD = circulating × USD price / 10^Decimals,
	// formatted to two fractional digits. Null when supply or USD
	// price is unavailable.
	MarketCapUSD *string `json:"market_cap_usd,omitempty"`

	// FDVUSD = max_supply × USD price / 10^Decimals. Null when
	// max_supply is null (uncapped issuer + no override + no SEP-1
	// declaration), when USD price is unavailable, or when suppressed
	// (see MarketCapLowLiquidity and MarketCapDecimalsMismatch).
	FDVUSD *string `json:"fdv_usd,omitempty"`
	// FDVWithheldReason says why FDVUSD alone is null beside a served MarketCapUSD (e.g. "low_liquidity").
	FDVWithheldReason string `json:"fdv_withheld_reason,omitempty"`

	// MarketCapLowLiquidity is true when market_cap_usd or fdv_usd were deliberately suppressed (served null) by
	// either guard:
	//
	//   - the floor: the backing price came from negligible liquidity (a single venue AND low trailing-24h USD
	//     volume), or
	//   - the ceiling: the figure exceeds the server's maximum multiple of the asset's own trailing-24h USD
	//     volume. FDV can breach it alone, leaving market_cap_usd served.
	// Distinguishes "suppressed on purpose" from "no supply/price data"; price_usd still serves.
	MarketCapLowLiquidity bool `json:"market_cap_low_liquidity,omitempty"`
	// MarketCapLowerBound is true on a listing row whose market_cap_usd rests on a floor supply.
	MarketCapLowerBound         bool `json:"market_cap_lower_bound,omitempty"`
	CirculatingSupplyLowerBound bool `json:"circulating_supply_lower_bound,omitempty"` // circulating_supply is a floor, cap or no cap

	// MarketCapDecimalsMismatch is true when market_cap_usd (and fdv_usd) were refused because the server's two decimals
	// resolvers disagreed for this Soroban token, so supply and price sat on different scales. Self-clearing.
	MarketCapDecimalsMismatch bool `json:"market_cap_decimals_mismatch,omitempty"`
	// MarketCapWithheldReason says why market_cap_usd is null beside a served PriceUSD and CirculatingSupply.
	MarketCapWithheldReason string `json:"market_cap_withheld_reason,omitempty"`

	// ListingReference and ListingValuation carry an independent listing
	// platform's price for this exact address, and the supply valued at
	// it. Present only where this index declines to publish a market cap;
	// never a substitute for one. See [AssetListingValuation].
	ListingReference *AssetListingReference `json:"listing_reference,omitempty"`
	ListingValuation *AssetListingValuation `json:"listing_valuation,omitempty"`

	// SupplyBasis identifies which ADR-0011 policy produced the
	// total/circulating numbers (e.g. "issuer_exclusion",
	// "admin_exclusion", "override"); null when no snapshot exists. It
	// never carries "sep1_declared_max" — see MaxSupplyBasis.
	SupplyBasis *string `json:"supply_basis,omitempty"`

	// Trust is the banded trust score with its factor breakdown; set on
	// the detail lookup only, nil on listing rows.
	Trust *AssetTrust `json:"trust,omitempty"`

	// MaxSupplyBasis names where MaxSupply (and FDVUSD) came from when not the SupplyBasis policy, e.g. "sep1_declared_max".
	MaxSupplyBasis *string `json:"max_supply_basis,omitempty"`

	// SupplyAsOf / SupplyAsOfLedger date the supply observation; nil when
	// the reading carries no vintage.
	SupplyAsOf       *time.Time `json:"supply_as_of,omitempty"`
	SupplyAsOfLedger *uint32    `json:"supply_as_of_ledger,omitempty"`

	// VolumeUSD24h is the trailing-24h USD-denominated trade volume across every pair this asset participates in.
	// "0" is valid (tracked, no trades in the window); null means the volume reader isn't wired or the lookup failed.
	//
	// Off-chain CEX/FX trades populate it directly (uniform 10^8 scale). On-chain DEX trades populate it when the
	// quote asset is on the operator's `[trades].usd_pegged_classic_assets` list (trusted 1:1 peg), or, when the
	// list is non-empty, when a recent `<quote>/<USD-peg>` VWAP exists in prices_1m at the trade's timestamp.
	VolumeUSD24h *string `json:"volume_24h_usd,omitempty"`
	// VolumeLowerBound is true when VolumeUSD24h excludes unpriced trades.
	VolumeLowerBound bool `json:"volume_lower_bound,omitempty"`

	// Change24hPct is the trailing-24h price change as a signed
	// percentage with two fractional digits (e.g. "+1.27", "-0.05",
	// "0.00"). Null when no current USD price exists for the asset,
	// or when there is no comparison bucket ~24h ago (e.g. the asset
	// first traded < 24h ago). Clients
	// should render "—" on null rather than fabricating "0%".
	Change24hPct *string `json:"change_24h_pct,omitempty"`

	// SEP-1 standing and backing declarations; issuer-declared, nil unless Sep1Status == "verified".
	CurrencyStatus         *string `json:"currency_status,omitempty"`
	IsAssetAnchored        *bool   `json:"is_asset_anchored,omitempty"`
	AttestationOfReserve   *string `json:"attestation_of_reserve,omitempty"`
	RedemptionInstructions *string `json:"redemption_instructions,omitempty"`
	Regulated              *bool   `json:"regulated,omitempty"`
	ApprovalServer         *string `json:"approval_server,omitempty"`
	ApprovalCriteria       *string `json:"approval_criteria,omitempty"`

	// ─── SEP-1 issuance declarations ───
	//
	// The issuer's own commitments from their stellar.toml `[[CURRENCIES]]` entry; populated only when Sep1Status
	// == "verified". These say what the issuer pledged; the F2 fields above observe what is actually on-chain.

	// Conditions is the issuer's free-form terms / conditions text
	// (SEP-1 `conditions`).
	Conditions *string `json:"conditions,omitempty"`

	// FixedNumber is the SEP-1-declared fixed total supply, if the
	// issuer committed to one. Decimal string in the asset's
	// smallest integer unit. Distinct from `TotalSupply` which is
	// the live-ledger sum.
	FixedNumber *string `json:"fixed_number,omitempty"`

	// MaxNumber is the SEP-1-declared maximum supply ceiling, if the
	// issuer set a cap. Decimal string. Distinct from `MaxSupply`
	// which is operator/policy-derived.
	MaxNumber *string `json:"max_number,omitempty"`

	// IsUnlimited signals the issuer asserts unbounded issuance.
	// Null when the issuer didn't address supply at all (no
	// fixed_number / max_number / is_unlimited declaration); false
	// when they did and committed to a bounded supply.
	IsUnlimited *bool `json:"is_unlimited,omitempty"`

	// UnverifiedWarning points at the verified Stellar-canonical asset when the requested asset
	// uses a verified currency's ticker code but is not the verified issuer. Nil for the verified
	// asset itself and for any code not claimed by a verified currency. Pairs with
	// `Flags.UnverifiedTickerCollision` on the envelope.
	UnverifiedWarning *UnverifiedWarning `json:"unverified_warning,omitempty"`

	// UnverifiedTickerCollision is the listing-row trust signal on /v1/assets: true when this row's (code, issuer)
	// uses a verified currency's ticker but is not the verified issuer (a look-alike). The listing serves
	// COALESCE(slug, code) AS slug, so a NULL-slug impersonator emits the verified asset's code as its slug;
	// consumers must AND the verified-slug check with `!UnverifiedTickerCollision`. False (omitted) for the verified
	// asset and for codes no verified currency claims. The detail path carries UnverifiedWarning instead.
	UnverifiedTickerCollision bool `json:"unverified_ticker_collision,omitempty"`

	// FiatCodeAnchor points at the catalogue's fiat denomination when the requested asset's code names a sovereign
	// currency (USD, EUR, ...) with no Stellar issuance of its own. A fiat code is a denomination, never an
	// impersonation claim, so its presence never suppresses MarketCapUSD (unlike UnverifiedWarning). Nil for any
	// code the catalogue does not hold as a fiat denomination.
	FiatCodeAnchor *FiatCodeAnchor `json:"fiat_code_anchor,omitempty"`

	// ─── Asset-catalogue overlay listing fields (populated when
	// the server's AssetsReader is wired) ──
	Slug             string           `json:"slug,omitempty"`
	Class            string           `json:"class,omitempty"`
	Change1hPct      *string          `json:"change_1h_pct,omitempty"`
	Change7dPct      *string          `json:"change_7d_pct,omitempty"`
	FirstSeenLedger  *int64           `json:"first_seen_ledger,omitempty"`
	LastSeenLedger   *int64           `json:"last_seen_ledger,omitempty"`
	ObservationCount *int64           `json:"observation_count,omitempty"`
	MarketsCount     *int             `json:"markets_count,omitempty"`
	TradeCount24h    *int             `json:"trade_count_24h,omitempty"`
	PriceHistory24h  []PricePoint     `json:"price_history_24h,omitempty"`
	PriceHistory7d   []PricePoint     `json:"price_history_7d,omitempty"`
	ATH              *ATHPoint        `json:"ath,omitempty"`
	IssuerScamReason string           `json:"issuer_scam_reason,omitempty"`
	TopMarkets       []AssetTopMarket `json:"top_markets,omitempty"`

	// IssuerDirectoryTags / Domain / Name are the live stellar-expert account_directory attribution for the issuer
	// (e.g. {malicious,unsafe} tags on a wash-inflated scam asset). Display-only third-party attribution, never an
	// input to verification. Omitted when no directory reader is wired, the issuer isn't listed, or the lookup
	// failed.
	IssuerDirectoryTags   []string `json:"issuer_directory_tags,omitempty"`
	IssuerDirectoryDomain string   `json:"issuer_directory_domain,omitempty"`
	IssuerDirectoryName   string   `json:"issuer_directory_name,omitempty"`

	// VolumeCharacter classifies the asset's trailing-window trade volume by account structure: "market" (honest
	// multi-account, default), "operational" (issuer-side wrap corridor), or "concentrated" (>90% in one account
	// pair on a market-styled pair: wash/ping-pong/dust). ANALYTICS / DISPLAY only: it does not re-rank and never
	// feeds pricing or verification. Omitted when the reader isn't wired.
	VolumeCharacter string `json:"volume_character,omitempty"`

	// VolumeCharacterSignals are the account-structure signals
	// VolumeCharacter is derived from. Present whenever the reader is
	// wired (zeroed for an asset with no priced trades in the window).
	VolumeCharacterSignals *AssetVolumeCharacterSignals `json:"volume_character_signals,omitempty"`
}

// AssetVolumeCharacterSignals are the account-structure signals AssetDetail.VolumeCharacter is derived from; shares are fractions in [0,1].
type AssetVolumeCharacterSignals struct {
	WindowDays             int     `json:"window_days"`
	VolumeUSD              string  `json:"volume_usd"`
	DistinctMakers         int64   `json:"distinct_makers"`
	DistinctTakers         int64   `json:"distinct_takers"`
	TopAccountPairVolShare float64 `json:"top_account_pair_vol_share"`
	SelfCrossShare         float64 `json:"self_cross_share"`
	IssuerSideShare        float64 `json:"issuer_side_share"`
	MarketStyledShare      float64 `json:"market_styled_share"`
	IsMarketStyled         bool    `json:"is_market_styled"`
}

// UnverifiedWarning is attached to AssetDetail when an asset code-collides with a verified currency but the issuer does not match.
type UnverifiedWarning struct {
	// VerifiedSlug is the canonical slug the consumer can redirect to.
	VerifiedSlug string `json:"verified_slug"`
	// VerifiedAssetID is the verified canonical asset_id.
	VerifiedAssetID string `json:"verified_asset_id"`
	// VerifiedName is the human-readable currency name.
	VerifiedName string `json:"verified_name"`
	// VerifiedIssuer is the short attribution string
	// (e.g. "Circle (centre.io)"). Empty when the catalogue entry
	// didn't include a verified_issuer_label.
	VerifiedIssuer string `json:"verified_issuer,omitempty"`
	// Note is a one-sentence warning rendered verbatim by the client.
	Note string `json:"note"`
}

// FiatCodeAnchor is the neutral note attached to AssetDetail when the code names a catalogue fiat denomination with no Stellar issuance; never an impersonation verdict.
type FiatCodeAnchor struct {
	// Ticker is the ISO-4217 code the asset denominates in.
	Ticker string `json:"ticker"`
	// KnownAnchor is true when the issuer is in the catalogue's
	// operator-curated known-anchor set for this ticker.
	KnownAnchor bool `json:"known_anchor"`
	// Note is a one-sentence summary rendered verbatim by the client.
	Note string `json:"note"`
}

// TradeRow is one raw trade row of [Client.History]; amounts are decimal strings (ADR-0003), and Price (quote/base at 10 fractional digits, computed at response time) is nil when one leg is zero.
type TradeRow struct {
	Source      string    `json:"source"`
	Ledger      uint32    `json:"ledger"`
	TxHash      string    `json:"tx_hash"`
	OpIndex     uint32    `json:"op_index"`
	Timestamp   time.Time `json:"ts"`
	BaseAsset   string    `json:"base_asset"`
	QuoteAsset  string    `json:"quote_asset"`
	BaseAmount  string    `json:"base_amount"`
	QuoteAmount string    `json:"quote_amount"`
	Price       *string   `json:"price"`
	// BaseDecimals / QuoteDecimals are the smallest-unit scale for each side: divide BaseAmount by
	// 10^BaseDecimals (QuoteAmount by 10^QuoteDecimals) for whole-asset units.
	//
	// The scale is a property of the row's source, not of the asset, and a single page mixes both:
	//
	//   - on-chain rows (sdex, soroswap, aquarius, phoenix, comet) carry the asset's own scale: 7 for
	//     native/classic, the token contract's declared decimals() for Soroban tokens;
	//   - CEX rows (binance, kraken, bitstamp, coinbase) carry 8 regardless of the pair.
	//
	// Never substitute a constant: Price is quote/base and scale-invariant, so a wrong scale is undetectable. Zero means
	// absent: /v1/observations rows carry no scale, and a History row omits it when the token declares none (amounts are
	// then base units of unknown scale; do not divide). A token whose declared decimals() is 0 also reads as 0.
	BaseDecimals  int `json:"base_decimals,omitempty"`
	QuoteDecimals int `json:"quote_decimals,omitempty"`
	// RoutedVia is the router/aggregator whose same-transaction
	// invocation drove this trade (`routers.name`, e.g.
	// "soroswap-router" — see [Client.Aggregators]). Empty for
	// direct trades and for very recent routed trades the server's
	// attribution sweeper (1-minute cadence) hasn't tagged yet.
	RoutedVia string `json:"routed_via,omitempty"`
}

// AggregatorRow is one routers-registry entry of [Client.Aggregators] with its trailing-24h routed-via rollup; RoutedVolume24hUSD is a decimal string (ADR-0003), nil when no routed trade carried a USD valuation, and vault-kind entries always report zero routed trades.
type AggregatorRow struct {
	ContractID     string `json:"contract_id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"` // "router" | "aggregator-vault"
	Protocol       string `json:"protocol"`
	AutoDiscovered bool   `json:"auto_discovered"`

	RoutedTrades24h    int64      `json:"routed_trades_24h"`
	RoutedVolume24hUSD *string    `json:"routed_volume_24h_usd"`
	VolumeLowerBound   bool       `json:"volume_lower_bound,omitempty"` // RoutedVolume24hUSD excludes unpriced trades
	LastRoutedAt       *time.Time `json:"last_routed_at"`

	// Notes are coverage caveats for this row's stats; nil for
	// kind == "aggregator-vault" rows, which need none.
	Notes []string `json:"notes,omitempty"`
}

// OHLCBar is a single OHLC bar of [Client.OHLC]; prices are decimal strings (ADR-0003) and volumes smallest-unit strings at a per-SOURCE scale (7 decimals on-chain, 8 CEX, 6 FX), so render as BaseVolume / 10^BaseVolumeDecimals; when Truncated the NEWEST trades are kept, so Close is exact but Open/High/Low may be incomplete.
type OHLCBar struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	Open        string    `json:"open"`
	High        string    `json:"high"`
	Low         string    `json:"low"`
	Close       string    `json:"close"`
	BaseVolume  string    `json:"base_volume"`
	QuoteVolume string    `json:"quote_volume"`
	// BaseVolumeDecimals / QuoteVolumeDecimals are the decimal exponent of the two sums above. They differ
	// when one leg is a token with nonstandard decimals. Nil when a contributing trade's source is
	// unrecognised, or when such a token's leg includes an off-chain venue: the sum is then not convertible
	// to asset units.
	BaseVolumeDecimals  *int `json:"base_volume_decimals"`
	QuoteVolumeDecimals *int `json:"quote_volume_decimals"`
	TradeCount          int  `json:"trade_count"`
	Truncated           bool `json:"truncated"`
	// Clamped is true when the requested `to` fell in the still-filling
	// bucket (or the future) and was pulled back to the last closed one.
	Clamped    bool               `json:"clamped"`
	BelowFloor bool               `json:"below_floor"`         // no trade in the window cleared the $0.01 notional floor: prices come from dust
	Substance  *SubstanceEvidence `json:"substance,omitempty"` // the measurement behind a thin-market bar
}

// OHLCSeriesResponse is the data shape of [Client.OHLCSeries]: up to `limit` closed bars of width Interval over [From, To).
type OHLCSeriesResponse struct {
	Base      string             `json:"base"`
	Quote     string             `json:"quote"`
	Interval  string             `json:"interval"`
	From      time.Time          `json:"from"`
	To        time.Time          `json:"to"`
	Intervals []OHLCSeriesBar    `json:"intervals"`
	Substance *SubstanceEvidence `json:"substance,omitempty"` // the measurement behind a thin-market series
}

// OHLCSeriesBar is one bar of an [OHLCSeriesResponse]; prices are decimal strings, volumes integer smallest-unit strings, and VBaseDecimals / VQuoteDecimals are the volume scales (nil when a contributing source is unrecognised).
type OHLCSeriesBar struct {
	T              time.Time `json:"t"`
	O              string    `json:"o"`
	H              string    `json:"h"`
	L              string    `json:"l"`
	C              string    `json:"c"`
	VBase          string    `json:"v_base"`
	VQuote         string    `json:"v_quote"`
	VBaseDecimals  *int      `json:"v_base_decimals"`
	VQuoteDecimals *int      `json:"v_quote_decimals"`
	N              int64     `json:"n"`
	Truncated      bool      `json:"truncated,omitempty"`
	BelowFloor     bool      `json:"below_floor,omitempty"` // no trade in the bar cleared the $0.01 notional floor
	// Sources are the venues behind this bar; a derived or vendor series
	// is not a fill-derived VWAP.
	Sources []string `json:"sources,omitempty"`
}

// Source is one row of the source registry from [Client.Sources]; only class `exchange` contributes to VWAP, and BackfillSafe is false for on-chain Soroban sources until a per-WASM-hash audit flips it (CEX/FX are always true).
type Source struct {
	Name              string `json:"name"`
	Class             string `json:"class"`
	Subclass          string `json:"subclass,omitempty"`
	IncludeInVWAP     bool   `json:"include_in_vwap"`
	Paid              bool   `json:"paid"`
	Credit            string `json:"credit,omitempty"`
	BackfillAvailable bool   `json:"backfill_available"`
	BackfillSafe      bool   `json:"backfill_safe"`
	DefaultWeight     int    `json:"default_weight"`
	// OnChain is true when the source observes the Stellar network
	// directly (dispatcher-path ingest) rather than an off-chain
	// vendor API. False for CEX / FX / aggregators.
	OnChain bool `json:"on_chain"`
	// Selectable is true when `source=` accepts this name: on-chain
	// sources and CEX venues. False for data vendors, which 400.
	Selectable bool `json:"selectable"`
	// Stats columns — populated only with `?include=stats`; zero values otherwise.
	TradeCount24h    int64  `json:"trade_count_24h,omitempty"`
	VolumeUSD24h     string `json:"volume_24h_usd,omitempty"`
	MarketsCount24h  int64  `json:"markets_count_24h,omitempty"`
	VolumeLowerBound bool   `json:"volume_lower_bound,omitempty"` // USD figures exclude unpriced trades
	// VolumeHistory24h / VolumeHistory7d — per-hour USD-volume
	// buckets (24 / 168 entries, oldest → newest, zero-filled).
	// Populated only when the request includes `sparkline` /
	// `sparkline7d` respectively.
	VolumeHistory24h []VolumeBucket `json:"volume_history_24h,omitempty"`
	VolumeHistory7d  []VolumeBucket `json:"volume_history_7d,omitempty"`
}

// VolumeBucket is one hourly USD-volume datapoint in a source or market sparkline; VolumeUSD is a decimal string per ADR-0003.
type VolumeBucket struct {
	Hour      time.Time `json:"hour"`
	VolumeUSD string    `json:"volume_usd"`
	// TradeCount is the number of trades in the hour; it powers the
	// trade-count line above the $-volume bars.
	TradeCount int64 `json:"trade_count"`
}

// MarketVolumeBucket is one hourly USD-volume datapoint on a market sparkline; unlike [VolumeBucket] it carries no trade count.
type MarketVolumeBucket struct {
	Hour      time.Time `json:"hour"`
	VolumeUSD string    `json:"volume_usd"`
}

// MethodologySource is one venue on [Methodology]: the static registry subset of [Source], without live stats.
type MethodologySource struct {
	Name              string `json:"name"`
	Class             string `json:"class"`
	Subclass          string `json:"subclass,omitempty"`
	DefaultWeight     int    `json:"default_weight"`
	IncludeInVWAP     bool   `json:"include_in_vwap"`
	Paid              bool   `json:"paid"`
	Credit            string `json:"credit,omitempty"`
	BackfillAvailable bool   `json:"backfill_available"`
	BackfillSafe      bool   `json:"backfill_safe"`
	OnChain           bool   `json:"on_chain"`
}

// Methodology is the data shape of [Client.Methodology]: a machine-readable summary of the active aggregation policy for transparency consumers.
type Methodology struct {
	Version       string                   `json:"version"`
	Aggregation   MethodologyAggregation   `json:"aggregation"`
	SourceClasses []MethodologySourceClass `json:"source_classes"`
	Sources       []MethodologySource      `json:"sources"`
	References    []MethodologyReference   `json:"references"`
}

// MethodologyAggregation captures the price-derivation policy.
type MethodologyAggregation struct {
	PriceMethod               string                     `json:"price_method"`
	OutlierFilter             MethodologyOutlierFilter   `json:"outlier_filter"`
	StablecoinFiatProxy       []MethodologyStablecoinPeg `json:"stablecoin_fiat_proxy"`
	ClosedBucketWindowSeconds int                        `json:"closed_bucket_window_seconds"`
}

// MethodologyOutlierFilter is one σ-trim rule.
type MethodologyOutlierFilter struct {
	Endpoint     string  `json:"endpoint,omitempty"`
	DefaultSigma float64 `json:"default_sigma"`
	Note         string  `json:"note,omitempty"`
}

// MethodologyStablecoinPeg is one (token → fiat) mapping.
type MethodologyStablecoinPeg struct {
	AssetID string `json:"asset_id"`
	PegsTo  string `json:"pegs_to"`
}

// MethodologySourceClass describes one registry class; the served list is the complete glossary for every [MethodologySource] Class, and only exchange carries ContributesToVWAP.
type MethodologySourceClass struct {
	Name              string `json:"name"`
	ContributesToVWAP bool   `json:"contributes_to_vwap"`
	Description       string `json:"description"`
}

// MethodologyReference points at an ADR or other narrative doc.
type MethodologyReference struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Market is one (base, quote) pair from [Client.Markets] / [Client.Pair] with a 24h activity summary; use LastTradeAt (minute precision, falls back to BucketCloseAt for pairs idle >24h) for staleness, NOT BucketCloseAt, which is start-of-day UTC.
type Market struct {
	Base          string    `json:"base"`
	Quote         string    `json:"quote"`
	LastTradeAt   time.Time `json:"last_trade_at"`
	BucketCloseAt time.Time `json:"bucket_close_at"`
	TradeCount24h int64     `json:"trade_count_24h"`
	// Volume24hUSD is the trailing-24h USD volume summed from
	// prices_1m's per-bucket volume_usd. Decimal string per
	// ADR-0003. Nil when the pair has no USD-equivalent trades.
	Volume24hUSD *string `json:"volume_24h_usd,omitempty"`
	// VolumeLowerBound is true when Volume24hUSD excludes unpriced trades.
	VolumeLowerBound bool `json:"volume_lower_bound,omitempty"`
	// LastPrice is the most recent quote-per-base price observed
	// for this pair (cross-source) within the trailing 24h. Nil
	// when no recent bucket carries one.
	LastPrice *string `json:"last_price,omitempty"`
	// VolumeHistory24h — per-hour USD-volume sparkline buckets.
	// Populated only when the request sets `?include=sparkline`.
	VolumeHistory24h []MarketVolumeBucket `json:"volume_history_24h,omitempty"`
	// FirstTradeAt is the pair's first recorded daily bucket
	// ("since inception = first recorded trade"). Present only when
	// the request passed include=inception.
	FirstTradeAt *time.Time `json:"first_trade_at,omitempty"`
}

// AssetSupplyFlows is the data payload of [Client.AssetSupplyFlows]; HistoryIncomplete is true when the running net dips below zero, so Days must not be cumulated into a supply level.
type AssetSupplyFlows struct {
	AssetID           string               `json:"asset_id"`
	ContractID        string               `json:"contract_id"`
	Days              []AssetSupplyFlowDay `json:"days"`
	HistoryIncomplete bool                 `json:"history_incomplete"`
	AsOfLedger        *int64               `json:"as_of_ledger,omitempty"`
}

// AssetSupplyFlowDay is one UTC day of supply flows; Mint/Burn/Clawback are base-unit decimal strings and Net = mint - burn - clawback (signed).
type AssetSupplyFlowDay struct {
	Day      string `json:"day"`
	Mint     string `json:"mint"`
	Burn     string `json:"burn"`
	Clawback string `json:"clawback"`
	Net      string `json:"net"`
	Flows    int64  `json:"flows"`
}

// AssetMetadata is the data shape of [Client.AssetMetadata] (the SEP-1 overlay); overlay fields populate only when Sep1Status == "verified" and are nil otherwise.
type AssetMetadata struct {
	AssetID    string  `json:"asset_id"`
	HomeDomain *string `json:"home_domain,omitempty"`
	Sep1Status string  `json:"sep1_status"`

	// SEP-1 [[CURRENCIES]] overlay — populated only on Sep1Status=="verified".
	Name            *string `json:"name,omitempty"`
	Description     *string `json:"description,omitempty"`
	Image           *string `json:"image,omitempty"`
	OrgName         *string `json:"org_name,omitempty"`
	AnchorAsset     *string `json:"anchor_asset,omitempty"`
	AnchorAssetType *string `json:"anchor_asset_type,omitempty"`

	// SEP-1 standing and backing declarations; issuer-declared, nil unless Sep1Status == "verified".
	CurrencyStatus         *string `json:"currency_status,omitempty"`
	IsAssetAnchored        *bool   `json:"is_asset_anchored,omitempty"`
	AttestationOfReserve   *string `json:"attestation_of_reserve,omitempty"`
	RedemptionInstructions *string `json:"redemption_instructions,omitempty"`
	Regulated              *bool   `json:"regulated,omitempty"`
	ApprovalServer         *string `json:"approval_server,omitempty"`
	ApprovalCriteria       *string `json:"approval_criteria,omitempty"`

	// SEP-1 issuance declarations — issuer-declared, distinct from
	// the F2 fields on AssetDetail which observe live ledger state.
	Conditions  *string `json:"conditions,omitempty"`
	FixedNumber *string `json:"fixed_number,omitempty"`
	MaxNumber   *string `json:"max_number,omitempty"`
	IsUnlimited *bool   `json:"is_unlimited,omitempty"`
}

// Account is the data shape returned by [Client.Me].
type Account struct {
	KeyID string `json:"key_id"`
	// ParentKeyID is the key that minted this one; empty for a key issued by registration or the dashboard.
	ParentKeyID string `json:"parent_key_id,omitempty"`
	Label       string `json:"label,omitempty"`
	// KeyPrefix is the first characters of the plaintext key
	// (`sip_<8hex>`) — safe to log/display; customers use it to
	// identify which key a row refers to.
	KeyPrefix       string    `json:"key_prefix,omitempty"`
	Tier            string    `json:"tier"`
	RateLimitPerMin int       `json:"rate_limit_per_min,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	// User / AccountInfo are the magic-link session caller's info —
	// present on cookie-session responses only (dashboard flows);
	// nil on API-key responses.
	User        *AccountUser `json:"user,omitempty"`
	AccountInfo *AccountOrg  `json:"account,omitempty"`
}

// UsageRow is one (date, endpoint family) aggregate from [Client.Usage]; Endpoint is the route pattern (empty on the legacy per-day shape), Billable (ok + 4xx) is what the monthly quota counts, and Throttled (429) is never counted against it.
type UsageRow struct {
	Date      string `json:"date"`
	Endpoint  string `json:"endpoint,omitempty"`
	Requests  int    `json:"requests"`
	Billable  int    `json:"billable"`
	Errors    int    `json:"errors"`
	Throttled int    `json:"throttled"`
}

// KeyCreated is the data shape of [Client.CreateKey]; Plaintext is the only place the secret appears, returned once and never again.
type KeyCreated struct {
	KeyID     string `json:"key_id"`
	Plaintext string `json:"plaintext"`
	Label     string `json:"label"`
	// KeyPrefix is the first 12 chars of the plaintext
	// (`sip_<8hex>`) — safe to display; identifies the key later.
	KeyPrefix string `json:"key_prefix,omitempty"`
	// Scopes are the capability scopes the key was minted with (read / account / dashboard / admin); absent/empty = full access.
	Scopes []string `json:"scopes,omitempty"`
	// ExpiresAt is when the key stops authenticating; nil = never. A key minted by an expiring credential inherits its expiry.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// IssuerListEntry is one issuer-directory row from [Client.Issuers], ranked by total observation count; HomeDomain is empty until SEP-1 resolves `stellar.toml`.
type IssuerListEntry struct {
	GStrkey    string `json:"g_strkey"`
	HomeDomain string `json:"home_domain,omitempty"`
	// OrgName is the issuer's organisation name from SEP-1
	// (`[DOCUMENTATION].ORG_NAME`). Empty until the SEP-1
	// fetcher (`stellarindex-ops sep1-refresh`) resolves it.
	OrgName               string `json:"org_name,omitempty"`
	AssetCount            int64  `json:"asset_count"`
	TotalObservationCount int64  `json:"total_observation_count"`
	// OrgVerified — bidirectional SEP-1 proof.
	OrgVerified bool `json:"org_verified"`
	// ScamReason is non-empty when the issuer is in the curated
	// scam directory.
	ScamReason string `json:"scam_reason,omitempty"`
}

// IssuedAsset is one entry in [Issuer.Assets]: a classic asset minted by the parent issuer.
type IssuedAsset struct {
	AssetID          string `json:"asset_id"`
	Code             string `json:"code"`
	Slug             string `json:"slug"`
	FirstSeenLedger  uint32 `json:"first_seen_ledger"`
	LastSeenLedger   uint32 `json:"last_seen_ledger"`
	ObservationCount int64  `json:"observation_count"`
}

// Issuer is the data shape of [Client.Issuer]; auth flags and SEP-1 fields stay nil until the SEP-1 fetcher resolves them.
type Issuer struct {
	GStrkey    string `json:"g_strkey"`
	HomeDomain string `json:"home_domain,omitempty"`
	// OrgName is the issuer's organisation name from
	// `[DOCUMENTATION].ORG_NAME` in stellar.toml. Populated by
	// the SEP-1 fetcher; empty until resolved.
	OrgName string `json:"org_name,omitempty"`
	// OrgVerified is true only when the SEP-1 verification is
	// BIDIRECTIONAL: the issuer's home_domain serves a stellar.toml
	// that lists this issuer back. One-way resolution is spoofable
	// and does NOT set this flag.
	OrgVerified bool `json:"org_verified"`
	// ScamReason is non-empty when the issuer is in the curated
	// scam directory — render as a warning.
	ScamReason    string `json:"scam_reason,omitempty"`
	AuthRequired  *bool  `json:"auth_required,omitempty"`
	AuthRevocable *bool  `json:"auth_revocable,omitempty"`
	AuthImmutable *bool  `json:"auth_immutable,omitempty"`
	AuthClawback  *bool  `json:"auth_clawback,omitempty"`
	// AuthFlagsSource says WHERE the four flags above came from: "live" for a current ledger entry, "last_known"
	// for an account that has since been removed. A last_known row is a historical fact, not the issuer's present
	// configuration, and carries no home_domain (a dead account must not keep advertising one).
	AuthFlagsSource   string  `json:"auth_flags_source,omitempty"`
	AuthFlagsAsOfLedg *uint32 `json:"auth_flags_as_of_ledger,omitempty"`
	SEP1ResolvedAt    *string `json:"sep1_resolved_at,omitempty"`
	// SEP1Payload is opaque JSON — schemas drift over time and many
	// issuers add custom fields. Callers who need typed access
	// should unmarshal into their own struct.
	SEP1Payload    json.RawMessage `json:"sep1_payload,omitempty"`
	CreationLedger *uint32         `json:"creation_ledger,omitempty"`
	Assets         []IssuedAsset   `json:"assets,omitempty"`
	// CoverageNote is non-empty when the asset-list read failed
	// (including a timeout) and Assets was omitted rather than
	// published as a fabricated empty list. Absent means Assets is
	// complete, or the issuer genuinely has never issued an asset.
	CoverageNote string `json:"coverage_note,omitempty"`
}

// ChartSeries is the data shape of [Client.Chart], with points shaped like [HistoryPoint]; Truncated reports the timeframe predates the earliest data (DataStartsAt + RequestedFrom set), and Discontinuous reports a mid-series hole bounded by GapStartsAt + GapEndsAt.
type ChartSeries struct {
	AssetID string `json:"asset_id"`
	Quote   string `json:"quote"`
	// Granularity is the bucket width the series is ON, which may differ from the request: price_type=twap snaps
	// onto the 1h / 1d TWAP aggregates, and a (Timeframe, Granularity) pair whose grid exceeds the server's
	// 50,000-bucket response cap is served at the finest width that fits (1y + 1m comes back as 15m). Label an axis
	// from this field, not from the request.
	Granularity   string         `json:"granularity"`
	Timeframe     string         `json:"timeframe"`
	PriceType     string         `json:"price_type"`
	Points        []HistoryPoint `json:"points"`
	Truncated     bool           `json:"truncated"`
	DataStartsAt  *time.Time     `json:"data_starts_at,omitempty"`
	RequestedFrom *time.Time     `json:"requested_from,omitempty"`
	Discontinuous bool           `json:"discontinuous"`
	GapStartsAt   *time.Time     `json:"gap_starts_at,omitempty"`
	GapEndsAt     *time.Time     `json:"gap_ends_at,omitempty"`
	// Credit names the licensed off-chain feed when every point of a fiat series comes from it; empty once an on-chain venue feeds it.
	Credit string `json:"credit,omitempty"`
	// StartsBeforeWindow: the first point is the last closed bucket and ends at or before RequestedFrom (1h on a daily series).
	StartsBeforeWindow bool `json:"starts_before_window,omitempty"`
	// MarketCapLowLiquidity (price_type=market_cap only) reports that
	// Points was withheld because the asset's current market cannot
	// support a valuation, as on Asset.MarketCapLowLiquidity.
	MarketCapLowLiquidity bool `json:"market_cap_low_liquidity,omitempty"`
	// RowCapTruncated: a read (Timeframe "all" or a capped bounded window) hit the 50,000-bucket cap; Points ends at DataEndsAt.
	RowCapTruncated bool       `json:"row_cap_truncated,omitempty"`
	DataEndsAt      *time.Time `json:"data_ends_at,omitempty"`
}

// ChangeSummary is the data shape of [Client.ChangeSummary], a per-entity multi-window delta rollup; *_value fields are money and JSON STRINGS, while *_delta_pct fields are percentages and stay numbers.
type ChangeSummary struct {
	EntityType   string `json:"entity_type"`
	EntityID     string `json:"entity_id"`
	RefreshedAt  string `json:"refreshed_at"`
	CurrentValue string `json:"current_value"`

	H1Value     *string  `json:"h1_value,omitempty"`
	H1DeltaPct  *float64 `json:"h1_delta_pct,omitempty"`
	H24Value    *string  `json:"h24_value,omitempty"`
	H24DeltaPct *float64 `json:"h24_delta_pct,omitempty"`
	D7Value     *string  `json:"d7_value,omitempty"`
	D7DeltaPct  *float64 `json:"d7_delta_pct,omitempty"`
	D30Value    *string  `json:"d30_value,omitempty"`
	D30DeltaPct *float64 `json:"d30_delta_pct,omitempty"`

	ATHValue *string `json:"ath_value,omitempty"`
	ATHAt    string  `json:"ath_at,omitempty"`
	ATLValue *string `json:"atl_value,omitempty"`
	ATLAt    string  `json:"atl_at,omitempty"`

	StreakDirection string `json:"streak_direction,omitempty"`
	StreakDays      *int   `json:"streak_days,omitempty"`
	Acceleration    string `json:"acceleration,omitempty"`
}

// NetworkStats is the data shape of [Client.NetworkStats], the home-page aggregate snapshot; Volume24hUSD is a *string per ADR-0003, nil when the 24h window has no USD-equivalent trades.
type NetworkStats struct {
	Volume24hUSD     *string `json:"volume_24h_usd,omitempty"`
	VolumeLowerBound bool    `json:"volume_lower_bound,omitempty"` // Volume24hUSD excludes unpriced trades
	MarketsCount24h  int64   `json:"markets_count_24h"`
	AssetsIndexed    int64   `json:"assets_indexed"`
	LatestLedger     int64   `json:"latest_ledger"`
	ExchangeSources  int     `json:"exchange_sources"`
	TotalSources     int     `json:"total_sources"`
}

// Currency / CurrenciesList / CurrencyHistoryPoint / CurrencyDetail
// removed — `/v1/currencies` HTTP surface retired (no production
// consumers). Use AssetDetail from `/v1/assets` instead.

// AccountUser is the magic-link session caller's user info on [Account].User.
type AccountUser struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	DisplayName     string     `json:"display_name,omitempty"`
	Role            string     `json:"role,omitempty"`
	IsStaff         bool       `json:"is_staff"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
	LastLoginAt     *time.Time `json:"last_login_at,omitempty"`
}

// AccountOrg is the session caller's parent account on [Account].AccountInfo.
type AccountOrg struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Slug   string `json:"slug,omitempty"`
	Tier   string `json:"tier,omitempty"`
	Status string `json:"status,omitempty"`
	// RateLimitPerMin / MonthlyRequestQuota are what auth enforces on a
	// key minted without explicit limits, account overrides included.
	RateLimitPerMin     int   `json:"rate_limit_per_min,omitempty"`
	MonthlyRequestQuota int64 `json:"monthly_request_quota,omitempty"`
}
