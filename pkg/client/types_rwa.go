package client

import "time"

// ─── RWA value over time (/v1/rwa/history) ──────────────────────────

// RWAHistoryView is the /v1/rwa/history payload: the RWA set valued daily on the reference basis; Assets sizes the WHOLE set, Members is the subset in the series, and each point's AssetsUnvalued is counted against Assets.
type RWAHistoryView struct {
	Basis       string `json:"basis"`
	SupplyBasis string `json:"supply_basis,omitempty"`
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
	Members        int       `json:"members"`
	MembershipAsOf time.Time `json:"membership_as_of"`
	// MarketObservedFrom is the first day the served price history holds; nil when unread.
	MarketObservedFrom *time.Time                `json:"market_observed_from,omitempty"`
	Excluded           []RWAHistoryExcluded      `json:"excluded,omitempty"`
	Sources            []string                  `json:"sources,omitempty"`
	Series             []RWAPremiumSeries        `json:"series"`
	Coverage           []RWAPremiumCoveragePoint `json:"coverage,omitempty"`
	Truncated          bool                      `json:"truncated,omitempty"`
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
	Curator            string  `json:"curator"`
	CuratorCredit      string  `json:"curator_credit,omitempty"`
	Status             string  `json:"status"`
	Assets             int     `json:"assets"`
	AlsoVerified       int     `json:"also_verified"`
	AssetsValued       int     `json:"assets_valued"`
	AdditionalValueUSD *string `json:"additional_value_usd,omitempty"`
	CombinedValueUSD   *string `json:"combined_value_usd,omitempty"`
	VerifiedValueUSD   *string `json:"verified_value_usd,omitempty"`
	// AdditionalValueLowerBound / CombinedValueLowerBound: that figure leaves a row out or sums a floor supply.
	AdditionalValueLowerBound bool                 `json:"additional_value_lower_bound"`
	CombinedValueLowerBound   bool                 `json:"combined_value_lower_bound"`
	Census                    RWACuratedCensus     `json:"census"`
	Published                 *RWACuratedPublished `json:"published,omitempty"`
	Basis                     string               `json:"basis"`
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
	// BothBases totals only the members carrying BOTH figures. The totals above sum different rows, so
	// subtracting them measures nothing; these share rows and their difference is the aggregate gap.
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
	LowerBound        bool    `json:"lower_bound"` // a summed row's supply is only a floor, so both totals are floors
}

// RWAValuation carries a row valuation or the reason there is none; money fields are populated only when Status is "published".
type RWAValuation struct {
	Status   string  `json:"status"`
	PriceUSD *string `json:"price_usd,omitempty"`
	// PriceBasis names a price that is NOT a direct market observation
	// ("declared_peg" or "transitive"). Absent means market-derived.
	PriceBasis   string  `json:"price_basis,omitempty"`
	MarketCapUSD *string `json:"market_cap_usd,omitempty"`
	// MarketCapWithheldReason names why a priced row with a supply serves no market cap, verbatim from /v1/assets.
	MarketCapWithheldReason string `json:"market_cap_withheld_reason,omitempty"`
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
	VolumeLowerBound bool    `json:"volume_lower_bound,omitempty"` // Volume24hUSD excludes unpriced trades
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
	// MarketCapLowerBound / ReferenceValueLowerBound: that total leaves a member out or sums a floor supply.
	MarketCapLowerBound      bool `json:"market_cap_lower_bound"`
	ReferenceValueLowerBound bool `json:"reference_value_lower_bound"`
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
	// MarketCapLowerBound / ReferenceValueLowerBound: that total leaves a member out or sums a floor supply.
	MarketCapLowerBound      bool `json:"market_cap_lower_bound"`
	ReferenceValueLowerBound bool `json:"reference_value_lower_bound"`
}

// RWARefusal counts the candidates one membership requirement turned away.
type RWARefusal struct {
	Reason string `json:"reason"`
	Assets int    `json:"assets"`
}
