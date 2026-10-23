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
	// Withheld names the requested ids [Client.PriceBatch] omitted from
	// Data because the server declined to publish their price (thin
	// market, flagged issuer, withheld upstream leg). An id in neither
	// Data nor Withheld has no price data. Only the batch surface sets it.
	Withheld []string `json:"withheld,omitempty"`
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
	// ProxyDeviation: a triangulated fiat:USD price was served through a USD peg while a declared peg trades off $1.
	ProxyDeviation bool `json:"proxy_deviation,omitempty"`
	// UnverifiedTickerCollision fires on `/v1/assets/{id}` when the requested asset's code matches a verified
	// currency's Stellar ticker but its issuer does not match the verified entry, i.e. someone issued their own
	// "USDC". The decoder ignores unknown fields, so older SDK builds are unaffected.
	UnverifiedTickerCollision bool `json:"unverified_ticker_collision,omitempty"`
	// DetectorsPartial fires on `/v1/mev` when the detection worker has
	// not completed a full run within its heartbeat TTL, so a quiet feed is
	// a detection gap rather than a quiet network.
	DetectorsPartial bool `json:"detectors_partial,omitempty"`
	// FiltersIgnored names the row-narrowing query parameters the response did NOT apply, spelled as the caller
	// sent them ("type", "code", "issuer", "q"). Empty, and omitted, when everything supplied was applied.
	// `/v1/assets` sets it on listings whose rows come from a source that cannot narrow (the class-scoped listings
	// and the lean asset-catalog fallback). A page that dropped a filter looks identical to one that matched, so a
	// consumer re-filtering the page keys on this.
	FiltersIgnored []string `json:"filters_ignored,omitempty"`
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
	// Decimals is the asset's smallest-unit-to-display divisor power
	// (always present on the wire). Stellar classic + native = 7;
	// SEP-41 contracts publish their own value via `decimals()`.
	Decimals int `json:"decimals"`
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

	// PriceWithheldReason is set only when PriceUSD is nil because the
	// price was withheld rather than never observed, in the reason
	// vocabulary of the `price-withheld` 404: "substance",
	// "scam_issuer", "upstream_leg" or "unattributed".
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

	// MarketCapLowLiquidity is true when market_cap_usd or fdv_usd were deliberately suppressed (served null) by
	// either guard:
	//
	//   - the floor: the backing price came from negligible liquidity (a single venue AND low trailing-24h USD
	//     volume), or
	//   - the ceiling: the figure exceeds the server's maximum multiple of the asset's own trailing-24h USD
	//     volume. FDV can breach it alone, leaving market_cap_usd served.
	//
	// Distinguishes "suppressed on purpose" from "no supply/price data"; price_usd still serves. Omitted (false)
	// when neither figure was suppressed.
	MarketCapLowLiquidity bool `json:"market_cap_low_liquidity,omitempty"`

	// MarketCapDecimalsMismatch is true when market_cap_usd (and fdv_usd) were refused (served
	// null) because the server's two decimals resolvers disagreed for this Soroban token, so supply
	// and price sat on different scales. Not a liquidity verdict; PriceUSD, CirculatingSupply and
	// Decimals still serve. Self-clearing once the server reconciles. Omitted (false) on
	// agreement.
	MarketCapDecimalsMismatch bool `json:"market_cap_decimals_mismatch,omitempty"`

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
	// Never substitute a constant: Price is quote/base and scale-invariant, so a wrong assumed scale is undetectable
	// from the payload. Populated by [Client.History]; omitted (zero) on /v1/observations rows, which carry no
	// per-side scale.
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
	// BaseVolumeDecimals / QuoteVolumeDecimals are the decimal exponent of the two sums above. Equal today (a
	// source stamps both legs at one scale) but carried separately because a scale belongs to an amount, not a
	// pair. Nil when a contributing trade's source is unrecognised: the sums are then not convertible to asset
	// units.
	BaseVolumeDecimals  *int `json:"base_volume_decimals"`
	QuoteVolumeDecimals *int `json:"quote_volume_decimals"`
	TradeCount          int  `json:"trade_count"`
	Truncated           bool `json:"truncated"`
	// Clamped is true when the requested `to` fell in the still-filling
	// bucket (or the future) and was pulled back to the last closed one.
	Clamped bool `json:"clamped"`
}

// OHLCSeriesResponse is the data shape of [Client.OHLCSeries]: up to `limit` closed bars of width Interval over [From, To).
type OHLCSeriesResponse struct {
	Base      string          `json:"base"`
	Quote     string          `json:"quote"`
	Interval  string          `json:"interval"`
	From      time.Time       `json:"from"`
	To        time.Time       `json:"to"`
	Intervals []OHLCSeriesBar `json:"intervals"`
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
	Label string `json:"label,omitempty"`
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
	// Scopes are the capability scopes the key was minted with
	// (read / account / dashboard / admin). Absent/empty = full
	// access — the posture every key minted before scopes shipped
	// keeps.
	Scopes []string `json:"scopes,omitempty"`
}

// Coin + CoinsPage types removed — `/v1/coins` HTTP surface
// retired (no production consumers). Use the AssetDetail surface
// from `/v1/assets` instead.

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

// Cursor is one per-source ingest progress marker from [Client.Cursors]; LagSeconds is computed server-side, and State is "live", "stale" or "abandoned" (abandoned rows are omitted unless requested).
type Cursor struct {
	Source      string `json:"source"`
	SubSource   string `json:"sub_source,omitempty"`
	LastLedger  uint32 `json:"last_ledger"`
	LastUpdated string `json:"last_updated"`
	LagSeconds  int64  `json:"lag_seconds"`
	State       string `json:"state"`
}

// Status is the data shape of [Client.Status], the customer-facing system-health rollup of /v1/status.
type Status struct {
	Overall   string          `json:"overall"`
	Region    StatusRegion    `json:"region"`
	Services  []StatusService `json:"services"`
	Latency   StatusLatency   `json:"latency"`
	Freshness StatusFreshness `json:"freshness"`
	Incidents StatusIncidents `json:"incidents"`
	// IncidentsStatus is the explicit tri-state trust signal for the Incidents block: "ok", "degraded", or
	// "unknown". Read it BEFORE the counts: on "unknown" the Alertmanager query failed (or no metrics backend is
	// wired) and the counts are the zero value of a failure, NOT an all-clear.
	IncidentsStatus string `json:"incidents_status"`
	// FreshnessStatus is the same trust signal for the Freshness block:
	// "ok" (every enabled source active), "degraded" (active < total) or
	// "unknown" (a count query failed; the counts are nil).
	FreshnessStatus string `json:"freshness_status"`
}

// StatusRegion identifies which region produced the response.
type StatusRegion struct {
	Name       string `json:"name"`
	Deployment string `json:"deployment"`
}

// StatusService is a per-binary heartbeat in [Status.Services]: "ok" if the last scrape was within 60 s, "down" when stale, "unknown" with no Prometheus backend.
type StatusService struct {
	Name     string    `json:"name"`
	Status   string    `json:"status"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// StatusLatency reports histogram-derived API percentiles over the last [WindowSecs] seconds; zero means no backend or no samples.
type StatusLatency struct {
	P50Ms      float64 `json:"p50_ms"`
	P95Ms      float64 `json:"p95_ms"`
	P99Ms      float64 `json:"p99_ms"`
	WindowSecs int     `json:"window_secs"`
	// P95TargetMs / P99TargetMs are the server's own SLO thresholds,
	// echoed so a consumer judges latency against the SAME numbers the
	// status rollup does. Hardcoding them client-side risks
	// two red SLO bars under a green "All systems operational" banner.
	P95TargetMs float64 `json:"p95_target_ms"`
	P99TargetMs float64 `json:"p99_target_ms"`
}

// StatusFreshness summarises the ingest layer; a nil count was not measured (its query failed), while a served 0 is a non-nil 0.
type StatusFreshness struct {
	LastAggregatorTick time.Time `json:"last_aggregator_tick,omitempty"`
	ActiveSources      *int      `json:"active_sources,omitempty"`
	TotalSources       *int      `json:"total_sources,omitempty"`
}

// StatusIncidents counts currently-firing alerts by severity; zero means no Alertmanager backend or no alerts.
type StatusIncidents struct {
	ActiveCount        int              `json:"active_count"`
	PageCount          int              `json:"page_count"`
	TicketCount        int              `json:"ticket_count"`
	InformationalCount int              `json:"informational_count"`
	Active             []ActiveIncident `json:"active,omitempty"`
}

// ActiveIncident is the customer-facing summary of one currently-firing alert in [StatusIncidents.Active]; internal labels are deliberately excluded.
type ActiveIncident struct {
	Name       string `json:"name"`
	Severity   string `json:"severity"`
	RunbookURL string `json:"runbook_url,omitempty"`
}

// Health is the data shape of [Client.Healthz] and [Client.Readyz].
type Health struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
	// Checks is populated on /readyz with per-dependency ping
	// results; absent on /healthz.
	Checks []HealthCheck `json:"checks,omitempty"`
	// StatusRoot points at /v1/status — the SLA-truth rollup
	// covering ingest lag, freshness, and per-pair latency.
	StatusRoot string `json:"status_root,omitempty"`
}

// HealthCheck is one per-dependency result in [Health].Checks.
type HealthCheck struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Error is populated only when OK is false.
	Error string `json:"error,omitempty"`
}

// Version is the data shape of [Client.Version]: build metadata for the API binary.
type Version struct {
	Version   string `json:"version"`
	BuildDate string `json:"build_date"`
	Commit    string `json:"commit"`
	Dirty     string `json:"dirty"`
	GoVersion string `json:"go_version"`
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
	// Credit names the licensed off-chain feed when every point of a
	// fiat series comes from it; empty once an on-chain venue feeds it.
	Credit string `json:"credit,omitempty"`
	// MarketCapLowLiquidity (price_type=market_cap only) reports that
	// Points was withheld because the asset's current market cannot
	// support a valuation, as on Asset.MarketCapLowLiquidity.
	MarketCapLowLiquidity bool `json:"market_cap_low_liquidity,omitempty"`
	// RowCapTruncated: a Timeframe "all" read hit the 50,000-bucket cap, so
	// Points is the OLDEST slice and ends at DataEndsAt, short of the present.
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
	Volume24hUSD    *string `json:"volume_24h_usd,omitempty"`
	MarketsCount24h int64   `json:"markets_count_24h"`
	AssetsIndexed   int64   `json:"assets_indexed"`
	LatestLedger    int64   `json:"latest_ledger"`
	ExchangeSources int     `json:"exchange_sources"`
	TotalSources    int     `json:"total_sources"`
}

// Incident is one customer-facing incident post from [Client.Incidents]; Severity is SEV-1..SEV-4, Status is investigating/identified/monitoring/resolved, and BodyMarkdown is the full post.
type Incident struct {
	Slug               string     `json:"slug"`
	Title              string     `json:"title"`
	Severity           string     `json:"severity"`
	Status             string     `json:"status"`
	StartedAt          time.Time  `json:"started_at"`
	ResolvedAt         *time.Time `json:"resolved_at,omitempty"`
	AffectedComponents []string   `json:"affected_components,omitempty"`
	PostmortemRef      string     `json:"postmortem,omitempty"`
	BodyMarkdown       string     `json:"body_markdown"`
}

// IncidentsList wraps the [Client.Incidents] response, sorted started_at descending.
type IncidentsList struct {
	Incidents []Incident `json:"incidents"`
	Count     int        `json:"count"`
}

// Currency / CurrenciesList / CurrencyHistoryPoint / CurrencyDetail
// removed — `/v1/currencies` HTTP surface retired (no production
// consumers). Use AssetDetail from `/v1/assets` instead.

// VWAPResult is the data shape of [Client.VWAP]; Truncated means the window exceeded the server's 10000-trade cap, the OLDEST trades were dropped and Price is NOT the true window VWAP, and volumes are smallest-unit integers at a per-SOURCE scale (divide by 10^BaseVolumeDecimals).
type VWAPResult struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	Price       string    `json:"price"`
	BaseVolume  string    `json:"base_volume"`
	QuoteVolume string    `json:"quote_volume"`
	// nil when a contributing trade's source is unrecognised: the sums
	// are then not convertible to asset units.
	BaseVolumeDecimals  *int `json:"base_volume_decimals"`
	QuoteVolumeDecimals *int `json:"quote_volume_decimals"`
	TradeCount          int  `json:"trade_count"`
	OutliersFiltered    int  `json:"outliers_filtered"`
	Truncated           bool `json:"truncated"`
	// Clamped: see [OHLCBar.Clamped].
	Clamped bool `json:"clamped"`
	// Breakdown is set only for a `breakdown=source` request.
	Breakdown *VWAPBreakdown `json:"breakdown,omitempty"`
	// Substance is the measurement behind a Flags.ThinMarket result.
	Substance *SubstanceEvidence `json:"substance,omitempty"`
}

// VWAPBreakdown mirrors `internal/api/v1.VWAPBreakdown`.
type VWAPBreakdown struct {
	Interval  *string               `json:"interval"`
	Truncated bool                  `json:"truncated"`
	Buckets   []VWAPBreakdownBucket `json:"buckets"`
}

// VWAPBreakdownBucket is one time bucket of a [VWAPBreakdown].
type VWAPBreakdownBucket struct {
	Start       time.Time             `json:"start"`
	End         time.Time             `json:"end"`
	QuoteVolume string                `json:"quote_volume"`
	TradeCount  int                   `json:"trade_count"`
	Sources     []VWAPSourceBreakdown `json:"sources"`
}

// VWAPSourceBreakdown is one venue's share of a [VWAPBreakdownBucket].
type VWAPSourceBreakdown struct {
	Source           string  `json:"source"`
	Price            *string `json:"price"`
	BaseVolume       string  `json:"base_volume"`
	QuoteVolume      string  `json:"quote_volume"`
	TradeCount       int     `json:"trade_count"`
	Weight           string  `json:"weight"`
	OutliersExcluded int     `json:"outliers_excluded"`
}

// TWAPResult is the data shape of [Client.TWAP]; TradeCount counts trades that carried weight and OutliersFiltered those the default outlier filter removed.
type TWAPResult struct {
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	Price            string    `json:"price"`
	TradeCount       int       `json:"trade_count"`
	OutliersFiltered int       `json:"outliers_filtered"`
	Truncated        bool      `json:"truncated"`
	// Clamped: see [OHLCBar.Clamped].
	Clamped bool `json:"clamped"`
	// Substance is the measurement behind a Flags.ThinMarket result.
	Substance *SubstanceEvidence `json:"substance,omitempty"`
}

// Pool is one (source, base, quote) row from [Client.Pools], unlike [Market] which collapses across sources; LastPrice is per-source.
type Pool struct {
	Source        string    `json:"source"`
	Base          string    `json:"base"`
	Quote         string    `json:"quote"`
	LastTradeAt   time.Time `json:"last_trade_at"`
	TradeCount24h int64     `json:"trade_count_24h"`
	Volume24hUSD  *string   `json:"volume_24h_usd,omitempty"`
	// VolumeLowerBound is true when Volume24hUSD excludes unpriced trades.
	VolumeLowerBound bool    `json:"volume_lower_bound,omitempty"`
	LastPrice        *string `json:"last_price,omitempty"`
}

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

	// Headline price block — all nil/empty together when no tier of
	// the fallback chain produced a price (typically a Stellar-only
	// token whose price isn't aggregated at the global level).
	PriceUSD       *string    `json:"price_usd,omitempty"`
	PriceAuthority string     `json:"price_authority,omitempty"` // "vwap_native" | "aggregator_avg" | "triangulated" | "reference_rate" | "identity" | "onchain_listing"
	PriceSources   []string   `json:"price_sources,omitempty"`
	PriceAsOf      *time.Time `json:"price_as_of,omitempty"`
	// PriceCredit names the vendor when PriceSources is exactly one
	// licensed off-chain feed; a composite or on-chain price carries none.
	PriceCredit string `json:"price_credit,omitempty"`

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

// ─── RWA value over time (/v1/rwa/history) ──────────────────────────

// RWAHistoryView is the /v1/rwa/history payload: the RWA set valued daily on the reference basis; Assets sizes the WHOLE set, Members is the subset in the series, and each point's AssetsUnvalued is counted against Assets.
type RWAHistoryView struct {
	Basis       string `json:"basis"`
	Granularity string `json:"granularity"`
	Timeframe   string `json:"timeframe"`
	Quote       string `json:"quote"`
	Assets      int    `json:"assets"`
	Issuers     int    `json:"issuers"`
	Members     int    `json:"members"`
	// MembershipAsOf is when the set was decided. Every point is valued
	// against THAT membership, including points predating an asset's
	// admission — the series is today's set applied backwards.
	MembershipAsOf time.Time            `json:"membership_as_of"`
	Excluded       []RWAHistoryExcluded `json:"excluded,omitempty"`
	Sources        []string             `json:"sources,omitempty"`
	// Points is the total series, ascending by day. A day no member
	// could be valued on is ABSENT.
	Points []RWAHistoryPoint `json:"points"`
	Groups []RWAHistoryGroup `json:"groups,omitempty"`
	// Truncated reports that a cap bound the series.
	Truncated bool `json:"truncated,omitempty"`
}

// RWAHistoryPoint is one day of an RWA value series.
type RWAHistoryPoint struct {
	T time.Time `json:"t"`
	// ValueUSD is a 2-dp decimal STRING (ADR-0003), never a float.
	ValueUSD       string `json:"value_usd"`
	AssetsValued   int    `json:"assets_valued"`
	AssetsUnvalued int    `json:"assets_unvalued"`
	// LowerBound is true whenever any set member is unvalued on this
	// day, i.e. whenever the figure is less than the reference-priced
	// value of the set on that day.
	LowerBound bool `json:"lower_bound"`
}

// RWAHistoryGroup is one decomposition of the total series; Feed and Source are set only on an asset group, as an issuer group may span several.
type RWAHistoryGroup struct {
	Key     string            `json:"key"`
	Label   string            `json:"label,omitempty"`
	Code    string            `json:"code,omitempty"`
	Issuer  string            `json:"issuer,omitempty"`
	Feed    string            `json:"feed,omitempty"`
	Source  string            `json:"source,omitempty"`
	Members int               `json:"members"`
	Points  []RWAHistoryPoint `json:"points"`
}

// RWAHistoryExcluded is one reason a set member is absent from a series, with how many members it accounts for; shared by /v1/rwa/history and /v1/rwa/premium.
type RWAHistoryExcluded struct {
	Reason string `json:"reason"`
	Assets int    `json:"assets"`
	Detail string `json:"detail"`
}

// ─── RWA premium to NAV over time (/v1/rwa/premium) ─────────────────

// RWAPremiumHistoryView is the /v1/rwa/premium payload: neither leg is ever carried forward, so a point exists only on a day both price and oracle value were observed, and there is no total series (read Coverage).
type RWAPremiumHistoryView struct {
	Basis       string `json:"basis"`
	Granularity string `json:"granularity"`
	Timeframe   string `json:"timeframe"`
	Quote       string `json:"quote"`
	Assets      int    `json:"assets"`
	Issuers     int    `json:"issuers"`
	// Bound is how many set members carry a curated (code, issuer) →
	// instrument-feed binding: the ceiling on how many series can exist.
	Bound int `json:"bound"`
	// Members is how many set members could be compared at all.
	// Members plus every Excluded[].Assets is exactly Assets, so the
	// account closes. NOT windowed — a narrower Timeframe can leave
	// Series shorter than this.
	Members        int                       `json:"members"`
	MembershipAsOf time.Time                 `json:"membership_as_of"`
	Excluded       []RWAHistoryExcluded      `json:"excluded,omitempty"`
	Sources        []string                  `json:"sources,omitempty"`
	Series         []RWAPremiumSeries        `json:"series"`
	Coverage       []RWAPremiumCoveragePoint `json:"coverage,omitempty"`
	Truncated      bool                      `json:"truncated,omitempty"`
}

// RWAPremiumSeries is one member's premium-to-NAV history.
type RWAPremiumSeries struct {
	AssetID string                   `json:"asset_id"`
	Code    string                   `json:"code,omitempty"`
	Issuer  string                   `json:"issuer,omitempty"`
	Label   string                   `json:"label,omitempty"`
	Feed    string                   `json:"feed"`
	Source  string                   `json:"source"`
	Points  []RWAPremiumHistoryPoint `json:"points"`
	// MarketWithheldDays counts days with observed trades that did NOT
	// clear the thin-market floor. "We saw trades and refused to price
	// them" and "there were no trades" are opposite findings that an
	// absent point renders identically.
	MarketWithheldDays int `json:"market_withheld_days,omitempty"`
	// ReferenceOnlyDays counts days with a published value and no
	// market at all — the ordinary case for a tokenized instrument
	// that is bought and held. Counted only from the member's FIRST
	// observed market day onward, and never on a day already in
	// MarketWithheldDays.
	ReferenceOnlyDays int `json:"reference_only_days,omitempty"`
}

// RWAPremiumHistoryPoint is one day of one member's premium; both prices travel beside the ratio so a reader can tell which side moved.
type RWAPremiumHistoryPoint struct {
	T time.Time `json:"t"`
	// PremiumPct is (market − reference) / reference × 100 as a SIGNED
	// 4-dp decimal STRING (ADR-0003), never a float.
	PremiumPct   string `json:"premium_pct"`
	MarketUSD    string `json:"market_usd"`
	ReferenceUSD string `json:"reference_usd"`
	// VolumeUSD and Trades are the market leg's evidence — the figures
	// the day's thin-market floor was applied to.
	VolumeUSD string `json:"volume_usd"`
	Trades    int64  `json:"trades"`
}

// RWAPremiumCoveragePoint is one day's account of how much of the set could be compared; AssetsMeasured + AssetsUnmeasured is Assets.
type RWAPremiumCoveragePoint struct {
	T                time.Time `json:"t"`
	AssetsMeasured   int       `json:"assets_measured"`
	AssetsUnmeasured int       `json:"assets_unmeasured"`
}

// ─── Tokenized real-world assets (/v1/rwa/assets) ───────────────────

// RWAAssetsView is the /v1/rwa/assets payload; membership is decided from identity and attestation only, and every money field is a decimal string (ADR-0003) that is ABSENT, not zero, when withheld (read Valuation.Status first).
type RWAAssetsView struct {
	Definition RWADefinition `json:"definition"`
	Summary    RWASummary    `json:"summary"`
	Assets     []RWAAsset    `json:"assets"`
	// Membership dates the SET — when it was built, and whether the
	// copy served is past its own lifetime. Nil only before the first
	// build has ever completed. Not the envelope's AsOf, which is the
	// RESPONSE's instant and moves between requests.
	Membership *RWAMembershipSet `json:"membership,omitempty"`
	ByClass    []RWAGroupTotal   `json:"by_class"`
	// ByIssuer is keyed on the issuer G-address; contract-issued rows,
	// which have none, are not in it.
	ByIssuer []RWAIssuerTotal `json:"by_issuer"`
	// Refused is the ordered requirement tally over the candidates
	// that reached the full evaluation — not the whole population. Read
	// Funnel for that.
	Refused []RWARefusal `json:"refused"`
	// Funnel accounts for the entire population the served set was
	// narrowed from.
	Funnel RWAFunnel `json:"funnel"`
	// UnreachedEntities names curated-directory entities recognised as issuing or custodying value for which this
	// index holds NO Stellar token at all. No token of theirs was ever collected, so none was ever evaluated and none
	// was refused by a requirement. Served so a consumer can tell a recognised entity the index cannot see from one
	// that does not exist.
	UnreachedEntities []RWAUnreachedEntity `json:"unreached_entities"`
	// CuratedAssets is a named third party's list of tokenized real-world assets, on that party's word and at that
	// party's own price. It is served APART from Assets and never merged into Summary or the group totals: Basis on
	// each row is "third_party_curated", and a row also present in the verified set is flagged on its Curator. Empty
	// whenever Curated is nil or its Status is not "served".
	CuratedAssets []RWAAsset `json:"curated_assets,omitempty"`
	// Curated describes the curated arm — which curator, whether it was
	// served, and the three figures a reader compares: the verified
	// total, the value the curator adds beyond it, and their sum. Nil on
	// a deployment with no curator wired.
	Curated *RWACuratedSummary `json:"curated,omitempty"`
}

// RWACurator names the third party whose word placed a curated row on the list, and whether the same token is also in the verified set.
type RWACurator struct {
	Curator      string `json:"curator"`
	Company      string `json:"company,omitempty"`
	Subclass     string `json:"subclass,omitempty"`
	AlsoVerified bool   `json:"also_verified"`
}

// RWACuratedCensus counts the curator's cached list: recognised, priced and stale-priced entries plus its last stamp; Entries = Contracts + Classic and only contract rows are served.
type RWACuratedCensus struct {
	Entries       int        `json:"entries"`
	Contracts     int        `json:"contracts"`
	Classic       int        `json:"classic"`
	Priced        int        `json:"priced"`
	PricedClassic int        `json:"priced_classic"`
	Stale         int        `json:"stale"`
	ObservedAt    *time.Time `json:"observed_at,omitempty"`
}

// RWACuratedPublishedSplit is one line of the curator's own subclass split for the published month, in the curator's vocabulary.
type RWACuratedPublishedSplit struct {
	Subclass string `json:"subclass"`
	ValueUSD string `json:"value_usd"`
}

// RWACuratedPublishedPoint is one month of the curator's published total series; MonthEnd is YYYY-MM-DD.
type RWACuratedPublishedPoint struct {
	MonthEnd string `json:"month_end"`
	ValueUSD string `json:"value_usd"`
}

// RWACuratedPublished is the curator's own unverified monthly total and split; Stale is true past 48h, the server omits the block past 7 days, and GapVsVerifiedUSD is nil when no verified reference total exists.
type RWACuratedPublished struct {
	TotalUSD             string                     `json:"total_usd"`
	AsOf                 string                     `json:"as_of"`
	ExecutedAt           time.Time                  `json:"executed_at"`
	BySubclass           []RWACuratedPublishedSplit `json:"by_subclass"`
	BySubclassExecutedAt *time.Time                 `json:"by_subclass_executed_at,omitempty"`
	Series               []RWACuratedPublishedPoint `json:"series"`
	Stale                bool                       `json:"stale,omitempty"`
	Source               string                     `json:"source"`
	GapVsVerifiedUSD     *string                    `json:"gap_vs_verified_usd,omitempty"`
}

// RWACuratedSummary is the curated arm's headline; Status is "served", "published_totals", "unavailable" or "unwired", and AdditionalValueUSD counts only rows NOT in the verified set so CombinedValueUSD never double counts.
type RWACuratedSummary struct {
	Curator            string               `json:"curator"`
	CuratorCredit      string               `json:"curator_credit,omitempty"`
	Status             string               `json:"status"`
	Assets             int                  `json:"assets"`
	AlsoVerified       int                  `json:"also_verified"`
	AssetsValued       int                  `json:"assets_valued"`
	AdditionalValueUSD *string              `json:"additional_value_usd,omitempty"`
	CombinedValueUSD   *string              `json:"combined_value_usd,omitempty"`
	VerifiedValueUSD   *string              `json:"verified_value_usd,omitempty"`
	Census             RWACuratedCensus     `json:"census"`
	Published          *RWACuratedPublished `json:"published,omitempty"`
	Basis              string               `json:"basis"`
}

// RWAUnreachedEntity is one recognised issuing entity the index holds no token for; the labels are the curated third party's own.
type RWAUnreachedEntity struct {
	Address string   `json:"address"`
	Name    string   `json:"name,omitempty"`
	Domain  string   `json:"domain,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// RWAFunnel is the full narrowing from issuer accounts to the served set; each stage's Dropped counts reconcile exactly to the next stage of the same Unit, and Balanced says whether that held.
type RWAFunnel struct {
	Stages   []RWAFunnelStage `json:"stages"`
	Balanced bool             `json:"balanced"`
	// Imbalance names what did not close, and is empty exactly when
	// Balanced is true. Semicolon-separated when several checks failed.
	Imbalance string `json:"imbalance,omitempty"`
	Basis     string `json:"basis"`
	// ListingDirectory is the evidence behind the "listing" arm's
	// verdict. Nil when nothing was observed: no listing reader wired,
	// or a read that did not answer.
	ListingDirectory *RWAListingDirectory `json:"listing_directory,omitempty"`
}

// RWAMembershipSet dates the served set: BuiltAt is when the rebuild FINISHED (not the envelope's AsOf), Stale alone is transient, and Stale with RebuildFailedAt set means the last rebuild FAILED.
type RWAMembershipSet struct {
	BuiltAt         time.Time  `json:"built_at"`
	Stale           bool       `json:"stale"`
	RebuildFailedAt *time.Time `json:"rebuild_failed_at,omitempty"`
}

// RWAListingDirectory is what the listing directory held when the set was built; Entries counts FRESH rows only, so Entries == 0 with Stale > 0 means the sync stopped, and ObservedAt is when this index read it.
type RWAListingDirectory struct {
	ObservedAt time.Time `json:"observed_at"`
	Entries    int       `json:"entries"`
	Contracts  int       `json:"contracts"`
	Stale      int       `json:"stale"`
}

// RWAFunnelStage is one population on the way to the served set; counts compare only when Units match, and Arm ("classic" or "contract") reconciles within an arm, never across.
type RWAFunnelStage struct {
	Arm     string          `json:"arm"`
	Stage   string          `json:"stage"`
	Unit    string          `json:"unit"`
	Count   int             `json:"count"`
	Dropped []RWAFunnelDrop `json:"dropped,omitempty"`
}

// RWAFunnelDrop is one counted reason a population shrank; Actor is "operator", "issuer", or "definition" for the membership rule working as intended.
type RWAFunnelDrop struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
	Actor  string `json:"actor"`
}

// RWADefinition is the membership rule applied: Requirements for a classic (code, issuer) asset, ContractRequirements for a contract-issued token, never both.
type RWADefinition struct {
	Requirements         []string `json:"requirements"`
	ContractRequirements []string `json:"contract_requirements"`
	AnchorClasses        []string `json:"anchor_classes"`
	RecognitionTags      []string `json:"recognition_tags"`
	// ContractRecognitionTags is the same vocabulary for a CONTRACT
	// address, and is narrower: the upstream directory tags AMM pools
	// and routers `defi` and `exchange`, which on a contract address
	// describe infrastructure that issues nothing.
	ContractRecognitionTags []string `json:"contract_recognition_tags"`
	// ContractAnchorClasses is the class vocabulary a contract-issued
	// token's curated binding may declare.
	ContractAnchorClasses []string `json:"contract_anchor_classes,omitempty"`
	// ContractRecognitionSources is the closed vocabulary of routes by
	// which a contract address counts as independently recognised.
	ContractRecognitionSources []string `json:"contract_recognition_sources"`
	ScamFlagTags               []string `json:"scam_flag_tags"`
	// BoundInstruments is the curated (code, issuer) → oracle-feed
	// binding set; a pair absent from it gets no reference.
	BoundInstruments []RWABoundInstrument `json:"bound_instruments"`
	// BoundContractInstruments is the curated contract-address →
	// instrument binding set. An address here is NOT thereby a member.
	BoundContractInstruments []RWABoundContractInstrument `json:"bound_contract_instruments"`
	DocumentationURL         string                       `json:"documentation_url"`
}

// RWABoundInstrument binds one exact classic (code, issuer) pair to the oracle feed its reference is read from.
type RWABoundInstrument struct {
	Code   string `json:"code"`
	Issuer string `json:"issuer"`
	Feed   string `json:"feed"`
}

// RWABoundContractInstrument binds one exact contract address to the real-world instrument it holds and that instrument's class.
type RWABoundContractInstrument struct {
	ContractID string `json:"contract_id"`
	Instrument string `json:"instrument"`
	Class      string `json:"class"`
}

// RWASummary aggregates the served set; MarketCapUSD is nil when no asset publishes one, never "0.00".
type RWASummary struct {
	Assets                  int     `json:"assets"`
	Issuers                 int     `json:"issuers"`
	MarketCapUSD            *string `json:"market_cap_usd,omitempty"`
	AssetsValued            int     `json:"assets_valued"`
	AssetsUnvalued          int     `json:"assets_unvalued"`
	LowerBound              bool    `json:"lower_bound"`
	EarliestFirstSeenLedger uint32  `json:"earliest_first_seen_ledger,omitempty"`
	// AssetsWithReference counts members carrying an independent oracle
	// valuation of their instrument; AssetsCompared counts the subset
	// where a market price could also be measured against it.
	AssetsWithReference int `json:"assets_with_reference"`
	AssetsCompared      int `json:"assets_compared"`
	// ReferenceValuation is the SECOND total: the set valued at
	// reference prices rather than at observed market prices. Never a
	// component of MarketCapUSD and never added to it.
	ReferenceValuation RWAReferenceSummary `json:"reference_valuation"`
	// BothBases totals only the members carrying BOTH figures. The two
	// totals above are sums over different rows, so subtracting one from
	// the other measures nothing; these two are taken over the same rows
	// and their difference is the aggregate gap.
	BothBases RWABothBases `json:"both_bases"`
	Basis     string       `json:"basis"`
	Truncated bool         `json:"truncated,omitempty"`
}

// RWAReferenceSummary aggregates the reference-priced basis (supply times an oracle value), NOT a market cap; ValueUSD is nil when no member is reference-valued, never "0.00".
type RWAReferenceSummary struct {
	ValueUSD       *string `json:"value_usd,omitempty"`
	AssetsValued   int     `json:"assets_valued"`
	AssetsUnvalued int     `json:"assets_unvalued"`
	LowerBound     bool    `json:"lower_bound"`
	// Sources names the distinct oracles behind the total. Each
	// contributing row carries its full provenance in RWAAsset.Reference.
	Sources []string `json:"sources,omitempty"`
	// Provenances names the distinct RWAReference.Provenance values in
	// the total — the total can mix kinds of claim.
	Provenances []string `json:"provenances,omitempty"`
	Basis       string   `json:"basis"`
}

// RWABothBases totals the members carrying a market cap AND a reference valuation, the only subset on which the two bases compare.
type RWABothBases struct {
	Assets            int     `json:"assets"`
	MarketCapUSD      *string `json:"market_cap_usd,omitempty"`
	ReferenceValueUSD *string `json:"reference_value_usd,omitempty"`
}

// RWAValuation carries a row valuation or the reason there is none; money fields are populated only when Status is "published".
type RWAValuation struct {
	Status   string  `json:"status"`
	PriceUSD *string `json:"price_usd,omitempty"`
	// PriceBasis names a price that is NOT a direct market observation
	// ("declared_peg" or "transitive"). Absent means market-derived.
	PriceBasis   string  `json:"price_basis,omitempty"`
	MarketCapUSD *string `json:"market_cap_usd,omitempty"`
}

// RWAReferenceValuation is supply valued at the oracle REFERENCE price, never folded into MarketCapUSD; ValueUSD is set if and only if Status is "published", beside an RWAAsset.Reference naming its provenance.
type RWAReferenceValuation struct {
	Status   string  `json:"status"`
	ValueUSD *string `json:"value_usd,omitempty"`
}

// RWAReference is an independent oracle's valuation of the instrument a token anchors to, NOT this platform's price for the token; it is the provenance behind RWAReferenceValuation.
type RWAReference struct {
	// PriceUSD is the oracle's published value, verbatim at the feed's
	// own decimal scale.
	PriceUSD string `json:"price_usd"`
	Source   string `json:"source"`
	// Credit names the vendor when Source is a licensed off-chain feed.
	Credit string `json:"credit,omitempty"`
	// Feed is the key PriceUSD is published under; its meaning follows
	// Provenance. Only under "oracle_instrument_nav" is it the canonical
	// `rwa:<CODE>` id, ready for /oracle/latest — under any other
	// provenance it is that source's own identifier (e.g. a listing
	// platform's asset id) and is NOT an ADR-0028 feed id.
	Feed string `json:"feed"`
	// Quote is the denominator, always "fiat:USD" on a served row: a
	// NAV feed denominated in a reserve asset is a ratio, and the
	// difference is invisible in the number alone.
	Quote string    `json:"quote"`
	AsOf  time.Time `json:"as_of"`
	// Stale marks a reference older than 72h — labelled, not withheld.
	Stale bool `json:"stale,omitempty"`
	// Provenance names what kind of figure PriceUSD is:
	// "oracle_instrument_nav", "fund_nav", "listing_platform_price",
	// "prospectus_constant_nav" or "curator_uploaded_price". Only the
	// first is a statement about the backing instrument.
	Provenance string `json:"provenance"`
	// DecimalsPublished is the publisher's stated precision; 2 on a
	// "fund_nav" reference.
	DecimalsPublished *int `json:"decimals_published,omitempty"`
	// NAVDisagreement marks an oracle reference that the fund's own
	// fresh NAV differs from by more than half a cent.
	NAVDisagreement bool `json:"nav_disagreement,omitempty"`
}

// RWAPremium is the token's market price against the oracle's instrument valuation, or the reason there is none; Pct is nil unless Status is "published", never zero-filled.
type RWAPremium struct {
	Status string  `json:"status"`
	Pct    *string `json:"pct,omitempty"`
}

// RWAISINCollision reports one ISIN declared in anchor_asset by several issuer accounts; a declared ISIN is a claim, not proof of holding it.
type RWAISINCollision struct {
	// ISIN is the declared identifier in canonical upper-case form.
	ISIN string `json:"isin"`
	// DeclaredByIssuers counts the distinct issuer accounts, this row's
	// included, declaring ISIN.
	DeclaredByIssuers int `json:"declared_by_issuers"`
}

// RWAVettedISIN is the hand-vetted ISIN binding that admitted an asset under
// the sep1_isin_vetted basis.
type RWAVettedISIN struct {
	ISIN       string `json:"isin"`
	Instrument string `json:"instrument"`
	Source     string `json:"source"`
	VerifiedOn string `json:"verified_on"`
	ReviewBy   string `json:"review_by"`
	// ReviewDue is true once the review date has passed; the row is labelled, not withheld.
	ReviewDue bool `json:"review_due,omitempty"`
}

// RWAAsset is one member of the set with the evidence that admitted it; identity is (Code, Issuer), as Code alone identifies nothing.
type RWAAsset struct {
	AssetID string `json:"asset_id"`
	// Code and Issuer are the classic identity. BOTH are empty on a
	// contract-issued row, which carries ContractID instead: exactly one
	// of (Code, Issuer) and ContractID is populated on every row.
	Code   string `json:"code"`
	Issuer string `json:"issuer"`
	// ContractID is the C-strkey of a contract-issued member, and the
	// whole of its identity.
	ContractID string `json:"contract_id,omitempty"`
	// Symbol is the token symbol the CONTRACT declares on chain.
	// Contract-authored display text, never identity — two contracts may
	// declare the same symbol and they are different assets.
	Symbol string `json:"symbol,omitempty"`
	Slug   string `json:"slug,omitempty"`
	Name   string `json:"name,omitempty"`
	// HomeDomain is the issuer account's on-chain domain the attestation
	// was fetched from; empty on a contract-issued row, whose curated
	// directory domain is IssuerDirectoryDomain instead.
	HomeDomain            string   `json:"home_domain,omitempty"`
	IssuerDirectoryName   string   `json:"issuer_directory_name,omitempty"`
	IssuerDirectoryTags   []string `json:"issuer_directory_tags,omitempty"`
	IssuerDirectoryDomain string   `json:"issuer_directory_domain,omitempty"`
	Basis                 string   `json:"basis"`
	// Recognition names the route by which the row's issuer (or
	// contract) counts as independently recognised; the routes are not
	// the same strength of evidence.
	Recognition string `json:"recognition"`
	AnchorClass string `json:"anchor_class,omitempty"`
	AnchorAsset string `json:"anchor_asset,omitempty"`
	// ISINCollision is set when AnchorAsset is an ISIN more than one
	// issuer account declares. Informational: it never changes the row.
	ISINCollision *RWAISINCollision `json:"isin_collision,omitempty"`
	// VettedISIN is set when the asset was admitted under sep1_isin_vetted: the
	// hand-vetted binding behind the ISIN. review_due flips once the review date passes.
	VettedISIN *RWAVettedISIN `json:"vetted_isin,omitempty"`
	// Valuation is the observed-market-price money, or the reason there
	// is none; ReferenceValuation is the same float at the reference
	// price. The two are separate bases and are never summed.
	Valuation          RWAValuation          `json:"valuation"`
	ReferenceValuation RWAReferenceValuation `json:"reference_valuation"`
	Reference          *RWAReference         `json:"reference,omitempty"`
	Premium            RWAPremium            `json:"premium"`
	CirculatingSupply  *string               `json:"circulating_supply,omitempty"`
	// SupplyBasis names the reading that produced CirculatingSupply
	// (the ADR-0011 vocabulary).
	SupplyBasis string `json:"supply_basis,omitempty"`
	// CirculatingSupplyLowerBound is true when CirculatingSupply is a
	// provable floor, not a complete reading — every reference valuation
	// on the row is then a lower bound too.
	CirculatingSupplyLowerBound bool `json:"circulating_supply_lower_bound,omitempty"`
	// Decimals is the on-chain smallest-unit scale — 7 for classic, and
	// whatever a SEP-41 contract declares for a contract-issued row — so
	// both valuations can be re-derived: CirculatingSupply /
	// 10^Decimals is the whole-token float each price multiplies. nil
	// when a contract's scale could not be read.
	Decimals         *int    `json:"decimals"`
	Volume24hUSD     *string `json:"volume_24h_usd,omitempty"`
	FirstSeenLedger  uint32  `json:"first_seen_ledger,omitempty"`
	ObservationCount int64   `json:"observation_count"`
	// Curator is set only on rows of CuratedAssets: whose word placed the
	// row there, and whether the verified set also carries it.
	Curator *RWACurator `json:"curator,omitempty"`
}

// RWAGroupTotal is one row of the per-declared-class breakdown.
type RWAGroupTotal struct {
	Class                   string  `json:"class"`
	Assets                  int     `json:"assets"`
	MarketCapUSD            *string `json:"market_cap_usd,omitempty"`
	AssetsUnvalued          int     `json:"assets_unvalued"`
	ReferenceValueUSD       *string `json:"reference_value_usd,omitempty"`
	AssetsReferenceUnvalued int     `json:"assets_reference_unvalued"`
	// Note explains a group that did not name itself (e.g. rows admitted
	// on an oracle binding with no declared class). Absent otherwise.
	Note string `json:"note,omitempty"`
}

// RWAIssuerTotal is one row of the per-issuer breakdown, keyed on the G-address rather than a company name.
type RWAIssuerTotal struct {
	Issuer                  string  `json:"issuer"`
	Name                    string  `json:"name,omitempty"`
	HomeDomain              string  `json:"home_domain,omitempty"`
	Assets                  int     `json:"assets"`
	MarketCapUSD            *string `json:"market_cap_usd,omitempty"`
	AssetsUnvalued          int     `json:"assets_unvalued"`
	ReferenceValueUSD       *string `json:"reference_value_usd,omitempty"`
	AssetsReferenceUnvalued int     `json:"assets_reference_unvalued"`
}

// RWARefusal counts the candidates one membership requirement turned away.
type RWARefusal struct {
	Reason string `json:"reason"`
	Assets int    `json:"assets"`
}
