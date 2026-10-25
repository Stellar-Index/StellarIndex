package v1

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// Sep1CachedReader supplies the SEP-1 overlay from the payload the sep1-refresh cron persisted; it never fetches live (that dominated p95).
type Sep1CachedReader interface {
	GetIssuerSep1Cached(ctx context.Context, gStrkey string) (*timescale.IssuerSep1Cached, error)
}

// Sep1FetchStateReader reports whether an issuer's domain failed to serve a storable document; separate interface so existing stubs keep compiling.
type Sep1FetchStateReader interface {
	IssuerSep1Unreachable(ctx context.Context, gStrkey string) (bool, error)
}

// An optional seam that stops matching is a silent opt-out that reverts every failed issuer to
// not_fetched, so the production reader is asserted here. A caching wrapper implementing
// [Sep1CachedReader] must be added too.
var _ Sep1FetchStateReader = (*timescale.Store)(nil)

// warnIfSep1CacheLacksFetchState checks the value Options.Sep1Cache actually wires, which the compile-time assertion cannot: if
// it stops satisfying [Sep1FetchStateReader] every issuer reads "not_fetched" silently. Called once from [New].
func warnIfSep1CacheLacksFetchState(s *Server, logger *slog.Logger) {
	if s.Sep1Cache == nil {
		return
	}
	if _, ok := s.Sep1Cache.(Sep1FetchStateReader); !ok {
		logger.Warn("sep1 overlay: wired Sep1Cache does not implement Sep1FetchStateReader; " +
			"every issuer will report sep1_status=not_fetched instead of unreachable")
	}
}

// AssetReader is the storage-side interface for asset reads (*timescale.Store, in-memory test stubs).
type AssetReader interface {
	// GetAsset returns the canonical representation for the given
	// asset-id. Returns ErrAssetNotFound when the asset isn't yet
	// indexed (i.e. no trades observed for it).
	GetAsset(ctx context.Context, id canonical.Asset) (AssetDetail, error)

	// ListAssets returns a page of indexed assets. cursor = ""
	// starts at the beginning; limit clamped to [1, 500].
	ListAssets(ctx context.Context, cursor string, limit int) ([]AssetDetail, string, error)
}

// ErrAssetNotFound is what AssetReader.GetAsset returns for an unknown asset; handlers translate it to 404 problem+json.
var ErrAssetNotFound = errors.New("api: asset not found")

// TokenDecimalsReader resolves a Soroban token's decimals() from lake contract-instance metadata; found=false keeps the caller's default.
type TokenDecimalsReader interface {
	TokenDecimals(ctx context.Context, contractID string) (uint32, bool, error)
}

// AssetDetail is the /v1/assets payload, matching docs/reference/api-design.md section 5.2.
type AssetDetail struct {
	// Kind is the wire-shape discriminator for the /v1/assets/{asset_id} oneOf between AssetDetail
	// and GlobalAssetView (ADR-0042); it is always "stellar_asset" here. Type is the asset class
	// within that shape (native / classic / soroban / fiat / global / external).
	Kind       string  `json:"kind"`
	AssetID    string  `json:"asset_id"`
	Type       string  `json:"type"`
	Code       string  `json:"code,omitempty"`
	Issuer     *string `json:"issuer,omitempty"`
	ContractID *string `json:"contract_id,omitempty"`
	HomeDomain *string `json:"home_domain,omitempty"`
	// Decimals is the on-chain unit scale (7 for classic/native; per-contract for SEP-41) and the
	// divisor for all supply and market-cap/FDV math. It must never be set from SEP-1
	// display_decimals, a rounding hint that would inflate market cap by up to 10^(7-display) and
	// hand issuers a manipulation vector; use DisplayDecimals for that.
	Decimals int `json:"decimals"`
	// DisplayDecimals is the issuer's SEP-1 `display_decimals` rounding
	// hint (e.g. USDC declares 2) — a UI preference for how many
	// fractional digits to show, NOT a unit scale. Omitted when the
	// SEP-1 overlay didn't declare one. Never feeds amount math.
	DisplayDecimals *int `json:"display_decimals,omitempty"`

	// Class is the asset class ("fiat" | "stablecoin" | "crypto") from internal/currency's verified
	// catalogue, via the asset's slug. Omitted for classic rows with no catalogue entry; it backs
	// the ?asset_class= filter.
	Class string `json:"class,omitempty"`
	// Sep1Status is the SEP-1 overlay state: not_applicable (no home domain), not_fetched (no fetch
	// attempted; OURS, never a statement about the issuer), verified (matching [[CURRENCIES]]
	// entry), no_match (fetched, no matching issuer+code) or unreachable (a fetch produced nothing
	// storable, or a held payload past its attestation age whose domain is failing now; THEIRS, and
	// the one an issuer can act on).
	Sep1Status string `json:"sep1_status"`

	// ─── SEP-1 overlay fields (populated when Sep1Status=="verified") ─

	// Name is the currency's human-readable name from [[CURRENCIES]]
	// (e.g. "USD Coin").
	Name *string `json:"name,omitempty"`
	// Description is the currency's `desc` field (short blurb).
	Description *string `json:"description,omitempty"`
	// Image is an absolute URL to the asset logo (from `image`).
	Image *string `json:"image,omitempty"`
	// OrgName is the issuer organisation's name
	// (DOCUMENTATION.ORG_NAME in stellar.toml).
	OrgName *string `json:"org_name,omitempty"`
	// AnchorAsset is the off-chain asset this token anchors to (e.g.
	// "USD"). Empty for non-anchored tokens.
	AnchorAsset *string `json:"anchor_asset,omitempty"`
	// AnchorAssetType classifies the anchor (fiat, crypto, stock, …).
	AnchorAssetType *string `json:"anchor_asset_type,omitempty"`

	// ─── F2 fields (ADR-0011 supply derivation) ─────────────────

	// CirculatingSupply / TotalSupply / MaxSupply are decimal strings in the asset's smallest
	// integer unit (stroops for XLM / classic; contract-defined for SEP-41); consumers divide by
	// 10^decimals. Null when no supply snapshot exists for the asset.
	CirculatingSupply *string `json:"circulating_supply,omitempty"`
	TotalSupply       *string `json:"total_supply,omitempty"`
	MaxSupply         *string `json:"max_supply,omitempty"`

	// MarketCapUSD = circulating × USD price / 10^decimals,
	// formatted to two fractional digits. Null when supply or USD
	// price is unavailable.
	MarketCapUSD *string `json:"market_cap_usd,omitempty"`

	// FDVUSD = max_supply × USD price / 10^decimals. Null when
	// max_supply is null (uncapped issuer + no override + no SEP-1
	// declaration) or when USD price is unavailable.
	FDVUSD *string `json:"fdv_usd,omitempty"`

	// MarketCapLowLiquidity is true when market_cap_usd or fdv_usd were deliberately suppressed:
	// either the dust floor (single venue and 24h USD volume below
	// aggregate.min_market_cap_volume_usd) or the turnover ceiling (capExceedsObservedTurnover;
	// FDV, over max_supply, can breach it alone). It separates "suppressed on purpose" from "no
	// data"; price_usd still serves, only the valuation is guarded.
	MarketCapLowLiquidity bool `json:"market_cap_low_liquidity,omitempty"`

	// MarketCapDecimalsMismatch is true when market_cap_usd and fdv_usd were refused because the
	// lake's on-chain decimals() disagrees with the nonstandard_decimals_assets projection that
	// price paths normalise through. Price and supply would sit on different scales, so their
	// product is off by a power of ten. Each field still serves on its own scale; the flag
	// self-clears when the aggregator's lockstep reconcile repairs the projection.
	MarketCapDecimalsMismatch bool `json:"market_cap_decimals_mismatch,omitempty"`

	// ListingReference / ListingValuation are a listing-sourced second opinion for a verified asset
	// whose market cap this index declines to publish (asset_listing_valuation.go). They are NOT a
	// market cap and must never be summed into one: value_usd is supply times a third party's
	// figure for venues we do not observe. Present together when published; listing_valuation
	// alone, with only a status, when the arm refuses.
	ListingReference *AssetListingReference `json:"listing_reference,omitempty"`
	ListingValuation *AssetListingValuation `json:"listing_valuation,omitempty"`

	// Trust is the banded trust score and its factor breakdown
	// (asset_trust.go). Detail endpoint only; never an input to pricing.
	Trust *AssetTrust `json:"trust,omitempty"`

	// DecimalsUnresolved is true when `decimals` is the hardcoded default, not the token's own
	// on-chain value. Never serialised: it exists to stop scale-dependent cap/FDV being published.
	// Set for missing RWA contract readings ([Server.fillContractDecimals]) and for Soroban rows
	// whose lake read failed with no projection row ([Server.applyTokenDecimals]).
	DecimalsUnresolved bool `json:"-"`

	// SupplyBasis identifies which ADR-0011 policy produced the
	// supply numbers; null when no snapshot exists. Lets consumers
	// decide how much to trust the absolute value (e.g. `override`
	// indicates an operator curated the locked-set or max_supply).
	SupplyBasis *string `json:"supply_basis,omitempty"`

	// MaxSupplyBasis names where max_supply came from when that is not
	// the policy SupplyBasis names: `sep1_declared_max` for an issuer's
	// stellar.toml declaration. Omitted otherwise.
	MaxSupplyBasis *string `json:"max_supply_basis,omitempty"`

	// SupplyAsOf / SupplyAsOfLedger are when, and at which ledger, the
	// supply observation behind the supply fields was taken. Omitted when
	// the reading carries no vintage (the live lake-flows fallback).
	SupplyAsOf       *WireTime `json:"supply_as_of,omitempty"`
	SupplyAsOfLedger *uint32   `json:"supply_as_of_ledger,omitempty"`

	// SupplyStale is true when the only supply observation is older than
	// preciseSupplyMaxAge: the supply still serves with its as-of, but no
	// cap is derived from it and the response is flagged stale. INTERNAL.
	SupplyStale bool `json:"-"`

	// VolumeUSD24h is the trailing-24h USD trade volume across every pair the asset trades in (as
	// base or quote), as a decimal string. "0" means tracked with no trades; null means the
	// volume reader is not wired or the lookup failed.
	VolumeUSD24h *string `json:"volume_24h_usd,omitempty"`
	// VolumeLowerBound: VolumeUSD24h excludes trades not valued at trade time, or SAC volume that could not be read.
	VolumeLowerBound bool `json:"volume_lower_bound,omitempty"`

	// Change24hPct is the trailing-24h price change as a signed percentage with two fractional
	// digits, (now_usd - then_usd) / then_usd * 100, where then_usd is the latest closed prices_1m
	// bucket ending at or before now-24h. Null when there is no current USD price or no closed
	// bucket in that window, so clients render a dash rather than a fabricated "0%".
	Change24hPct *string `json:"change_24h_pct,omitempty"`

	// Asset-catalogue extension: fields lifted from AssetSummary so /v1/assets/{id} is a superset
	// of /v1/coins/{slug}. Populated only when an AssetsReader is wired and the asset has a
	// catalogue row (skipped for fiat:* and external:* assets).

	// PriceUSD is the latest VWAP/last-trade USD price as a decimal string, inlined to save a
	// /v1/price round-trip. The overlay reads the latest prices_1m bucket in a trailing 24h window
	// behind CachedAssetsReader (2 min TTL), so it can differ from /v1/price by ~0.1-0.2% on a
	// moving pair; callers needing cross-region determinism must read /v1/price. Null when no USD
	// price can be derived or it is withheld ([PriceWithheldReason]).
	PriceUSD *string `json:"price_usd,omitempty"`

	// PriceBasis identifies a PriceUSD that is not a Stellar market observation:
	// [priceBasisGlobalMarket], [priceBasisDeclaredPeg] (operator-declared 1:1 fiat peg times the
	// FX rate) or [priceBasisTransitive]. Fills run only when no market-derived price survived the
	// substance gate, and carry no change pills, sparkline-backed cap or other market-history
	// claims. Absent means market-derived.
	PriceBasis string `json:"price_basis,omitempty"`

	// PriceAuthority, PriceSources, PriceAsOf and PriceCredit are the catalogue price's provenance
	// (the GlobalAssetView triple plus the vendor credit), carried onto listing rows so a fiat or
	// catalogue row never serves one vendor's value without naming it. Absent on market-derived prices.
	PriceAuthority aggregate.PriceAuthority `json:"price_authority,omitempty"`
	PriceSources   []string                 `json:"price_sources,omitempty"`
	PriceAsOf      *WireTime                `json:"price_as_of,omitempty"`
	PriceCredit    string                   `json:"price_credit,omitempty"`

	// PriceWithheldReason says price_usd is null because the price was
	// WITHHELD, not because none exists, in the `price-withheld` 404's
	// reason vocabulary. Never set beside a price_usd.
	PriceWithheldReason PriceWithheldReason `json:"price_withheld_reason,omitempty"`

	// GlobalMarket is the global-market reference for a classic asset the
	// verified catalogue binds to a global ticker on its exact (code,
	// issuer), with the Stellar price's divergence from it. Omitted for
	// every other asset and when no fresh aggregator price exists.
	GlobalMarket *AssetGlobalMarket `json:"global_market,omitempty"`

	// IssuerBehaviour is a classic asset's issuer flags and SAC
	// mint/burn/clawback totals. Detail path only; omitted when neither resolves.
	IssuerBehaviour *AssetIssuerBehaviour `json:"issuer_behaviour,omitempty"`

	// ThinMarket marks a price_usd served under `include_thin=true` from a
	// market below the substance floor; no valuation or series derives from it.
	ThinMarket bool `json:"thin_market,omitempty"`

	// Substance is the measurement behind a thin price (detail only).
	Substance *SubstanceEvidence `json:"substance,omitempty"`

	// thinFloor is the floor a listing thin row failed, for its counter.
	thinFloor pricingguard.SubstanceFloor

	// Change1hPct / Change7dPct round out the trailing-window set
	// alongside Change24hPct. Same shape — signed percentage with
	// two fractional digits, null when no past-bucket snapshot
	// exists within the window's tolerance.
	Change1hPct *string `json:"change_1h_pct,omitempty"`
	Change7dPct *string `json:"change_7d_pct,omitempty"`

	// TopMarkets is a preview of the asset's top markets by 24h USD
	// volume. Up to 5 entries; null when the AssetsReader doesn't
	// return any.
	TopMarkets []AssetTopMarket `json:"top_markets,omitempty"`

	// PriceHistory24h / PriceHistory7d are sparkline-grade USD-price
	// timeseries (24 hourly + 7 daily samples respectively).
	PriceHistory24h []AssetPricePoint `json:"price_history_24h,omitempty"`
	PriceHistory7d  []AssetPricePoint `json:"price_history_7d,omitempty"`

	// MarketsCount / TradeCount24h are the trailing-24h activity
	// counters mirrored from AssetSummary. Pointers so 0 ("silent
	// 24h") is distinguishable from "not computed" (no reader / lookup
	// error) in alerting.
	MarketsCount  *int64 `json:"markets_count,omitempty"`
	TradeCount24h *int64 `json:"trade_count_24h,omitempty"`

	// ATH is the asset's all-time-high USD price + the day it was
	// set. Sourced from prices_1d, USD-quotes only.
	ATH *AssetATH `json:"ath,omitempty"`

	// IssuerScamReason is non-empty when this asset's issuer appears
	// in the curated scam directory. Clients should render a
	// prominent warning when present.
	IssuerScamReason string `json:"issuer_scam_reason,omitempty"`

	// IssuerDirectoryTags / IssuerDirectoryDomain / IssuerDirectoryName mirror the third-party
	// account_directory label for the issuer. Display-only, except scam-class tags, which withhold
	// dollar figures (suppressScamIssuerPricing) and demote the listing rank without touching
	// verified status or the substance/decimals gates. Distinct from IssuerScamReason (the
	// in-binary list). Best-effort: omitted when unlisted or the lookup fails.
	IssuerDirectoryTags   []string `json:"issuer_directory_tags,omitempty"`
	IssuerDirectoryDomain string   `json:"issuer_directory_domain,omitempty"`
	IssuerDirectoryName   string   `json:"issuer_directory_name,omitempty"`
	// issuerDirectoryUnchecked: the directory read for this issuer failed,
	// so the absence of a scam tag proves nothing and pricing is withheld.
	issuerDirectoryUnchecked bool
	// f2ReadFailed: a volume, 24h-change or supply read errored, so the
	// fields it feeds (incl. market_cap_usd / fdv_usd) are missing, not absent.
	f2ReadFailed bool

	// VolumeCharacter classifies trailing-window volume by account structure: "market" (default),
	// "operational" (an issuer-side mint/redeem wrap corridor: real, but not market activity) or
	// "concentrated" (>90% of volume in one account pair on a market-styled pair). Analytics only:
	// it does not re-rank or feed pricing or verification. Omitted when the reader is not wired.
	VolumeCharacter string `json:"volume_character,omitempty"`

	// VolumeCharacterSignals are the account-structure signals VolumeCharacter derives from.
	// Present whenever the reader is wired (zeroed when no priced trades fall in the window).
	VolumeCharacterSignals *AssetVolumeCharacterSignals `json:"volume_character_signals,omitempty"`

	// Slug is the short identifier ("USDC", or issuer-disambiguated "USDC-GA5Z..." on collision),
	// mirroring AssetSummary.Slug so consumers can build /assets/ URLs without a /v1/coins lookup.
	Slug string `json:"slug,omitempty"`

	// FirstSeenLedger, LastSeenLedger and ObservationCount mirror AssetRow's catalogue activity.
	// ObservationCount counts trades only (a holder-only asset carries 0). The ledger fields span
	// any source, trade or trustline, so FirstSeenLedger is an upper bound on genesis and neither
	// means "last traded". All three are absent with no catalogue row (native XLM); 0 is a real
	// ObservationCount, while a 0 ledger stays absent.
	FirstSeenLedger  *uint32 `json:"first_seen_ledger,omitempty"`
	LastSeenLedger   *uint32 `json:"last_seen_ledger,omitempty"`
	ObservationCount *int64  `json:"observation_count,omitempty"`

	// SEP-1 issuance declarations come from the issuer's stellar.toml [[CURRENCIES]] entry and are
	// populated only when Sep1Status == "verified". They are issuer-declared, not observed ledger
	// state like the F2 fields above.

	// Conditions is the issuer's declared terms for the asset (SEP-1 conditions), free-form text.
	Conditions *string `json:"conditions,omitempty"`

	// FixedNumber is the SEP-1-declared fixed total supply as a decimal string in the smallest
	// unit. It is what the issuer committed to, not the live-ledger total_supply.
	FixedNumber *string `json:"fixed_number,omitempty"`

	// MaxNumber is the SEP-1-declared maximum supply as a decimal string: the issuer's own ceiling,
	// distinct from the policy-derived max_supply.
	MaxNumber *string `json:"max_number,omitempty"`

	// IsUnlimited is the issuer's SEP-1 declaration that it reserves the right to issue without
	// bound. A pointer so "issuer did not say" differs from "said false".
	IsUnlimited *bool `json:"is_unlimited,omitempty"`

	Sep1CurrencyAuthority

	// UnverifiedWarning points at the verified asset when the requested asset uses a verified
	// currency's ticker code but is not the verified issuer (a ticker collision). Set by
	// handleAssetGet; pairs with Flags.UnverifiedTickerCollision.
	UnverifiedWarning *UnverifiedWarning `json:"unverified_warning,omitempty"`

	// FiatCodeAnchor points at the catalogue's fiat denomination when the asset's code names a
	// sovereign currency with no Stellar issuance of its own. A fiat code is a denomination, not
	// an impersonation claim, so it never suppresses market_cap_usd (unlike UnverifiedWarning).
	FiatCodeAnchor *FiatCodeAnchor `json:"fiat_code_anchor,omitempty"`

	// UnverifiedTickerCollision is the listing-row form of UnverifiedWarning: true when this
	// (code, issuer) uses a verified currency's ticker but is not the verified issuer. The listing
	// serves COALESCE(slug, code) AS slug, so an impersonator with a NULL slug emits the verified
	// CODE as its slug; consumers must AND a verified-slug check with
	// !unverified_ticker_collision or they would badge the impersonator verified.
	UnverifiedTickerCollision bool `json:"unverified_ticker_collision,omitempty"`
}

// UnverifiedWarning is attached when an asset code collides with a verified currency but the issuer differs; every field is safe to render verbatim.
type UnverifiedWarning struct {
	// VerifiedSlug is the canonical slug ("usdc") consumers can
	// redirect to.
	VerifiedSlug string `json:"verified_slug"`
	// VerifiedAssetID is the verified canonical asset_id ("USDC-G…").
	VerifiedAssetID string `json:"verified_asset_id"`
	// VerifiedName is the human-readable name ("USD Coin").
	VerifiedName string `json:"verified_name"`
	// VerifiedIssuer is the verified-issuer attribution
	// ("Circle (centre.io)"). Empty when the catalogue entry
	// didn't include a verified_issuer_label.
	VerifiedIssuer string `json:"verified_issuer,omitempty"`
	// Note is a one-sentence warning rendered verbatim by clients.
	// Composed server-side from the verified currency's metadata
	// so the wording stays consistent.
	Note string `json:"note"`
}

// FiatCodeAnchor is attached when a classic asset's code names a fiat denomination with no Stellar issuance; neutral, never suppresses market_cap_usd.
type FiatCodeAnchor struct {
	// Ticker is the ISO-4217 code the asset denominates in ("USD").
	Ticker string `json:"ticker"`
	// KnownAnchor is true when the issuer is in the catalogue's
	// operator-curated known-anchor set for this ticker — an
	// affirmatively recognised anchor rather than merely unflagged.
	KnownAnchor bool `json:"known_anchor"`
	// Note is a one-sentence, verbatim-safe-to-render summary.
	Note string `json:"note"`
}

// detailFromAsset builds an AssetDetail from the canonical shape when no AssetReader is wired (tests, dev); HomeDomain stays
// nil and sep1_status defaults to "not_applicable".
func detailFromAsset(a canonical.Asset) AssetDetail {
	d := AssetDetail{
		Kind:    "stellar_asset",
		AssetID: a.String(),
		Type:    string(a.Type),
		Code:    a.Code,
		// Classic and native are 7 by protocol; Soroban tokens get their real decimals() overlaid by
		// applyTokenDecimals when the lake has the instance metadata, else 7 is the default.
		Decimals:   7,
		Sep1Status: "not_applicable",
	}
	if a.Issuer != "" {
		v := a.Issuer
		d.Issuer = &v
	}
	if c, ok := a.ContractAddress(); ok {
		d.ContractID = &c
	}
	return d
}

// ─── Asset reader on the Server ──────────────────────────────────

// assetReaderOrNil returns the AssetReader, nil during scaffolding; handlers degrade to a 503 "feature unavailable".
func (s *Server) assetReaderOrNil() AssetReader { return s.Assets }

// ─── Handlers ─────────────────────────────────────────────────────

// assetListFilters holds the validated row filters (type, code, issuer, q) shared by the /v1/assets paths; "any" collapses to
// "". Every path takes the struct whole, so a dropped filter is a compile error, not a plausible wrong page.
type assetListFilters struct {
	typ    string // "" | native | classic | soroban | fiat
	code   string // exact classic code, case-sensitive
	issuer string // G-strkey, CRC-checked
	q      string // case-insensitive substring over code / slug / issuer
}

// ignored returns the supplied filter names, in OpenAPI order, for a path that serves its page without applying them
// ([Flags.FiltersIgnored]); a dropped-filter page is otherwise the same 200 as a matched one. "type" counts only when it
// narrows.
func (f assetListFilters) ignored() []string {
	var out []string
	for _, p := range []struct{ name, value string }{
		{"type", f.typ}, {"code", f.code}, {"issuer", f.issuer}, {"q", f.q},
	} {
		if p.value != "" {
			out = append(out, p.name)
		}
	}
	return out
}

// unfiltered reports whether the caller narrowed the listing. Only then does a row publish the cross-arm total: filters narrow
// the spine before [Server.foldAliasTwins], so a filtered page can only report what it selected.
func (f assetListFilters) unfiltered() bool {
	return f == assetListFilters{}
}

// assetsOrderParam is the parsed order_by: the store order plus whether the client asked for one, so fixed-order paths can
// reject an order they cannot honour yet serve requests that omit it.
type assetsOrderParam struct {
	order    timescale.AssetsOrder
	explicit bool
	raw      string
}

// parseAssetsOrder validates `order_by`, writing a 400 and returning ok=false on an unrecognised value. Only this step decides
// the ranking: dropping it would serve observation-count order to a client asking for 24h volume.
func parseAssetsOrder(w http.ResponseWriter, r *http.Request) (assetsOrderParam, bool) {
	raw := r.URL.Query().Get("order_by")
	switch raw {
	case "":
		return assetsOrderParam{order: timescale.AssetsOrderObservationCountDesc}, true
	case "observation_count_desc":
		return assetsOrderParam{
			order:    timescale.AssetsOrderObservationCountDesc,
			explicit: true,
			raw:      raw,
		}, true
	case "volume_24h_usd_desc":
		return assetsOrderParam{
			order:    timescale.AssetsOrderVolume24hUSDDesc,
			explicit: true,
			raw:      raw,
		}, true
	default:
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-order",
			"Invalid order_by", http.StatusBadRequest,
			"order_by must be 'observation_count_desc' or 'volume_24h_usd_desc'")
		return assetsOrderParam{}, false
	}
}

// parseAssetListFilters validates the type/code/issuer filters; writes a 400 and returns ok=false on a malformed value, so bad
// input fails fast whichever path would serve it.
func parseAssetListFilters(w http.ResponseWriter, r *http.Request) (assetListFilters, bool) {
	q := r.URL.Query()
	var f assetListFilters

	// type: structural asset class. "any"/"" disable the filter.
	switch t := strings.TrimSpace(q.Get("type")); t {
	case "", "any":
		// no filter
	case "native", "classic", "soroban", "fiat":
		f.typ = t
	default:
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-type",
			"Invalid type", http.StatusBadRequest,
			"type must be one of native, classic, soroban, fiat, any")
		return assetListFilters{}, false
	}

	// code: exact classic asset code — case-sensitive, 1-12 alnum
	// (Stellar's alphanum4/alphanum12 rule, matching canonical's
	// validateClassicAssetCode).
	if c := strings.TrimSpace(q.Get("code")); c != "" {
		if !isValidClassicCode(c) {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-code",
				"Invalid code", http.StatusBadRequest,
				"code must be 1-12 alphanumeric characters (a Stellar asset code)")
			return assetListFilters{}, false
		}
		f.code = c
	}

	// issuer: G-strkey, CRC-checked (same validator as /v1/accounts).
	if iss := strings.TrimSpace(q.Get("issuer")); iss != "" {
		if !canonical.IsAccountID(iss) {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-issuer",
				"Invalid issuer", http.StatusBadRequest,
				"issuer must be a valid Stellar account G-strkey")
			return assetListFilters{}, false
		}
		f.issuer = iss
	}

	// q: free-text search. Any string is a legal substring, so the only
	// thing to validate is its size — see [assetListMaxQueryLen].
	f.q = strings.TrimSpace(q.Get("q"))
	if len(f.q) > assetListMaxQueryLen {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-parameter",
			"Invalid q", http.StatusBadRequest,
			fmt.Sprintf("q may be at most %d bytes", assetListMaxQueryLen))
		return assetListFilters{}, false
	}

	return f, true
}

// assetListMaxQueryLen bounds the free-text `q` (bytes). It goes verbatim into the cache key and three LIKE patterns; the
// longest useful value is a 69-byte classic asset id, so more only costs.
const assetListMaxQueryLen = 100

// Rate-limit surcharges for the /v1/assets plans. One route covers several query plans but the
// limiter pre-charges one token, so each surcharge is added via [middleware.ChargeRateLimit]. The
// weights sit deliberately below the measured cost ratio so an error under-charges an abuser
// rather than locking out an honest client; re-derive them from the slow-request log when the
// plans change.
const (
	// assetListSurchargeVolumePlan prices the volume-ranked plan (concentration-adjusted sort over
	// per-asset CTEs), measured ~18x the default ordering. 10 tokens in total.
	assetListSurchargeVolumePlan = 9

	// assetListSurchargeSearch prices a q that reaches the store: three unindexed LOWER(col) LIKE predicates over the ~190K-row
	// spine behind a cache keyed on q verbatim, so cycling values always misses. 5 tokens in total.
	assetListSurchargeSearch = 4
)

// assetListCost is the rate-limit price of one /v1/assets request, decided by the PLAN: `order_by=volume_24h_usd_desc` and
// `asset_class=all` hit the same volume-ranked read, so both pay. No AssetsReader, or class-scoped catalogue listings (which
// ignore `q`), cost the base token.
func assetListCost(f assetListFilters, order timescale.AssetsOrder, assetClass string, storeBacked bool) int {
	const base = 1
	if !storeBacked || (assetClass != "" && assetClass != "all") {
		return base
	}
	cost := base
	if assetClass == "all" || order == timescale.AssetsOrderVolume24hUSDDesc {
		cost += assetListSurchargeVolumePlan
	}
	if f.q != "" {
		cost += assetListSurchargeSearch
	}
	return cost
}

// parseAssetListLimit validates `limit` (integer in [1, 500], default 100); ok=false means a 400 was written. Split out of
// handleAssetList for the cyclomatic budget.
func parseAssetListLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 100, true
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 || parsed > 500 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-limit",
			"Invalid limit", http.StatusBadRequest,
			"limit must be an integer in [1, 500]")
		return 0, false
	}
	return parsed, true
}

// typeMatchesListingSpine reports whether a `type` filter can match any row of the classic listing spine (classic_assets plus
// traded Soroban-native contracts); on false the caller serves an empty page.
func typeMatchesListingSpine(typ string) bool {
	return typ == "" || typ == "classic" || typ == "soroban"
}

// isValidClassicCode reports whether s is a classic asset code: 1-12 bytes, [A-Za-z0-9] only (mirrors the unexported
// canonical.validateClassicAssetCode).
func isValidClassicCode(s string) bool {
	if len(s) == 0 || len(s) > 12 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alnum {
			return false
		}
	}
	return true
}

// handleAssetList serves GET /v1/assets (parameters in OpenAPI). Row filters narrow the spine before the alias fold, so a
// filtered SAC-wrapped asset's volume_24h_usd is its classic arm alone. Class-scoped catalogue listings ignore them rather than
// 400 and name them in flags.filters_ignored. No AssetReader returns an empty list in the normal envelope.
func (s *Server) handleAssetList(w http.ResponseWriter, r *http.Request) {
	// Parse + validate query params FIRST — bad input is 400
	// regardless of whether the backing reader is wired.
	cursor := r.URL.Query().Get("cursor")
	limit, ok := parseAssetListLimit(w, r)
	if !ok {
		return
	}

	// Row filters (type / code / issuer). Validated BEFORE the
	// asset_class dispatch so malformed input 400s on every path,
	// not just the assetsReader-backed one.
	filters, ok := parseAssetListFilters(w, r)
	if !ok {
		return
	}
	filters.q = s.sacSearchAsClassic(r, filters.q)

	// order_by is validated here so an unrecognised value 400s on every path, not only the one
	// that reads it.
	orderBy, ok := parseAssetsOrder(w, r)
	if !ok {
		return
	}

	assetClass := normaliseAssetClass(r.URL.Query().Get("asset_class"))

	// An unrecognised asset_class 400s rather than silently not applying. There is deliberately no
	// rwa class: that set is decided by attestation, not a column, and lives at /v1/rwa/assets.
	if !validAssetClass(assetClass) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-class",
			"Invalid asset_class", http.StatusBadRequest,
			"asset_class must be one of fiat, stablecoin, crypto or all "+
				"(blockchain, cryptocurrency and cryptocurrencies fold to "+
				"crypto), or be omitted for the default listing. Tokenized "+
				"real-world assets are not an asset_class — they are served "+
				"by GET /v1/rwa/assets, which decides membership by issuer "+
				"attestation rather than by a class column.")
		return
	}

	// The catalogue and unified listings rank on their own fixed schemes, each
	// with a keyset cursor encoding THAT scheme's keys, so they cannot honour a
	// caller-chosen order. Say so rather than serve a ranking that is not the one
	// advertised.
	if orderBy.explicit && assetClass != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-order",
			"Invalid order_by", http.StatusBadRequest,
			"order_by is not supported together with asset_class — the "+
				"asset_class listings rank on their own fixed scheme "+
				"(catalogue by market cap, then classic by 24h volume). "+
				"Request order_by without asset_class.")
		return
	}

	// Price the request by the plan it selected — after validation, so a
	// 400 costs the base token only, and before any path reads.
	if !middleware.ChargeRateLimit(w, r, assetListCost(filters, orderBy.order, assetClass, s.AssetsReader != nil)) {
		return
	}

	if assetClass == "fiat" || assetClass == "stablecoin" || assetClass == "crypto" {
		s.handleAssetListFromCatalogue(w, r, assetClass, filters, limit, cursor)
		return
	}

	// Unified listing: catalogue rows (market-cap ordered) then classic_assets by volume; the
	// cursor encodes the phase. Opt-in so SDK consumers of the classic-only listing see no
	// wire-shape change.
	if assetClass == "all" {
		s.handleAssetListUnified(w, r, filters, limit, cursor)
		return
	}

	// With an AssetsReader wired, list via the ListAssetsExt path /v1/coins uses, which carries the
	// price, volume, change, sparkline and ATH fields; otherwise fall to the lean AssetReader.
	if s.AssetsReader != nil {
		s.handleAssetListFromAssets(w, r, filters, cursor, limit, orderBy.order)
		return
	}

	// Lean AssetReader fallback: ListAssets takes only a cursor and limit, so none of the row
	// filters reaches it. Report which were dropped.
	leanFlags := Flags{FiltersIgnored: filters.ignored()}

	reader := s.assetReaderOrNil()
	if reader == nil {
		// Feature not wired yet — empty list is consistent with
		// the contract and doesn't force a 503.
		writeJSON(w, []AssetDetail{}, leanFlags)
		return
	}

	rows, next, err := reader.ListAssets(r.Context(), cursor, limit)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("ListAssets failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError,
			"")
		return
	}
	// A nil slice would marshal as "data": null, violating the OpenAPI required-array contract;
	// don't depend on every reader returning non-nil empty.
	if rows == nil {
		rows = []AssetDetail{}
	}

	env := Envelope{
		Data:  rows,
		Flags: leanFlags,
	}
	if next != "" {
		env.Pagination = &Pagination{Next: next}
	}
	writeEnvelope(w, env)
}

// AssetsListOverfetchBy is how far past limit the classic listing requests so the extra row signals a next page; exported so the prewarm cache-key set mirrors it.
const AssetsListOverfetchBy = 1

// handleAssetListFromAssets serves /v1/assets when an AssetsReader is wired; code and issuer push down to the indexed
// classic_assets columns. The same order must be threaded through cursor validate and encode: the observation_count_desc and
// volume_24h_usd_desc keysets differ, and the wrong one rejects a good cursor or silently skips/repeats rows.
func (s *Server) handleAssetListFromAssets(
	w http.ResponseWriter,
	r *http.Request,
	filters assetListFilters,
	cursor string,
	limit int,
	order timescale.AssetsOrder,
) {
	// Reject a malformed cursor up front: it would otherwise hit the keyset predicate's degenerate
	// (0, "") case, matching no rows and reading as a quiet end of pagination.
	if err := timescale.ValidateAssetsCursor(cursor, order); err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest, err.Error())
		return
	}
	if !typeMatchesListingSpine(filters.typ) {
		writeEnvelope(w, Envelope{Data: []AssetDetail{}, Flags: Flags{}})
		return
	}
	key := assetListCacheKey(r)
	if body, refresh, ok := s.assetListCache.lookup(key); ok {
		if refresh {
			s.refreshAssetListPage(r, key, filters, cursor, limit, order) //nolint:contextcheck // the refresh outlives this request on purpose; see its doc.
		}
		writeCachedAssetList(w, body)
		return
	}
	env, rows, err := s.buildAssetListPage(r, filters, cursor, limit, order)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("ListAssetsExt (assets listing) failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	// A page built on a dead context holds whatever best-effort reads
	// landed before cancellation; it is answered, never replayed.
	if r.Context().Err() == nil && assetListPageCacheable(env, rows) {
		if err := s.assetListCache.put(key, env); err != nil {
			s.logger.Debug("assets listing render for cache failed", "err", err)
		}
	}
	writeEnvelope(w, env)
}

// buildAssetListPage assembles one default-listing page: the cached rows plus every per-request overlay, in their load-bearing
// order; returns the served rows so the caller can judge cacheability.
func (s *Server) buildAssetListPage(
	r *http.Request,
	filters assetListFilters,
	cursor string,
	limit int,
	order timescale.AssetsOrder,
) (Envelope, []AssetDetail, error) {
	// Overfetch by one: the (limit+1)th row signals a next page. Passing limit itself would make
	// len(rows) > limit never true and only the first page reachable.
	opts := timescale.ListAssetsOptions{
		Limit:  limit + AssetsListOverfetchBy,
		Issuer: filters.issuer,
		Code:   filters.code,
		Type:   filters.typ,
		Q:      filters.q,
		Cursor: cursor,
		Order:  order,
	}
	// This path can serve a row set observed by a previous process (boot seed, prewarm), which is
	// correct only if the envelope is not stamped stale:false / as_of=now; observedAt and stale
	// carry the rows' real provenance.
	rows, observedAt, stale, err := s.listAssetsExtAt(r.Context(), opts)
	if err != nil {
		return Envelope{}, nil, err
	}
	// The +1th row only signals a next page and is not returned.
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	out := make([]AssetDetail, 0, len(rows))
	for _, row := range rows {
		out = append(out, assetDetailFromAssetRow(row))
	}
	s.stampListingCollisions(out)
	unmeasured := s.applySubstanceGateToListing(r.Context(), out, includeThinRequested(r))
	s.fillMarketCapsFromSupply(r.Context(), out, assetRowSourceCounts(rows))
	// After the gate (which would strip it) and the market-cap fill (so no valuation derives from
	// it); see fillDeclaredPegPricesInListing's ordering contract.
	s.fillDeclaredPegPricesInListing(r.Context(), out)
	// One batch query (no N+1) for the page's issuer labels. Must run before the listing-valuation
	// arm: that arm refuses scam-tagged issuers using a field only this call populates, so run
	// first it would publish a dollar figure under a nulled price_usd. Every price producer runs
	// above this line, so none can undo the suppression.
	s.fillIssuerDirectoryTags(r.Context(), out)
	settleListingThinRows(out)
	// Last among the valuation producers: it reads the outcome of all of them (cap, gated price,
	// dust flag, peg basis, scam tags) to decide whether there is a hole to fill. See
	// asset_listing_valuation.go.
	s.applyListingValuations(r.Context(), out)
	s.fillImagesFromSep1(r.Context(), out)
	// Last, after every price producer and both gates: the sparkline attaches only to rows that
	// still publish a price.
	s.attachSparklinesIfRequested(r, out)
	// Honest freshness as /v1/markets does for SWR reads: cached rows are stamped with their
	// actual observation time and, past the TTL, flags.stale, never now() over a boot-seeded or
	// refresh-starved page set. An uncached read stamps the build time once, so a cached replay
	// carries the same as_of as the response that built it.
	env := Envelope{Data: out, Flags: Flags{Stale: stale || unmeasured, Degraded: stale || unmeasured || anyDirectoryUnchecked(out)}, AsOf: WireTime(time.Now().UTC())}
	if !observedAt.IsZero() {
		env.AsOf = WireTime(observedAt.UTC())
	}
	if hasMore && len(out) > 0 {
		// Keyset cursor from the raw last row, encoded by the store so it carries every ORDER BY
		// key; fewer keys skip or repeat rows across pages.
		env.Pagination = &Pagination{
			Next: timescale.EncodeAssetsCursor(rows[len(rows)-1], order),
		}
	}
	return env, out, nil
}

// fillMarketCapsFromSupply fills market_cap and circulating_supply on rows with a supply reading and USD price, best-effort.
// sourceCounts maps asset_id to distinct price venues; a single-venue price under aggregate.min_market_cap_volume_usd has its
// cap suppressed, and a missing entry is unmeasured.
func (s *Server) fillMarketCapsFromSupply(ctx context.Context, rows []AssetDetail, sourceCounts map[string]int) {
	// Precise supply: the ADR-0011 observer's latest reading, bounded by [preciseSupplyMaxAge].
	// Authoritative (includes claimable and LP-locked holdings), so preferred.
	precise := s.latestPreciseSupply(ctx)
	// Broad fallback: cached trustline-balance sums from the lake for every classic asset. It
	// undercounts (omits claimable and LP), so it only fills rows the precise pipeline misses.
	broad := s.cachedClassicSupply(ctx)
	// Lake-flows supply (mint - burn - clawback over the SAC) is the only reading not keyed on where
	// tokens are held, so it alone sees claimable balances, LP reserves and SAC balances. It
	// outranks the trustline sum but never falls below it; see classic_lake_supply.go.
	lake := s.classicLakeSupply(ctx, rows)
	if len(precise) == 0 && len(broad) == 0 && len(lake) == 0 {
		return
	}
	// Bounded once for the whole page, not per row (tokenMetadataReadTimeout
	// is a PER-CALL budget and a listing can carry many Soroban rows) — same
	// shape as rwaContractMetadataBudget for the contract-only surface.
	dctx, cancel := context.WithTimeout(ctx, listingDecimalsLockstepBudget)
	defer cancel()
	for i := range rows {
		s.fillRowMarketCap(dctx, &rows[i], precise, lake, broad, sourceCounts)
	}
}

// listingDecimalsLockstepBudget bounds the total time [fillMarketCapsFromSupply]
// spends on live lake decimals() reads across an entire listing page.
const listingDecimalsLockstepBudget = 5 * time.Second

// preciseSupplyReader is the optional seam [Server.latestPreciseSupply] reads the ADR-0011 supply observer through; named so
// production readers are checked at compile time, since a drifted optional seam is a silent opt-out.
type preciseSupplyReader interface {
	LatestSupplyObservations(context.Context, time.Duration) (map[string]timescale.SupplyObservation, error)
}

// Compile-time proof that both production readers still satisfy the seam: the
// bare store, and the cache the binary actually wires in front of it.
var (
	_ preciseSupplyReader = (*timescale.Store)(nil)
	_ preciseSupplyReader = (*CachedAssetsReader)(nil)
)

// latestPreciseSupply reads the supply observer's latest observation per asset, authoritative (all four holding domains,
// locked-set policy); missing method or read error yields nil. Bounded by [preciseSupplyMaxAge] so a stale figure cannot
// outrank a live lake reading; supply_1d is no substitute (newest bucket ages to ~27h).
func (s *Server) latestPreciseSupply(ctx context.Context) map[string]timescale.SupplyObservation {
	sr, ok := s.AssetsReader.(preciseSupplyReader)
	if !ok {
		return nil
	}
	m, err := sr.LatestSupplyObservations(ctx, preciseSupplyMaxAge)
	if err != nil {
		return nil
	}
	return m
}

// fillRowMarketCap fills one row's market cap from circulating supply. A dust-liquidity price (single venue AND sub-floor 24h
// volume) must not present an obscure asset as worth billions, so the cap is null (market_cap_low_liquidity=true); price_usd
// and circulating_supply stay.
func (s *Server) fillRowMarketCap(
	ctx context.Context,
	row *AssetDetail,
	precise map[string]timescale.SupplyObservation,
	lake, broad map[string]string,
	sourceCounts map[string]int,
) {
	if row.MarketCapUSD != nil || row.PriceUSD == nil || row.ThinMarket {
		return
	}
	circ, basis := classicSupplyReading(row.AssetID, precise, lake, broad)
	if circ == "" {
		return
	}
	// Must precede every branch below: each stamps a smallest-unit supply figure that `decimals`
	// has to be able to scale, whether or not a cap goes out beside it.
	s.applyConfirmedListingDecimals(ctx, row)
	if row.MarketCapDecimalsMismatch || row.DecimalsUnresolved {
		// Lockstep refusal, as populateMarketCap on the detail path: the price was normalised
		// through the nonstandard_decimals_assets projection while row.Decimals now holds a
		// disagreeing lake reading, so supply over price would be off by a power of ten. Null
		// with a reason beats that; circulating_supply is a raw fact and still serves.
		stampCirculatingSupply(row, circ, basis)
		return
	}
	// A ticker-collision look-alike gets no price x supply headline: its price comes from its own
	// manipulable market while its ticker borrows a real asset's standing. The
	// unverified_ticker_collision flag is the reason; circulating_supply still surfaces.
	if row.UnverifiedTickerCollision {
		stampCirculatingSupply(row, circ, basis)
		return
	}
	// Native carve-out: the listing SQL forces native's source_count to 0, and XLM is
	// definitionally liquid, so it must never be dust-suppressed on an unmeasured count.
	if row.AssetID != "native" &&
		dustLiquiditySuppressed(sourceCounts[row.AssetID], row.VolumeUSD24h, s.MinMarketCapVolumeUSD) {
		row.MarketCapLowLiquidity = true
		stampCirculatingSupply(row, circ, basis)
		return
	}
	mc := computeMarketCapUSD(circ, *row.PriceUSD, row.Decimals)
	if mc == "" {
		return
	}
	// The turnover ceiling tests the computed claim, not the inputs, so the absolute floor above
	// cannot stand in for it.
	if row.AssetID != "native" &&
		capExceedsObservedTurnover(mc, row.VolumeUSD24h, s.MaxMarketCapVolumeRatio) {
		row.MarketCapLowLiquidity = true
		stampCirculatingSupply(row, circ, basis)
		return
	}
	row.MarketCapUSD = &mc
	stampCirculatingSupply(row, circ, basis)
}

// applyConfirmedListingDecimals puts a row's supply divisor on its price's scale: rows are stamped 7, but asset_price_snapshot
// holds true-scale prices for confirmed nonstandard_decimals_assets, so supply / 10^7 would be off by 10^(decimals - 7). Fresh
// Soroban tokens the projection lags also try the bounded lake read and lockstep refusal of [Server.applyTokenDecimals].
func (s *Server) applyConfirmedListingDecimals(ctx context.Context, row *AssetDetail) {
	confirmed, hasConfirmed := s.NonstandardDecimals.Lookup(row.AssetID)
	if hasConfirmed {
		row.Decimals = confirmed
	}
	if row.Type != string(canonical.AssetSoroban) {
		return
	}
	lake, lakeKnown, lakeErr := s.lakeTokenDecimals(ctx, row.AssetID)
	if !lakeKnown {
		if !hasConfirmed && lakeErr != nil {
			row.DecimalsUnresolved = true
		}
		return
	}
	row.Decimals = lake
	if (hasConfirmed && confirmed != lake) || (!hasConfirmed && lake != aggregate.StandardDecimals) {
		row.MarketCapDecimalsMismatch = true
		// Request-driven, so labelled by site and outcome only: a per-asset label here would be
		// unbounded cardinality (see obs.NonstandardDecimalsLockstepMismatchTotal).
		obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("asset_listing", "").Inc()
	}
}

// applySubstanceGateToListing extends the thin-market substance gate to listing prices (catalogue SQL computes price_usd outside /v1/price):
// allowed when any plausible backing pair (XLM, fiat:USD, declared USD peg) clears the floor. Withheld or unmeasured rows lose price_usd and
// change pills and let the caller stamp flags.stale; under admitThin a measured-thin row keeps price_usd flagged thin_market.
func (s *Server) applySubstanceGateToListing(ctx context.Context, rows []AssetDetail, admitThin bool) (unmeasured bool) {
	if s.Substance == nil {
		return false
	}
	for i := range rows {
		row := &rows[i]
		if row.PriceUSD == nil {
			continue
		}
		asset, err := canonical.ParseAsset(row.AssetID)
		if err != nil {
			continue
		}
		allowed, measured, floor := s.listingSubstanceVerdict(ctx, asset, admitThin)
		if allowed {
			continue
		}
		switch {
		case measured && admitThin:
			row.ThinMarket = true
			row.thinFloor = floor
		case measured:
			row.PriceWithheldReason = PriceWithheldSubstance
			row.PriceUSD = nil
		default:
			unmeasured = true
			row.PriceUSD = nil
		}
		row.Change1hPct = nil
		row.Change24hPct = nil
		row.Change7dPct = nil
	}
	return unmeasured
}

// settleListingThinRows drops the thin flag from rows a later overlay unpriced and counts rows still served thin, so a peg-won
// or scam-nulled row never counts as a thin serve.
func settleListingThinRows(rows []AssetDetail) {
	for i := range rows {
		row := &rows[i]
		if !row.ThinMarket {
			continue
		}
		if row.PriceUSD == nil {
			row.ThinMarket = false
			continue
		}
		obs.PriceServeThinAdmittedTotal.WithLabelValues("listing", string(row.thinFloor)).Inc()
	}
}

// listingPriceAllowed is [Server.listingSubstanceVerdict] reduced to yes/no; it fails CLOSED (unmeasured is not allowed).
func (s *Server) listingPriceAllowed(ctx context.Context, asset canonical.Asset) bool {
	return s.assetPriceAllowed(ctx, asset, "listing")
}

// assetPriceAllowed is [Server.listingPriceAllowed] for a caller that is
// not the listing, counted under its own `surface`.
func (s *Server) assetPriceAllowed(ctx context.Context, asset canonical.Asset, surface string) bool {
	allowed, _ := pricingguard.AssetSubstanceVerdict(ctx, s.Substance, asset, s.USDPeggedClassics, surface)
	return allowed
}

// listingSubstanceVerdict is the listing's per-row [pricingguard.AssetSubstanceVerdict] over the operator's USD pegs.
// measured=false when no quote cleared and one could not be measured (unverified, not withheld on evidence). Under admitThin a
// thin verdict is left uncounted, as the row's fate is known only after peg fill and scam suppression.
func (s *Server) listingSubstanceVerdict(ctx context.Context, asset canonical.Asset, admitThin bool) (allowed, measured bool, floor pricingguard.SubstanceFloor) {
	if admitThin {
		return pricingguard.AssetSubstanceVerdictThinServed(ctx, s.Substance, asset, s.USDPeggedClassics, "listing")
	}
	allowed, measured = pricingguard.AssetSubstanceVerdict(ctx, s.Substance, asset, s.USDPeggedClassics, "listing")
	return allowed, measured, pricingguard.FloorNone
}

// priceBasisDeclaredPeg is the [AssetDetail.PriceBasis] value when PriceUSD was filled from an
// operator-declared 1:1 fiat peg times the fiat-to-USD FX rate rather than observed on a market.
const priceBasisDeclaredPeg = "declared_peg"

// priceBasisTransitive marks a price derived through one intermediate hop (asset -> hop -> USD proxy), so consumers can weight
// it below a direct price; both legs must clear the substance floors.
const priceBasisTransitive = "transitive"

// declaredPegFXMaxAge bounds the fiat-to-USD rate age behind a declared-peg fill: the 7-day window [Server.fiatUSDPriceFor]
// enforces on fx_quotes (daily ECB rates), extended to the PriceReader fallback whose stale flag is discarded. An older rate
// leaves the price nil.
const declaredPegFXMaxAge = 7 * 24 * time.Hour

// fillDeclaredPegPricesInListing applies [Server.fillDeclaredPegPrice] per row, memoising FX per fiat ticker. Order is
// load-bearing: after applySubstanceGateToListing (which nils PriceUSD and would strip the fill) and after
// fillMarketCapsFromSupply, so a peg-filled row keeps market_cap null.
func (s *Server) fillDeclaredPegPricesInListing(ctx context.Context, rows []AssetDetail) {
	refs := s.globalMarketRefs(ctx, rows)
	if len(s.FiatPeggedClassics) == 0 && refs == nil {
		return
	}
	now := time.Now()
	memo := make(map[string]*string, 1)
	for i := range rows {
		s.applyGlobalMarket(&rows[i], refs, now)
		s.fillFixedPegPrice(ctx, &rows[i], memo)
	}
}

// fillDeclaredPegPrice fills PriceUSD from the declared fiat peg (pricing_guard.fiat_pegged_classic_assets) only when no market price
// survived, stamping PriceBasis="declared_peg". memo caches fiat ticker to price per request (nil value = unavailable); nil on single-row
// paths. A vetted global ticker's market price ([Server.applyGlobalMarket]) fills first: a fixed peg would hide a depeg.
func (s *Server) fillDeclaredPegPrice(ctx context.Context, row *AssetDetail, memo map[string]*string) {
	s.applyGlobalMarket(row, s.globalMarketRefs(ctx, []AssetDetail{*row}), time.Now())
	s.fillFixedPegPrice(ctx, row, memo)
}

// fillFixedPegPrice is the declared fiat peg × FX fill.
func (s *Server) fillFixedPegPrice(ctx context.Context, row *AssetDetail, memo map[string]*string) {
	if len(s.FiatPeggedClassics) == 0 || (row.PriceUSD != nil && !row.ThinMarket) {
		return
	}
	fiat, ok := s.FiatPeggedClassics[row.AssetID]
	if !ok {
		return
	}
	price, hit := memo[fiat.Code]
	if !hit {
		price = s.declaredPegUSDPrice(ctx, fiat)
		if memo != nil {
			memo[fiat.Code] = price
		}
	}
	if price == nil {
		return
	}
	p := *price
	row.PriceUSD = &p
	row.PriceBasis = priceBasisDeclaredPeg
	row.PriceWithheldReason = ""
	row.ThinMarket = false
}

// declaredPegUSDPrice resolves 1 unit of the peg currency in USD, nil without a fresh rate. Reuses [Server.fiatUSDPriceFor] so
// resolvers cannot drift; the price is the rate string verbatim (declared 1:1), so no float arithmetic touches money.
func (s *Server) declaredPegUSDPrice(ctx context.Context, fiat canonical.Asset) *string {
	if fiat.Code == "USD" {
		one := "1.00000000000000"
		return &one
	}
	price, asOf, _, ok := s.fiatUSDPriceFor(ctx, fiat.Code)
	if !ok || time.Since(asOf) > declaredPegFXMaxAge {
		return nil
	}
	return &price
}

// assetRowSourceCounts maps asset_id to the distinct-venue count behind each listing price, for the dust guard. Rows with a nil
// count are omitted; a missing entry is unmeasured and never suppresses.
func assetRowSourceCounts(rows []timescale.AssetRow) map[string]int {
	m := make(map[string]int, len(rows))
	for _, row := range rows {
		if row.SourceCount != nil {
			m[row.AssetID] = *row.SourceCount
		}
	}
	return m
}

// classicSupplyTTL is how long the broad trustline-derived supply map is reused; the GROUP BY is ~0.5s and totals move slowly.
const classicSupplyTTL = 10 * time.Minute

// classicSupplyMaxAge bounds the broad map's age while refresh keeps failing; it matches classicLakeSupplyTTL because
// higherClassicSupply max()es the two, and a floor only holds against a reading of comparable vintage.
const classicSupplyMaxAge = classicLakeSupplyTTL

// classicSupplyRetryGap rate-limits refresh attempts after a failure: the backing query is a
// full-table GROUP BY, and retrying it per request turned one slow query into a sustained outage.
const classicSupplyRetryGap = 60 * time.Second

// classicSupplyRefreshBudget is the detached refresh's own deadline. It blocks no request, so it
// can exceed the API request timeout, which a request-scoped refresh never could.
const classicSupplyRefreshBudget = 3 * time.Minute

// classicSupplyColdWait bounds a request's wait when nothing is cached; short because the backing query outlives the request
// timeout.
const classicSupplyColdWait = 2 * time.Second

// classicSupplyReader is the narrow capability cachedClassicSupply needs from the explorer reader, shared by the request path
// and the detached refresh.
type classicSupplyReader interface {
	ClassicCirculatingSupply(context.Context) (map[string]string, error)
}

// cachedClassicSupply returns the classic circulating-supply map (CODE-ISSUER to raw 7dp total), cached with a TTL and
// single-flight since the ClickHouse GROUP BY is too heavy per request. Nil without a capable explorer reader (callers fall
// back to precise supply); serves the last good map on refresh errors until classicSupplyMaxAge.
func (s *Server) cachedClassicSupply(ctx context.Context) map[string]string {
	er, ok := s.Explorer.(classicSupplyReader)
	if !ok {
		return nil
	}
	s.classicSupplyMu.Lock()
	if s.classicSupplyCache != nil && time.Since(s.classicSupplyAt) < classicSupplyTTL {
		m := s.classicSupplyCache
		s.classicSupplyMu.Unlock()
		return m
	}
	// Stale or never filled: start a detached refresh (one in flight, one attempt per
	// classicSupplyRetryGap) and serve the last-good map without blocking.
	last := s.servableClassicSupplyLocked()
	flight := s.classicSupplyFlight
	if flight == nil && time.Since(s.classicSupplyAttemptAt) >= classicSupplyRetryGap {
		s.classicSupplyAttemptAt = time.Now() // advances on failure too
		flight = make(chan struct{})
		s.classicSupplyFlight = flight
		// Detaching from the request context is intentional (G118): a refresh on the caller's ctx
		// dies at the request timeout without advancing classicSupplyAt, so every later request
		// would retry the doomed query. The goroutine has its own budget in refreshClassicSupply.
		go s.refreshClassicSupply(er, flight) //nolint:gosec,contextcheck // G118 + contextcheck: the detachment is intentional; see refreshClassicSupply's doc comment.
	}
	s.classicSupplyMu.Unlock()

	// Serving stale beats blocking: this map only feeds market-cap enrichment,
	// and a 10-minute-old supply figure is far better than a 15s stall.
	if last != nil {
		return last
	}
	// Nothing servable: wait briefly so a fast query still fills this response, never for the whole
	// request budget, since the query can outrun the 15s timeout. The detached refresh keeps
	// running so the next request hits cache; PrewarmClassicSupply normally fills it first.
	if flight == nil {
		return nil // retry gap still open and nothing cached
	}
	timer := time.NewTimer(classicSupplyColdWait)
	defer timer.Stop()
	select {
	case <-flight:
		s.classicSupplyMu.Lock()
		m := s.servableClassicSupplyLocked()
		s.classicSupplyMu.Unlock()
		return m
	case <-timer.C:
		return nil // degrade now; the detached refresh fills the cache for the next request
	case <-ctx.Done():
		return nil
	}
}

// servableClassicSupplyLocked returns the cached broad map, or nil once it is
// classicSupplyMaxAge old. Caller holds classicSupplyMu.
func (s *Server) servableClassicSupplyLocked() map[string]string {
	if s.classicSupplyCache == nil || time.Since(s.classicSupplyAt) >= classicSupplyMaxAge {
		return nil
	}
	return s.classicSupplyCache
}

// PrewarmClassicSupply fills the classic supply cache out of band; concurrency-safe via the request path's single-flight, and a warm cache is a no-op.
func (s *Server) PrewarmClassicSupply(ctx context.Context) {
	s.cachedClassicSupply(ctx)
}

// refreshClassicSupply recomputes the map on a detached context. Load-bearing: a GROUP BY outgrowing the 15s request timeout
// never completes on the request context, and classicSupplyAt advances only on success, so every request would re-run the
// doomed query. classicSupplyAttemptAt rate-limits retries.
func (s *Server) refreshClassicSupply(er classicSupplyReader, done chan struct{}) {
	// Deferred so a panic in the GROUP BY cannot skip clearing the flight: a flight left set is
	// never replaced (refreshes start only when it is nil), which would freeze the map for the
	// life of the API and silently drop market_cap from every asset.
	defer s.endClassicSupplyFlight(done)
	defer worker.Recover(s.logger, "api-classic-supply-refresh")
	ctx, cancel := context.WithTimeout(context.Background(), classicSupplyRefreshBudget)
	defer cancel()

	m, err := er.ClassicCirculatingSupply(ctx)

	s.classicSupplyMu.Lock()
	if err == nil && len(m) > 0 {
		s.classicSupplyCache = m
		s.classicSupplyAt = time.Now()
	}
	s.classicSupplyMu.Unlock()
	if err != nil {
		s.logger.Warn("classic supply refresh failed (serving last good)", "err", err)
	}
}

// endClassicSupplyFlight releases the single-flight marker and wakes cold-start waiters; deferred so it runs on EVERY exit,
// panics included.
func (s *Server) endClassicSupplyFlight(done chan struct{}) {
	s.classicSupplyMu.Lock()
	s.classicSupplyFlight = nil
	s.classicSupplyMu.Unlock()
	close(done)
}

// dustLiquiditySuppressed reports whether a cap or FDV backed by sourceCount venues and volume24hUSD is dust liquidity: a
// single venue AND sub-floor volume (the AND is load-bearing); floor <= 0 disables. Unmeasured inputs never suppress:
// sourceCount == 0, or a nil, unparseable or non-positive volume (the reader reports a bogus "0" for pure-Soroban SEP-41).
func dustLiquiditySuppressed(sourceCount int, volume24hUSD *string, floor float64) bool {
	// sourceCount <= 1 proceeds: 0 is unmeasured, but a positive measured sub-floor volume is
	// itself evidence of dust, and impersonator assets live in the unmeasured-count arm. Native
	// is carved out by both callers, not here.
	if floor <= 0 || sourceCount > 1 {
		return false
	}
	if volume24hUSD == nil {
		return false
	}
	vol, ok := new(big.Float).SetPrec(128).SetString(strings.TrimSpace(*volume24hUSD))
	if !ok {
		return false
	}
	if vol.Sign() <= 0 {
		// Non-positive volume is unmeasured (bogus "0" artifact / bad data),
		// not a real sub-floor reading — mirror the nil / unparseable branch.
		return false
	}
	return vol.Cmp(big.NewFloat(floor)) < 0
}

// capExceedsObservedTurnover is the ceiling to [dustLiquiditySuppressed]'s floor: a cap so large against the asset's own
// trailing-24h volume that no market valued anything close. The ratio is days to turn over (default 50,000, ~137 years).
// Unmeasured inputs never suppress: a missing volume is a data gap.
func capExceedsObservedTurnover(capUSD string, volume24hUSD *string, maxRatio float64) bool {
	if maxRatio <= 0 || volume24hUSD == nil {
		return false
	}
	vol, ok := new(big.Float).SetPrec(128).SetString(strings.TrimSpace(*volume24hUSD))
	if !ok || vol.Sign() <= 0 {
		return false
	}
	claim, ok := new(big.Float).SetPrec(128).SetString(strings.TrimSpace(capUSD))
	if !ok || claim.Sign() <= 0 {
		return false
	}
	// Multiplied rather than divided: big.Float has no exact reciprocal.
	return claim.Cmp(new(big.Float).SetPrec(128).Mul(big.NewFloat(maxRatio), vol)) > 0
}

// computeMarketCapUSD returns (circulating / 10^decimals) x priceUSD as a 2-dp decimal string, "" on unparseable input or a
// negative result; big.Float keeps huge supplies exact (ADR-0003). Zero renders "0.00" to agree with [usdMarketValue]; dust
// suppression is the caller's job.
func computeMarketCapUSD(circRaw, priceRaw string, decimals int) string {
	circ, ok := new(big.Float).SetPrec(128).SetString(circRaw)
	if !ok {
		return ""
	}
	price, ok := new(big.Float).SetPrec(128).SetString(priceRaw)
	if !ok {
		return ""
	}
	scale := new(big.Float).SetPrec(128).SetInt(
		new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil),
	)
	mc := new(big.Float).SetPrec(128).Quo(circ, scale)
	mc.Mul(mc, price)
	if mc.Sign() < 0 {
		return ""
	}
	return mc.Text('f', 2)
}

// sep1ImagesTTL bounds when a refresh is kicked, not how long the map is served: the last good map is served while it runs.
// Payloads move only on the sep1-refresh cron (hours).
const sep1ImagesTTL = 10 * time.Minute

// sep1ImagesRetryGap rate-limits refresh attempts: a failed refresh leaves sep1ImagesAt un-advanced, so without a gap every
// request would restart the whole scan and turn one slow query into an outage.
const sep1ImagesRetryGap = 60 * time.Second

// sep1ImagesBudget is the detached refresh's own deadline; it exceeds the request timeout and the measured scan cost so a slow
// scan finishes rather than being killed and repeated.
const sep1ImagesBudget = 2 * time.Minute

// sep1ImageKey is the logo map's exact join key. Asset codes are
// case-sensitive on the network, so the code keeps its case.
func sep1ImageKey(code, issuer string) string {
	return strings.TrimSpace(code) + "-" + issuer
}

// sep1ImageFoldKey keys the case-insensitive fallback entries; the "fold:" prefix cannot collide with an exact key since codes
// are alphanumeric.
func sep1ImageFoldKey(code, issuer string) string {
	return "fold:" + strings.ToUpper(strings.TrimSpace(code)) + "-" + issuer
}

// sep1ImageFor looks a logo up by exact code first, then through the
// fallback entry [buildSep1ImageMap] writes only for unambiguous folds.
func sep1ImageFor(images map[string]string, code, issuer string) string {
	if img, ok := images[sep1ImageKey(code, issuer)]; ok {
		return img
	}
	return images[sep1ImageFoldKey(code, issuer)]
}

// buildSep1ImageMap keys each safe logo by exact (code, issuer) and, where exactly one code spelling folds to it, by a
// case-insensitive fallback too, so a mis-cased stellar.toml reaches its asset but an issuer's USDX and usdx never share a
// logo.
func buildSep1ImageMap(imgs []timescale.Sep1Image) map[string]string {
	built := make(map[string]string, len(imgs))
	spellings := map[string][]string{}
	for _, img := range imgs {
		if !isSafeImageURL(img.Image) {
			continue
		}
		k := sep1ImageKey(img.Code, img.Issuer)
		if _, dup := built[k]; !dup {
			fk := sep1ImageFoldKey(img.Code, img.Issuer)
			spellings[fk] = append(spellings[fk], k)
		}
		built[k] = img.Image
	}
	for fk, keys := range spellings {
		if len(keys) == 1 {
			built[fk] = built[keys[0]]
		}
	}
	return built
}

// matchAssetCode returns the index of the entry naming the case-sensitive code want: exact spelling, else the one
// case-insensitive spelling, else -1; two distinct case-insensitive spellings are ambiguous and match none.
func matchAssetCode(want string, codes []string) int {
	fold, ambiguous := -1, false
	for i, c := range codes {
		if c == want {
			return i
		}
		if !strings.EqualFold(c, want) {
			continue
		}
		if fold == -1 {
			fold = i
		} else if codes[fold] != c {
			ambiguous = true
		}
	}
	if ambiguous {
		return -1
	}
	return fold
}

// sep1ImagesReader is the narrow capability the logo overlay needs, type-asserted off [Server.sep1Cache] (not added to
// [Sep1CachedReader]) so existing stubs keep compiling and opt out. Production wires *timescale.Store.
type sep1ImagesReader interface {
	AllSep1Images(ctx context.Context) ([]timescale.Sep1Image, error)
}

// Compile-time proof that the production reader satisfies the seam: an optional seam that stops
// matching is a silent opt-out, here every listing row losing its SEP-1 logo with nothing red.
// Only the bare store is listed because that is what the binary wires; a caching wrapper placed in
// front of this seam must be added here too.
var _ sep1ImagesReader = (*timescale.Store)(nil)

// cachedSep1Images returns the SEP-1 logo map ([buildSep1ImageMap]) from every verified issuer's cached payload,
// stale-while-revalidate: past [sep1ImagesTTL] a detached refresh runs behind the cached map, since an inline one would block a
// request for the whole scan. Failures degrade to "no logo"; nil without an AllSep1Images reader.
func (s *Server) cachedSep1Images(ctx context.Context) map[string]string {
	reader, ok := s.Sep1Cache.(sep1ImagesReader)
	if !ok {
		return nil
	}
	// contextcheck: readSep1Images deliberately does NOT take the caller's
	// ctx — it may launch the detached refresh, which must outlive this
	// request. ctx is unused on purpose; see this function's doc.
	_ = ctx
	m, _ := s.readSep1Images(reader) //nolint:contextcheck // the detachment is the point; see this function's doc.
	return m
}

// readSep1Images serves what the cache holds and kicks a detached refresh when stale or absent and no attempt is running or too
// recent. Returns the map (possibly nil or stale) and the in-flight completion channel; only [Server.PrewarmSep1Images] may
// wait on it.
func (s *Server) readSep1Images(reader sep1ImagesReader) (map[string]string, chan struct{}) {
	s.sep1ImagesMu.Lock()
	defer s.sep1ImagesMu.Unlock()

	served := s.sep1ImagesCache // last good map; nil only before the first success
	if s.sep1ImagesCache != nil && time.Since(s.sep1ImagesAt) < sep1ImagesTTL {
		return served, nil
	}
	if s.sep1ImagesFlight != nil {
		return served, s.sep1ImagesFlight
	}
	if time.Since(s.sep1ImagesAttemptAt) < sep1ImagesRetryGap {
		return served, nil
	}
	s.sep1ImagesAttemptAt = time.Now() // advances on failure too
	flight := make(chan struct{})
	s.sep1ImagesFlight = flight
	// G118 is the intended behaviour, not a defect: detaching from the
	// request context is what stops a scan being killed at the request
	// deadline and restarted, unbounded, by the next caller.
	go s.refreshSep1Images(reader, flight) //nolint:gosec,contextcheck // G118 + contextcheck: the detachment is deliberate — see cachedSep1Images' doc.
	return served, flight
}

// refreshSep1Images rebuilds the logo map on a DETACHED context and swaps it in; on error the previous map is left as is, so a
// failed refresh never blanks logos that were rendering.
func (s *Server) refreshSep1Images(reader sep1ImagesReader, done chan struct{}) {
	// Deferred so a panic in the scan cannot leave the single-flight marker
	// set — which would freeze the cache for the life of the process, since
	// a non-nil flight is never replaced.
	defer s.endSep1ImagesFlight(done)
	defer worker.Recover(s.logger, "api-sep1-images-refresh")

	ctx, cancel := context.WithTimeout(context.Background(), sep1ImagesBudget)
	defer cancel()

	imgs, err := reader.AllSep1Images(ctx)
	if err != nil {
		// Not an error path for the caller: the previous map keeps being
		// served, and sep1ImagesAttemptAt (already advanced) is what stops
		// this becoming a retry storm.
		s.logger.Warn("sep1 image refresh failed (serving the last good logo map)", "err", err)
		return
	}

	built := buildSep1ImageMap(imgs)

	s.sep1ImagesMu.Lock()
	s.sep1ImagesCache = built
	s.sep1ImagesAt = time.Now()
	s.sep1ImagesMu.Unlock()
}

// endSep1ImagesFlight releases the single-flight marker and wakes the prewarm's waiter; deferred so it runs on EVERY exit,
// panics included.
func (s *Server) endSep1ImagesFlight(done chan struct{}) {
	s.sep1ImagesMu.Lock()
	s.sep1ImagesFlight = nil
	s.sep1ImagesMu.Unlock()
	close(done)
}

// PrewarmSep1Images fills the SEP-1 logo map out of band using the handler's cache slot; best-effort, a failed scan leaves the cache as it was.
func (s *Server) PrewarmSep1Images(ctx context.Context) {
	reader, ok := s.Sep1Cache.(sep1ImagesReader)
	if !ok {
		return
	}
	_, flight := s.readSep1Images(reader) //nolint:contextcheck // same detachment as the request path; ctx is honoured by the wait below, not by the scan.
	if flight == nil {
		return // already warm, or an attempt is gapped out — either way, nothing to wait for
	}
	// Waiting is what makes this a prewarm rather than a nudge: the caller's
	// next pass should find the entry warm, not find its own kick still
	// running. Shutdown returns at once and still lets the detached refresh
	// finish and record what it learned.
	select {
	case <-flight:
	case <-ctx.Done():
	}
}

// fillImagesFromSep1 overlays the SEP-1 [[CURRENCIES]] logo URL onto rows lacking one, from the once-per-TTL logo map (the hot
// listing query deliberately does not JOIN sep1_payload). Best-effort: no verified image or no reader is a no-op.
func (s *Server) fillImagesFromSep1(ctx context.Context, rows []AssetDetail) {
	images := s.cachedSep1Images(ctx)
	if len(images) == 0 {
		return
	}
	for i := range rows {
		if rows[i].Image != nil || rows[i].Issuer == nil || rows[i].Code == "" {
			continue
		}
		if img := sep1ImageFor(images, rows[i].Code, *rows[i].Issuer); img != "" {
			v := img
			rows[i].Image = &v
		}
	}
}

// assetDetailFromAssetRow projects a storage AssetRow onto the AssetDetail wire shape for listing rows, mirroring the scalar
// population in applyAssetRowToDetail.
func assetDetailFromAssetRow(row timescale.AssetRow) AssetDetail {
	asset, err := canonical.ParseAsset(row.AssetID)
	d := AssetDetail{
		Kind:       "stellar_asset",
		AssetID:    row.AssetID,
		Code:       row.Code,
		Decimals:   7,
		Sep1Status: "not_applicable",
	}
	if err == nil {
		d.Type = string(asset.Type)
		if c, ok := asset.ContractAddress(); ok {
			d.ContractID = &c
		}
	}
	if row.IssuerGStrkey != "" {
		v := row.IssuerGStrkey
		d.Issuer = &v
	}
	// Slug + activity metadata.
	if row.Slug != "" {
		d.Slug = row.Slug
	}
	// Ledger 0 doesn't exist on Stellar, so a zero there is "unset" and stays omitted.
	// observation_count is a NOT NULL COUNT, so zero is a real reading and is served as 0
	// (see applyAssetRowToDetail; native XLM is the one row with no count).
	if row.FirstSeenLedger != 0 {
		v := row.FirstSeenLedger
		d.FirstSeenLedger = &v
	}
	if row.LastSeenLedger != 0 {
		v := row.LastSeenLedger
		d.LastSeenLedger = &v
	}
	d.ObservationCount = observationCountWire(row)
	// A LISTING row's price is copied verbatim on purpose: asset_price_snapshot's writer already
	// applies the nonstandard-decimals correction at full NUMERIC precision, so scaling it here
	// would apply the factor twice. The per-asset reader's row (GetAssetByAssetID) is RAW, and a
	// caller publishing its price must normalise it first ([Server.normalizeCatalogueReadUSD]).
	if row.PriceUSD != nil {
		d.PriceUSD = row.PriceUSD
	}
	if row.Volume24hUSD != nil {
		d.VolumeUSD24h = row.Volume24hUSD
		d.VolumeLowerBound = row.VolumeLowerBound
	}
	// The listing carries only the derived volume_character label (from the asset_volume_character
	// rollup); full signals stay detail-only. Display only: never alters the raw VolumeUSD24h.
	if row.VolumeCharacter != nil {
		d.VolumeCharacter = *row.VolumeCharacter
	}
	if row.MarketCapUSD != nil {
		d.MarketCapUSD = row.MarketCapUSD
	}
	if row.CirculatingSupply != nil {
		d.CirculatingSupply = row.CirculatingSupply
	}
	if row.Change1hPct != nil {
		d.Change1hPct = row.Change1hPct
	}
	if row.Change24hPct != nil {
		d.Change24hPct = row.Change24hPct
	}
	if row.Change7dPct != nil {
		d.Change7dPct = row.Change7dPct
	}
	if reason := scamReason(row.IssuerGStrkey); reason != "" {
		d.IssuerScamReason = reason
	}
	return d
}

// normaliseAssetClass folds surface labels into catalogue classes ("blockchain"/"cryptocurrency" alias "crypto").
// validAssetClass reports whether a normalised class names a listing this handler dispatches to ("" is the default listing);
// keep it beside the normaliser, since an alias folding to a value missing here would 400.
func validAssetClass(normalised string) bool {
	switch normalised {
	case "", "all", "fiat", "stablecoin", "crypto":
		return true
	default:
		return false
	}
}

func normaliseAssetClass(raw string) string {
	c := strings.ToLower(strings.TrimSpace(raw))
	switch c {
	case "blockchain", "cryptocurrency", "cryptocurrencies":
		return "crypto"
	}
	return c
}

// handleAssetListFromCatalogue serves /v1/assets?asset_class={fiat,stablecoin,crypto} from the verified-currency catalogue
// (cross-chain identities, not per-issuer rows). Rows have asset_id = slug (routed to GlobalAssetView) and type = "global",
// with no issuer/contract_id/home_domain/sep1_status. `filters` only names ignored row filters in flags.filters_ignored.
func (s *Server) handleAssetListFromCatalogue(
	w http.ResponseWriter, r *http.Request,
	class string, filters assetListFilters, limit int, cursor string,
) {
	flags := Flags{FiltersIgnored: filters.ignored()}
	if s.VerifiedCurrencies == nil {
		writeJSON(w, []AssetDetail{}, flags)
		return
	}
	// StellarIssued (not Browseable): /v1/assets is Stellar-only, so
	// asset_class=fiat yields nothing here (fiat lives on
	// /v1/external/assets); stablecoin/crypto yield only Stellar-issued rows.
	matched := filterCatalogueByClass(s.VerifiedCurrencies.StellarIssued(), currency.AssetClass(class))
	caps := s.computeCatalogueMarketCaps(r.Context(), matched, class)
	rows := s.projectCatalogueRows(r.Context(), matched, caps)
	s.writeCataloguePage(w, r, rows, limit, cursor, flags)
}

// handleExternalAssetList serves GET /v1/external/assets: non-Stellar assets (fiat and reference-only coins), optional
// ?asset_class=; same wire shape and pagination as the catalogue-class listing. market_cap_usd is set for fiat via fxHistory,
// null otherwise.
func (s *Server) handleExternalAssetList(w http.ResponseWriter, r *http.Request) {
	cursor := r.URL.Query().Get("cursor")
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-limit",
				"Invalid limit", http.StatusBadRequest,
				"limit must be an integer in [1, 500]")
			return
		}
		limit = parsed
	}

	// Every external row is priced by an off-chain feed no test net runs.
	if s.VerifiedCurrencies == nil || !s.servesPubnetReference() {
		writeJSON(w, []AssetDetail{}, Flags{})
		return
	}
	entries := s.VerifiedCurrencies.External()
	class := normaliseAssetClass(r.URL.Query().Get("asset_class"))
	if !validAssetClass(class) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-class",
			"Invalid asset_class", http.StatusBadRequest,
			"asset_class must be one of fiat, stablecoin, crypto or all "+
				"(blockchain, cryptocurrency and cryptocurrencies fold to "+
				"crypto), or be omitted for the default listing.")
		return
	}
	if class == "fiat" || class == "stablecoin" || class == "crypto" {
		entries = filterCatalogueByClass(entries, currency.AssetClass(class))
	}
	caps := s.computeAllCatalogueMarketCaps(r.Context(), entries)
	rows := s.projectCatalogueRows(r.Context(), entries, caps)
	// /v1/external/assets reads no row filters at all, so it has none to
	// report as dropped.
	s.writeCataloguePage(w, r, rows, limit, cursor, Flags{})
}

// filterCatalogueByClass returns the catalogue entries of the target class in source order.
func filterCatalogueByClass(entries []*currency.VerifiedCurrency, target currency.AssetClass) []*currency.VerifiedCurrency {
	out := make([]*currency.VerifiedCurrency, 0, len(entries))
	for _, vc := range entries {
		if vc.Class == target {
			out = append(out, vc)
		}
	}
	return out
}

// computeCatalogueMarketCaps returns a parallel-indexed market_cap_usd slice; only fiat rows (M2 x FX rate) carry one, others
// stay empty.
func (s *Server) computeCatalogueMarketCaps(ctx context.Context, matched []*currency.VerifiedCurrency, class string) []string {
	caps := make([]string, len(matched))
	if class != "fiat" {
		return caps
	}
	forEachBounded(s.logger, len(matched), readFanoutConcurrency, func(i int) {
		vc := matched[i]
		if vc.CirculatingSupply == "" {
			return
		}
		if capStr := s.fiatMarketCapUSD(ctx, vc); capStr != nil {
			caps[i] = *capStr
		}
	})
	return caps
}

// projectCatalogueRows projects the matched rows with their market caps and overlays the SEP-1 logo from the cached CODE-ISSUER
// map, so the top-cap rows do not show fallback avatars.
func (s *Server) projectCatalogueRows(ctx context.Context, matched []*currency.VerifiedCurrency, caps []string) []AssetDetail {
	images := s.cachedSep1Images(ctx)
	rows := make([]AssetDetail, len(matched))
	for i, vc := range matched {
		rows[i] = projectCatalogueRow(vc)
		if caps[i] != "" {
			c := caps[i]
			rows[i].MarketCapUSD = &c
		}
		if img := catalogueRowImage(vc, images); img != "" {
			v := img
			rows[i].Image = &v
		}
	}
	return rows
}

// catalogueRowImage resolves vc's SEP-1 logo via its Stellar issuance; "" for fiat/reference-only entries and issuers with no
// verified image.
func catalogueRowImage(vc *currency.VerifiedCurrency, images map[string]string) string {
	if len(images) == 0 {
		return ""
	}
	se := vc.StellarEntry()
	if se == nil || se.Code == "" || se.Issuer == "" {
		return ""
	}
	return sep1ImageFor(images, se.Code, se.Issuer)
}

// writeCataloguePage ranks rows by market cap, paginates and writes the envelope. The cursor names the slugs already served,
// not an offset: caps move between requests, so an offset duplicates or skips rows. Rows are filled BEFORE ranking
// ([Server.fillAndRankCatalogueRows]); `flags` rides on every page.
func (s *Server) writeCataloguePage(
	w http.ResponseWriter, r *http.Request,
	rows []AssetDetail, limit int, cursor string, flags Flags,
) {
	served, ok := parseCatalogueServedCursor(w, r, cursor)
	if !ok {
		return
	}
	rows = dropServedCatalogueRows(rows, served)
	if len(rows) == 0 {
		writeJSON(w, []AssetDetail{}, flags)
		return
	}
	// No filters: the class-scoped listings and /v1/external/assets share
	// this writer and neither applies the row filters (stated on all four
	// parameters in the spec), so no arm has been selected away and the
	// cross-arm total is the only figure a row here can honestly carry.
	if s.fillAndRankCatalogueRows(r.Context(), rows, assetListFilters{}) {
		flags.Stale = true
		flags.Degraded = true
	}
	end := min(limit, len(rows))
	page := rows[:end]
	s.attachSparklinesIfRequested(r, page)
	flags.Degraded = flags.Degraded || anyDirectoryUnchecked(page)
	env := Envelope{Data: page, Flags: flags}
	if end < len(rows) {
		for _, row := range page {
			served = append(served, row.Slug)
		}
		env.Pagination = &Pagination{Next: encodeCatalogueServedCursor(served)}
	}
	writeEnvelope(w, env)
}

// encodeCatalogueServedCursor / parseCatalogueServedCursor: the cursor is base64url of the served slugs as a JSON array, so no
// slug character can read as a separator.
func encodeCatalogueServedCursor(slugs []string) string {
	raw, _ := json.Marshal(slugs) // a []string always marshals
	return base64.RawURLEncoding.EncodeToString(raw)
}

// parseCatalogueServedCursor reports ok=false after writing a problem+json
// for a cursor this writer did not emit; an empty cursor is page 1.
func parseCatalogueServedCursor(w http.ResponseWriter, r *http.Request, cursor string) ([]string, bool) {
	if cursor == "" {
		return nil, true
	}
	var slugs []string
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err == nil {
		err = json.Unmarshal(raw, &slugs)
	}
	if err != nil || len(slugs) == 0 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor must be the pagination.next value from a prior response, or omitted to start at page 1.")
		return nil, false
	}
	return slugs, true
}

// dropServedCatalogueRows removes the rows a prior page served, in place.
func dropServedCatalogueRows(rows []AssetDetail, served []string) []AssetDetail {
	if len(served) == 0 {
		return rows
	}
	seen := make(map[string]struct{}, len(served))
	for _, slug := range served {
		seen[slug] = struct{}{}
	}
	return slices.DeleteFunc(rows, func(row AssetDetail) bool {
		_, ok := seen[row.Slug]
		return ok
	})
}

// fillAndRankCatalogueRows fills each row's served money (own price, then the twin merge) and only then ranks by market cap, so
// order follows published figures; ranking first would rank Stellar-issued rows on nil caps. unmeasured is
// fillCatalogueStatsForPage's.
func (s *Server) fillAndRankCatalogueRows(ctx context.Context, rows []AssetDetail, filters assetListFilters) (unmeasured bool) {
	s.fillCataloguePricesForPage(ctx, rows)
	unmeasured = s.fillCatalogueStatsForPage(ctx, rows, filters)
	sortAssetDetailsByMarketCapDesc(rows)
	return unmeasured
}

// fillCataloguePricesForPage resolves each row's headline USD price through buildGlobalAssetView's fallback chain so listing
// rows match the single-asset view; rows with a price are skipped, and market cap is adopted only when the row has none.
func (s *Server) fillCataloguePricesForPage(ctx context.Context, page []AssetDetail) {
	if s.VerifiedCurrencies == nil {
		return
	}
	priceCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	forEachBounded(s.logger, len(page), readFanoutConcurrency, func(i int) {
		if page[i].PriceUSD != nil {
			return
		}
		vc, ok := s.VerifiedCurrencies.LookupBySlug(page[i].Slug)
		if !ok {
			return
		}
		view := s.catalogueRowPricing(priceCtx, vc)
		page[i].PriceUSD = view.PriceUSD
		page[i].PriceAuthority = view.PriceAuthority
		page[i].PriceSources = view.PriceSources
		// The identity tier stamps the request clock; on a cacheable listing that is noise, not provenance.
		if view.PriceAuthority != aggregate.AuthorityIdentity {
			page[i].PriceAsOf = view.PriceAsOf
		}
		page[i].PriceCredit = view.PriceCredit
		if page[i].MarketCapUSD == nil {
			page[i].MarketCapUSD = view.MarketCapUSD
		}
	})
}

// catalogueRowPricing resolves one entry's headline price, its provenance and market cap via buildGlobalAssetView, falling back
// to the on-chain classic listing price (fillGlobalPriceFromOnChain) for Stellar-only tokens the global tier cannot reach.
func (s *Server) catalogueRowPricing(ctx context.Context, vc *currency.VerifiedCurrency) GlobalAssetView {
	return s.buildGlobalAssetView(ctx, vc)
}

// onChainListingPriceUSD returns the on-chain USD price the /v1/assets listing shows for assetID, nil when absent. It passes
// the same fail-closed substance gate as the classic row, so a dust market cannot price a row /v1/price withholds.
func (s *Server) onChainListingPriceUSD(ctx context.Context, assetID string) *string {
	if s.AssetsReader == nil || assetID == "" {
		return nil
	}
	row, err := s.AssetsReader.GetAssetByAssetID(ctx, assetID)
	if err != nil || row.PriceUSD == nil {
		return nil
	}
	if s.Substance != nil {
		asset, err := canonical.ParseAsset(assetID)
		if err != nil || !s.listingPriceAllowed(ctx, asset) {
			return nil
		}
	}
	// The per-asset reader's price is a RAW prices_1m ratio (it is not
	// the writer-normalised listing rollup, whatever this function's name
	// suggests), so it takes the same correction the detail page gives
	// the same row. Withheld means unpriced, never raw.
	price, ok := s.normalizeCatalogueReadUSDByID(*row.PriceUSD, assetID)
	if !ok {
		return nil
	}
	return &price
}

// assetTypeGlobal is [AssetDetail.Type] on a catalogue-projected row; such rows are keyed by slug, so on-chain reads must
// resolve the Stellar twin first ([Server.seriesAssetIDForRow]).
const assetTypeGlobal = "global"

// projectCatalogueRow maps a catalogue entry to the listing's AssetDetail; no issuer/contract_id/home_domain/sep1_status, which
// belong to the canonical /v1/assets/{asset_id} detail.
func projectCatalogueRow(vc *currency.VerifiedCurrency) AssetDetail {
	name := vc.Name
	d := AssetDetail{
		Kind:       "stellar_asset",
		AssetID:    vc.Slug,
		Type:       assetTypeGlobal,
		Code:       vc.Ticker,
		Decimals:   vc.SupplyDecimals,
		Sep1Status: "not_applicable",
		Class:      string(vc.Class),
		Slug:       vc.Slug,
		Name:       &name,
	}
	if vc.CirculatingSupply != "" {
		s := vc.CirculatingSupply
		d.CirculatingSupply = &s
	}
	return d
}

// sortAssetDetailsByMarketCapDesc sorts in place by market_cap_usd desc, stable; an absent or zero cap ranks last ("0.00" for a
// zero-supply asset is no evidence of size).
func sortAssetDetailsByMarketCapDesc(rows []AssetDetail) {
	sort.SliceStable(rows, func(i, j int) bool {
		ai := rankableMarketCap(rows[i].MarketCapUSD)
		aj := rankableMarketCap(rows[j].MarketCapUSD)
		switch {
		case ai == nil && aj == nil:
			return false
		case ai == nil:
			return false
		case aj == nil:
			return true
		default:
			return ai.Cmp(aj) > 0
		}
	})
}

// rankableMarketCap is the cap sortAssetDetailsByMarketCapDesc ranks on:
// nil when absent, unparseable, or not positive.
func rankableMarketCap(s *string) *big.Float {
	f := bigFloatFromOptionalString(s)
	if f == nil || f.Sign() <= 0 {
		return nil
	}
	return f
}

func bigFloatFromOptionalString(s *string) *big.Float {
	if s == nil || *s == "" {
		return nil
	}
	f, ok := new(big.Float).SetPrec(128).SetString(*s)
	if !ok {
		return nil
	}
	return f
}

// handleAssetListUnified serves /v1/assets with no asset_class or issuer filter: catalogue rows by market_cap_usd desc, then
// classic_assets by 24h volume_usd desc. The cursor is phase-prefixed (empty, "catalogue:<served>", "classic:<inner>",
// "classic:"); a page never spans phases; `filters` narrows both, as the catalogue suppresses classic twins.
func (s *Server) handleAssetListUnified(
	w http.ResponseWriter, r *http.Request,
	filters assetListFilters, limit int, cursor string,
) {
	phase, inner := parseUnifiedCursor(cursor)

	if phase == "catalogue" {
		s.serveCatalogueUnifiedPage(w, r, filters, limit, inner)
		return
	}
	// phase == "classic"
	s.serveClassicUnifiedPage(w, r, filters, limit, inner)
}

// parseUnifiedCursor decodes the phase-prefixed cursor format. An
// empty cursor maps to catalogue phase page 1.
func parseUnifiedCursor(cursor string) (phase, inner string) {
	if cursor == "" {
		return "catalogue", ""
	}
	if rest, ok := strings.CutPrefix(cursor, "catalogue:"); ok {
		return "catalogue", rest
	}
	if rest, ok := strings.CutPrefix(cursor, "classic:"); ok {
		return "classic", rest
	}
	// Legacy cursor format (no phase prefix) — treat as classic-phase
	// to preserve backward compatibility with consumers that round-
	// tripped a pre-unified cursor.
	return "classic", cursor
}

// serveCatalogueUnifiedPage projects the catalogue minus served rows, fills and ranks by market cap, slices to the limit and
// writes the envelope with the next cursor.
func (s *Server) serveCatalogueUnifiedPage(
	w http.ResponseWriter, r *http.Request,
	filters assetListFilters, limit int, innerCursor string,
) {
	if s.VerifiedCurrencies == nil {
		// No catalogue → skip directly to classic phase.
		s.serveClassicUnifiedPage(w, r, filters, limit, "")
		return
	}
	// StellarIssued (not Browseable): the unified listing is Stellar-only; fiat and reference-only
	// coins live on /v1/external/assets.
	//
	// Narrow BEFORE the market-cap fan-out: a filtered-out entry would otherwise cost a supply and
	// FX read for a row that can never be served.
	entries := filterCatalogueEntries(s.VerifiedCurrencies.StellarIssued(), filters)
	caps := s.computeAllCatalogueMarketCaps(r.Context(), entries)
	rows := s.projectCatalogueRows(r.Context(), entries, caps)
	// q= over the catalogue phase (the classic phase filters in SQL). It is applied to the rows,
	// not the entries, because it also matches the projected name.
	rows = filterCatalogueRowsByQuery(rows, filters.q)

	// The same served-slug cursor as writeCataloguePage: the rank below
	// moves between requests, so an offset into it would duplicate or skip.
	served, ok := parseCatalogueServedCursor(w, r, innerCursor)
	if !ok {
		return
	}
	rows = dropServedCatalogueRows(rows, served)
	if len(rows) == 0 {
		// Catalogue done → transition to classic.
		s.serveClassicUnifiedPage(w, r, filters, limit, "")
		return
	}
	// Price + twin-stats merge, then the rank, over every catalogue
	// row BEFORE the slice — writeCataloguePage makes the same call.
	unmeasured := s.fillAndRankCatalogueRows(r.Context(), rows, filters)
	end := min(limit, len(rows))
	page := rows[:end]
	s.attachSparklinesIfRequested(r, page)
	env := Envelope{Data: page, Flags: Flags{Stale: unmeasured, Degraded: unmeasured || anyDirectoryUnchecked(page)}}
	if end < len(rows) {
		for _, row := range page {
			served = append(served, row.Slug)
		}
		env.Pagination = &Pagination{Next: "catalogue:" + encodeCatalogueServedCursor(served)}
		writeEnvelope(w, env)
		return
	}
	// Catalogue exhausted below the limit: fill the rest of the page from the classic stream, or
	// page 1 would return only the short catalogue tail and present it as the whole asset universe.
	if remaining := limit - len(page); remaining > 0 {
		classicRows, nextInner, stale, ok := s.fetchClassicUnifiedRows(w, r, filters, remaining, "")
		if !ok {
			return
		}
		env.Data = append(page, classicRows...)
		env.Flags.Stale = unmeasured || stale
		if nextInner != "" {
			env.Pagination = &Pagination{Next: "classic:" + nextInner}
		} else {
			env.Pagination = nil
		}
		writeEnvelope(w, env)
		return
	}
	// Exact-boundary page: next page picks up classic from the top.
	env.Pagination = &Pagination{Next: "classic:"}
	writeEnvelope(w, env)
}

// serveClassicUnifiedPage serves the classic phase through the AssetsReader path in volume order, prefixing the inner
// next-cursor with the phase.
func (s *Server) serveClassicUnifiedPage(
	w http.ResponseWriter, r *http.Request,
	filters assetListFilters, limit int, innerCursor string,
) {
	out, nextInner, stale, ok := s.fetchClassicUnifiedRows(w, r, filters, limit, innerCursor)
	if !ok {
		return
	}
	// Required-array guard: AssetListEnvelope.data is a required array, so a nil slice would serve
	// `"data": null` on a 200. Every empty phase must reach the wire as [].
	if out == nil {
		out = []AssetDetail{}
	}
	env := Envelope{Data: out, Flags: Flags{Stale: stale, Degraded: stale || anyDirectoryUnchecked(out)}}
	if nextInner != "" {
		env.Pagination = &Pagination{Next: "classic:" + nextInner}
	}
	writeEnvelope(w, env)
}

// fetchClassicUnifiedRows reads one classic-phase page, shared with the page-1 fill. ok=false: a response was already written;
// nextInner empty: exhausted. stale: the substance gate withheld an unmeasured price ([Server.applySubstanceGateToListing]);
// the caller stamps it.
func (s *Server) fetchClassicUnifiedRows(
	w http.ResponseWriter, r *http.Request,
	filters assetListFilters, limit int, innerCursor string,
) ([]AssetDetail, string, bool, bool) {
	// Reject a malformed cursor (not the `<rank_tier>:<vol_or_blank>:<asset_id>` shape emitted
	// below) with 400 rather than let it fall through to a keyset predicate that matches no rows.
	if err := timescale.ValidateAssetsCursor(innerCursor, timescale.AssetsOrderVolume24hUSDDesc); err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest, err.Error())
		return nil, "", false, false
	}
	// A type this spine cannot carry (native / fiat) ends the phase before the reader: the
	// catalogue phase serves those rows. ok=true because an empty classic phase is a legitimate
	// result. The slice is non-nil because it becomes the envelope's required `data` array verbatim.
	if !typeMatchesListingSpine(filters.typ) {
		return []AssetDetail{}, "", false, true
	}
	if s.AssetsReader == nil {
		// No AssetsReader wired → empty terminator.
		writeJSON(w, []AssetDetail{}, Flags{})
		return nil, "", false, false
	}
	opts := timescale.ListAssetsOptions{
		Cursor: innerCursor,
		Order:  timescale.AssetsOrderVolume24hUSDDesc,
		// Without Q the explorer's search box would round-trip to the
		// same page.
		Q: filters.q,
		// Row filters push down to the spine as on the default listing, BEFORE foldAliasTwins, so
		// under `type=classic` a SAC-wrapped asset's row carries its classic-arm volume alone (the
		// documented OpenAPI contract). Fold-then-filter would fetch the unfiltered page and drop
		// rows in Go, an unbounded spine scan for a short page.
		Code:   filters.code,
		Issuer: filters.issuer,
		Type:   filters.typ,
	}
	// Overfetch-by-one (same shape as handleAssetListFromAssets) to
	// drive the cursor advance.
	opts.Limit = limit + AssetsListOverfetchBy
	rows, err := s.AssetsReader.ListAssetsExt(r.Context(), opts)
	if err != nil {
		if clientAborted(r, err) {
			return nil, "", false, false
		}
		s.logger.Error("ListAssetsExt (unified) failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return nil, "", false, false
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	out := make([]AssetDetail, 0, len(rows))
	for _, row := range rows {
		out = append(out, assetDetailFromAssetRow(row))
	}
	// Alias-fold FIRST: collapse a SAC-form row into its canonical classic/native row before
	// catalogue twin suppression, so "USDC shows twice" cannot happen.
	//
	// NOT under type=soroban: the fold drops an alias row whose canonical primary is absent from the
	// page, and that predicate excludes every canonical classic row, so the contract arm would come
	// back empty. Skipping loses nothing: the arm it merges into was filtered out.
	var mergedAliases map[string]struct{}
	if filters.typ != "soroban" {
		out, mergedAliases = s.foldAliasTwins(r.Context(), out, !filters.unfiltered())
	}
	out = s.suppressCatalogueTwins(out)
	// After the catalogue suppression (no read for a row about to be
	// dropped) and before the gate and cap fill that weigh the volume.
	if filters.unfiltered() {
		s.restoreOffPageContractArms(r.Context(), out, mergedAliases)
	}
	s.stampListingCollisions(out)
	unmeasured := s.applySubstanceGateToListing(r.Context(), out, includeThinRequested(r))
	s.fillMarketCapsFromSupply(r.Context(), out, assetRowSourceCounts(rows))
	// AFTER the gate (which would strip it) and the market-cap fill (so no valuation derives from
	// it); see fillDeclaredPegPricesInListing's ordering contract.
	s.fillDeclaredPegPricesInListing(r.Context(), out)
	// Curated issuer label (account_directory), one batch query per page. Scam-class tags withhold
	// the row's price and market cap (suppressScamIssuerPricing).
	//
	// BEFORE the listing-valuation arm: that arm reads the scam tags from a field only this call
	// populates; run first, it would publish a dollar figure for a flagged issuer beneath a nulled
	// price_usd. Every price producer runs above this line, so none can undo the suppression.
	s.fillIssuerDirectoryTags(r.Context(), out)
	settleListingThinRows(out)
	// LAST among the valuation producers: it reads the outcome of every one above it (served cap,
	// gated price, dust flag, peg basis, scam tags) to decide whether a hole needs filling. See
	// asset_listing_valuation.go.
	s.applyListingValuations(r.Context(), out)
	s.fillImagesFromSep1(r.Context(), out)
	s.attachSparklinesIfRequested(r, out)
	nextInner := ""
	// `hasMore` alone, NOT `hasMore && len(out) > 0`: `out` shrinks after the query (twin
	// suppression, alias folding), and gating on it would emit no cursor for a page whose rows all
	// folded away, reading as end-of-pagination with rows unserved. The cursor comes from the RAW
	// last row, which exists whenever hasMore is true.
	if hasMore {
		// Cursor shape <rank_tier>:<sort_vol_or_blank>:<asset_id> encodes exactly what the ORDER BY
		// ranks on (rank tier, then the concentration-adjusted SortVolume24hUSD, not the raw
		// volume), or pagination skips or repeats rows. Built from the RAW last row, before the
		// folds trimmed `out`.
		nextInner = timescale.EncodeAssetsCursor(rows[len(rows)-1], timescale.AssetsOrderVolume24hUSDDesc)
	}
	return out, nextInner, unmeasured, true
}

// computeAllCatalogueMarketCaps returns market_cap_usd parallel to entries; only fiat rows (M2 x FX rate) carry one.
func (s *Server) computeAllCatalogueMarketCaps(ctx context.Context, entries []*currency.VerifiedCurrency) []string {
	caps := make([]string, len(entries))
	forEachBounded(s.logger, len(entries), readFanoutConcurrency, func(i int) {
		vc := entries[i]
		if vc.Class != currency.ClassFiat || vc.CirculatingSupply == "" {
			return
		}
		if capStr := s.fiatMarketCapUSD(ctx, vc); capStr != nil {
			caps[i] = *capStr
		}
	})
	return caps
}

// normaliseAssetIDInput rescues uppercase "NATIVE" (canonical is lowercase, ADR-0010). Compound forms stay case-significant:
// classic codes are case-distinct per issuer, C-strkeys are uppercase-only, and `fiat:USD` rejects lowercase.
func normaliseAssetIDInput(raw string) string {
	if strings.EqualFold(raw, "native") {
		return "native"
	}
	return raw
}

// handleAssetGet serves GET /v1/assets/{asset_id}. Verified-currency slugs resolve first (to [handleGlobalAsset]); canonical
// ids have anchored prefixes, so only "xlm" collides: /v1/assets/XLM serves GlobalAssetView, /v1/assets/native serves
// AssetDetail, and normalizeXLMAliases never runs for "XLM".
func (s *Server) handleAssetGet(w http.ResponseWriter, r *http.Request) {
	rawID := normaliseAssetIDInput(r.PathValue("asset_id"))

	if s.tryServeGlobalAsset(w, r, rawID) {
		return
	}

	parsed, ok := s.parseAssetGetID(w, r, rawID)
	if !ok {
		return
	}

	// XLM dual-form: native, crypto:XLM and the XLM SAC are the same asset; normalise them to
	// native so the whole AssetDetail is consistent (normalizeXLMAliases).
	parsed = normalizeXLMAliases(parsed)

	// Configured classic<->SAC wrappers: a SAC-form id resolves to its canonical classic identity
	// from the config-fed AliasRegistry (no lake round-trip), so /v1/assets/{sac} serves the same
	// detail as /v1/assets/{classic}. An undeclared SAC falls through to the lake-backed
	// resolveSACToClassic below.
	parsed = canonical.CanonicalAsset(parsed)

	// A C-address that is a Stellar Asset Contract resolves to the classic asset it wraps. The trust
	// anchor is the lake instance's StellarAsset executable (only stellar-core mints it); as a
	// belt-and-braces check the classic asset must re-derive to the queried address.
	if parsed.Type == canonical.AssetSoroban && s.Explorer != nil {
		if classic, ok := s.resolveSACToClassic(r.Context(), parsed.ContractID); ok {
			parsed = classic
		}
	}

	// Response cache, drift-safe because the entry came from this same handler within 30s. It
	// covers the whole F2 chain and the overlays, which cost ~50-200ms each warm and dominate the
	// ~700-900ms uncached latency.
	includeThin := includeThinRequested(r)
	cacheKey := assetDetailCacheKey(parsed, includeThin)
	// The prewarm always re-renders: served a hit, it would refresh nothing and the entry would lapse
	// for up to a prewarm cadence once per TTL.
	if !isPrewarmRequest(r) {
		if entry, ok := s.assetDetailCache.get(cacheKey); ok {
			writeCachedAssetDetail(w, entry)
			return
		}
	}
	// Every read below runs under the default record, so a withheld
	// headline carries its substance evidence into the opt-in pass.
	ctx, off := WithThinAdmission(r.Context(), parsed, defaultPriceQuote, false)

	detail, served := s.resolveAssetDetail(w, r.WithContext(ctx), parsed)
	if served {
		return
	}

	// ADR-0042: stamp the wire-shape discriminator here, BEFORE renderAssetDetailEnvelope caches the
	// bytes. resolveAssetDetail's two branches do not agree on setting Kind, and setting it in only
	// one would make the response cache replay Kind for some asset_ids only. This is the single
	// point both branches funnel through.
	detail.Kind = "stellar_asset"

	// A static lookup on the asset's own issuer: the curated scam warning
	// must not depend on any overlay's DB read succeeding.
	stampIssuerScamReason(&detail, parsed)

	// Real-decimals overlay for Soroban tokens (lake instance METADATA). MUST run before
	// applyF2Fields, whose market-cap / FDV math divides by 10^detail.Decimals. Classic and native
	// are always 7 and never consulted.
	s.applyTokenDecimals(ctx, &detail, parsed)

	// Backfill HomeDomain when the storage row lacks one. On-chain account state comes FIRST and the
	// curated known-issuers map is the last resort: the account's own home_domain tracks anchor
	// acquisitions the map misses, and this domain is what the SEP-1 overlay verifies against, so a
	// stale map entry would verify a domain the issuer has left. A degraded read (deadline/error)
	// is folded into the response's stale flag.
	homeDomainDegraded := s.backfillHomeDomain(ctx, &detail)

	// SEP-1 overlay from the payload the `sep1-refresh` cron cached in `issuers.sep1_payload`; never
	// a live HTTPS fetch.
	if s.Sep1Cache != nil {
		s.applySep1Overlay(ctx, &detail, parsed)
	} else if detail.HomeDomain != nil && *detail.HomeDomain != "" && detail.Sep1Status == "" {
		detail.Sep1Status = "not_fetched"
	}

	// F2 overlay (supply / market-cap / FDV), best-effort. The math reads detail.Decimals, the
	// on-chain scale; the SEP-1 overlay surfaces display_decimals separately, so ordering does not
	// affect the units.
	s.applyF2Fields(ctx, &detail, parsed)

	// Asset-catalogue overlay (price, top_markets, history, changes, ATH) so /v1/assets/{id} is a
	// superset of /v1/coins/{slug}. Skipped for fiat:* and when no row or reader exists.
	s.applyAssetExtensionFields(ctx, &detail, parsed)

	// Curated issuer label (account_directory): additive issuer_directory_{tags,domain,name}. Runs
	// AFTER every price/gate overlay; only suppressScamIssuerPricing below reads its answer. A
	// lookup failure omits the fields AND withholds pricing (see asset_directory_tags.go).
	s.applyIssuerDirectoryTags(ctx, &detail)

	// Volume-character overlay: trailing-window account-structure signals and the derived
	// volume_character. Analytics only: it never touches price/gate surfaces or re-ranks.
	// Best-effort.
	s.applyVolumeCharacter(ctx, &detail, parsed)

	// Declared-peg price fill, LAST among the price producers: it fills only when neither the gated
	// F2 lookup (whose reader withholds substanceless markets) nor the catalogue overlay left a
	// market price, and after applyF2Fields so market_cap/fdv never derive from the peg. See
	// fillDeclaredPegPrice. Issuer behaviour runs first because the fill folds it into a depeg
	// warning.
	s.applyIssuerBehaviour(ctx, &detail)
	if includeThin {
		s.thinDetailPricePass(ctx, &detail, parsed, off)
	}
	s.fillDeclaredPegPrice(ctx, &detail, nil)

	// Scam-issuer price suppression, the payload-side twin of the reader-seam ScamGate. Runs AFTER
	// every price producer (including the peg fill) so nothing can resurrect the value: a flagged
	// issuer publishes no price_usd / market_cap_usd / fdv_usd / change_*, keeping circulating_supply
	// and the scam warning. Directory tags are otherwise display-only.
	suppressScamIssuerPricing(&detail)

	// Same rule for every other reason the headline price is absent (thin-market gate, never
	// priced): price_history_* ends on the withheld number. Runs after every producer and
	// suppressor so nothing re-publishes the series.
	withholdPriceSeriesWhenUnpriced(&detail)
	clearThinWhenUnpriced(&detail)

	// Listing-priced valuation, LAST: it reads what every producer and suppressor left to decide
	// whether a hole needs filling. The figure lands in `listing_valuation` under its own
	// provenance and gives the row no market cap.
	//
	// This path's supply comes from asset_supply_history, never the lake's flow totals, so valuing
	// what the handler holds would price only trustline-visible tokens; applyListingValuations
	// does the lake read itself.
	listingRows := []AssetDetail{detail}
	s.applyListingValuations(ctx, listingRows)
	detail = listingRows[0]

	// Verified-currency overlay: attaches `unverified_warning` and flips
	// flags.unverified_ticker_collision when the code matches a verified ticker but the issuer
	// does not.
	flags := s.verifiedCurrencyFlags(&detail, parsed)
	// Output-only summary of the finished payload; nothing upstream reads it.
	detail.Trust = buildAssetTrust(&detail, flags.UnverifiedTickerCollision)
	// Every enrichment is best-effort and just leaves its field null, so a context that died in the
	// chain (client gone, or the RequestTimeout deadline, e.g. the 30s prewarm client outrunning a
	// slow read) yields a numbers-free body with no other signal. Caching it would replay that
	// degraded snapshot as a fresh 200 for the full TTL, so stamp it stale and skip the cache write.
	ctxDead := ctx.Err() != nil
	flags.Stale = flags.Stale || assetDetailStale(&detail, homeDomainDegraded, ctxDead)
	flags.Degraded = flags.Stale || detail.issuerDirectoryUnchecked

	// Render once, cache, write; see [assetDetailResponseCache].
	body, err := renderAssetDetailEnvelope(detail, flags)
	if err != nil {
		// Marshal failure is very rare; fall back to writeJSON rather than a 500.
		s.logger.Debug("asset detail envelope render failed; falling back to direct write", "asset_id", parsed.String(), "err", err)
		writeJSON(w, detail, flags)
		return
	}
	// A failed decimals, issuer-directory or F2 read is a transient outage, and a dead context means
	// the body holds only the reads that landed before cancellation; replaying either for the whole
	// TTL would outlive the outage. Skip the cache write, not the response.
	if !transientlyDegraded(&detail, ctxDead) {
		s.assetDetailCache.put(cacheKey, body, flags.Degraded)
	}
	writeCachedAssetDetail(w, &assetDetailEntry{body: body, cachedAt: time.Now(), degraded: flags.Degraded})
}

// assetDetailStale reports whether detail rests on a failed, cut-short or
// carried-forward read.
func assetDetailStale(detail *AssetDetail, homeDomainDegraded, ctxDead bool) bool {
	return homeDomainDegraded || detail.DecimalsUnresolved || detail.SupplyStale || detail.f2ReadFailed || ctxDead
}

// transientlyDegraded reports whether detail rests on a read that failed or
// was cut short, so the body must not outlive the request in the detail cache.
func transientlyDegraded(detail *AssetDetail, ctxDead bool) bool {
	return detail.DecimalsUnresolved || ctxDead || detail.issuerDirectoryUnchecked || detail.f2ReadFailed
}

// resolveAssetDetail fetches the AssetDetail for parsed from the reader, else a canonical echo.
// served=true means a problem response (404 / 500 / client abort) was already written.
func (s *Server) resolveAssetDetail(w http.ResponseWriter, r *http.Request, parsed canonical.Asset) (AssetDetail, bool) {
	reader := s.assetReaderOrNil()
	if reader == nil {
		// No reader wired: echo a pure-canonical representation.
		return detailFromAsset(parsed), false
	}
	d, err := reader.GetAsset(r.Context(), parsed)
	if errors.Is(err, ErrAssetNotFound) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/asset-not-found",
			"Asset not found", http.StatusNotFound,
			"no trades or oracle observations for "+parsed.String())
		return AssetDetail{}, true
	}
	if err != nil {
		if clientAborted(r, err) {
			return AssetDetail{}, true
		}
		s.logger.Error("GetAsset failed", "err", err, "asset_id", parsed.String())
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return AssetDetail{}, true
	}
	return d, false
}

// applyTokenDecimals overlays a Soroban token's lake decimals() onto detail.Decimals (classic/native are 7) and is the lockstep
// check with `nonstandard_decimals_assets`. A lake/projection disagreement serves the lake value but flags
// MarketCapDecimalsMismatch (cap refused; obs.NonstandardDecimalsLockstepMismatchTotal{site="asset_detail"}); a FAILED lake
// read with no projection row sets DecimalsUnresolved: cap refused, flags.stale uncached.
func (s *Server) applyTokenDecimals(ctx context.Context, detail *AssetDetail, a canonical.Asset) {
	if a.Type != canonical.AssetSoroban || a.ContractID == "" {
		return
	}
	confirmed, hasConfirmed := s.NonstandardDecimals.Lookup(a.ContractID)
	lake, lakeKnown, lakeErr := s.lakeTokenDecimals(ctx, a.ContractID)
	if !lakeKnown {
		if hasConfirmed {
			detail.Decimals = confirmed
		} else if lakeErr != nil {
			detail.DecimalsUnresolved = true
		}
		return
	}
	detail.Decimals = lake
	if (hasConfirmed && confirmed != lake) || (!hasConfirmed && lake != aggregate.StandardDecimals) {
		detail.MarketCapDecimalsMismatch = true
		// asset_detail is request-driven (any contract id a client asks for), so a per-asset label
		// would mint an unbounded Prometheus series per contract; label by site+outcome only and
		// correlate the contract from the WARN line below.
		obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("asset_detail", "").Inc()
		s.logger.Warn("token decimals resolvers disagree; refusing market cap for this request",
			"contract_id", a.ContractID, "lake_decimals", lake,
			"projection_decimals", confirmed, "projection_row", hasConfirmed)
	}
}

// lakeTokenDecimals is the bounded lake read behind applyTokenDecimals. known=false covers every missing-reading case; err is
// non-nil only when the read FAILED (error or tokenMetadataReadTimeout), and callers must then not publish a scale-dependent
// figure.
func (s *Server) lakeTokenDecimals(ctx context.Context, contractID string) (decimals int, known bool, err error) {
	if s.TokenDecimals == nil {
		return 0, false, nil
	}
	dctx, cancel := context.WithTimeout(ctx, tokenMetadataReadTimeout)
	defer cancel()
	d, found, err := s.TokenDecimals.TokenDecimals(dctx, contractID)
	if err != nil {
		s.logger.Warn("token decimals read failed; scale unresolved", "contract_id", contractID, "err", err)
		return 0, false, err
	}
	if !found {
		return 0, false, nil
	}
	return int(d), true, nil
}

// tryServeGlobalAsset serves the global view when raw matches a verified-currency slug or ticker (case-insensitive, slug wins)
// and reports whether it did.
func (s *Server) tryServeGlobalAsset(w http.ResponseWriter, r *http.Request, raw string) bool {
	if s.VerifiedCurrencies == nil {
		return false
	}
	vc := s.lookupCatalogue(raw)
	if vc == nil {
		return false
	}
	// /v1/assets is Stellar-only: an external slug (fiat or reference-only coin) 404s with a
	// pointer to /v1/external/assets/{slug}, not a redirect.
	if vc.StellarEntry() == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/asset-is-external",
			"Asset is external (non-Stellar)", http.StatusNotFound,
			fmt.Sprintf("%q is a non-Stellar asset; its detail lives at /v1/external/assets/%s", raw, vc.Slug))
		return true
	}
	s.handleGlobalAsset(w, r, vc)
	return true
}

// parseAssetGetID resolves the path parameter: canonical parse and Validate, then classic-slug resolution ("usdt-gcqtgzqq"),
// which the explorer links by, so uncached pages would otherwise 404. ok=false means problem+json was written.
func (s *Server) parseAssetGetID(w http.ResponseWriter, r *http.Request, rawID string) (canonical.Asset, bool) {
	parsed, err := canonical.ParseAsset(rawID)
	if err != nil {
		if resolved, ok := s.resolveClassicSlug(r.Context(), rawID); ok {
			return resolved, true
		}
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest,
			"asset_id must match: native | <code>-<G-issuer> | <C-contract> | fiat:<CODE> | a classic asset slug (the full asset_id, any casing)")
		return canonical.Asset{}, false
	}
	if err := parsed.Validate(); err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest,
			err.Error())
		return canonical.Asset{}, false
	}
	return parsed, true
}

// classicSlugResolver is the optional reader capability behind /v1/assets/{slug} classic-slug
// resolution (the proxyPairGate idiom); a reader without it answers 400 on non-canonical ids.
type classicSlugResolver interface {
	ClassicAssetBySlug(ctx context.Context, slug string) (code, issuer string, ok bool, err error)
}

// resolveClassicSlug maps a classic-asset slug to (code, issuer). A lookup error logs and reads as a miss (caller 400s), never
// a 500.
func (s *Server) resolveClassicSlug(ctx context.Context, raw string) (canonical.Asset, bool) {
	// Two reader seams exist (`assets` and `assetsReader`); binaries wire the same adapter behind
	// both but a test may wire only one, so probe both.
	res, ok := s.Assets.(classicSlugResolver)
	if !ok {
		res, ok = s.AssetsReader.(classicSlugResolver)
	}
	if !ok {
		return canonical.Asset{}, false
	}
	code, issuer, found, err := res.ClassicAssetBySlug(ctx, raw)
	if err != nil {
		s.logger.Warn("classic slug lookup failed", "slug", raw, "err", err)
		return canonical.Asset{}, false
	}
	if !found {
		return canonical.Asset{}, false
	}
	asset, err := canonical.NewClassicAsset(code, issuer)
	if err != nil {
		s.logger.Warn("classic slug resolved to unbuildable asset", "slug", raw, "code", code, "err", err)
		return canonical.Asset{}, false
	}
	return asset, true
}

// lookupCatalogue resolves a slug OR ticker to a verified currency (slug wins),
// case-insensitive. Returns nil when neither matches.
func (s *Server) lookupCatalogue(raw string) *currency.VerifiedCurrency {
	if s.VerifiedCurrencies == nil {
		return nil
	}
	if vc, ok := s.VerifiedCurrencies.LookupBySlug(raw); ok {
		return vc
	}
	if vc, ok := s.VerifiedCurrencies.LookupByTicker(raw); ok {
		return vc
	}
	return nil
}

// handleExternalAssetGet serves GET /v1/external/assets/{slug}: detail for a NON-Stellar asset (fiat or reference-only coin); a
// Stellar asset 404s (its detail is on /v1/assets/{slug}).
func (s *Server) handleExternalAssetGet(w http.ResponseWriter, r *http.Request) {
	raw := normaliseAssetIDInput(r.PathValue("slug"))
	if !s.servesPubnetReference() {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/asset-not-found",
			"Asset not found", http.StatusNotFound,
			fmt.Sprintf("external assets are not served on network %s", s.Network))
		return
	}
	vc := s.lookupCatalogue(raw)
	if vc == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/asset-not-found",
			"Asset not found", http.StatusNotFound,
			fmt.Sprintf("no external asset %q in the verified-currency catalogue", raw))
		return
	}
	if vc.StellarEntry() != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/asset-is-stellar",
			"Asset is Stellar", http.StatusNotFound,
			fmt.Sprintf("%q is a Stellar asset; its detail lives at /v1/assets/%s", raw, vc.Slug))
		return
	}
	s.handleGlobalAsset(w, r, vc)
}

// verifiedCurrencyFlags applies the unverified-collision warning to detail and returns the matching envelope flags.
func (s *Server) verifiedCurrencyFlags(detail *AssetDetail, asset canonical.Asset) Flags {
	flags := Flags{}
	if applyUnverifiedWarning(detail, asset, s.VerifiedCurrencies) {
		flags.UnverifiedTickerCollision = true
	}
	applyFiatCodeAnchorNote(detail, asset, s.VerifiedCurrencies)
	return flags
}

// applyFiatCodeAnchorNote stamps detail.FiatCodeAnchor for a classic asset whose code names a catalogue fiat denomination with
// no Stellar issuance (a SEP-1 fiat anchor's deposit token). A neutral note, never an impersonation verdict or cap suppression.
func applyFiatCodeAnchorNote(detail *AssetDetail, asset canonical.Asset, cat *currency.Catalogue) {
	if cat == nil || asset.Type != canonical.AssetClassic {
		return
	}
	denom, knownAnchor, ok := cat.FiatCodeAnchor(asset.Code, asset.Issuer)
	if !ok {
		return
	}
	note := fmt.Sprintf("issuer-declared %s token; verify the anchor", denom.Ticker)
	if knownAnchor {
		note = fmt.Sprintf("%s token issued by a recognised anchor for %s", denom.Ticker, denom.Name)
	}
	detail.FiatCodeAnchor = &FiatCodeAnchor{
		Ticker:      denom.Ticker,
		KnownAnchor: knownAnchor,
		Note:        note,
	}
}

// stampListingCollisions sets UnverifiedTickerCollision on listing rows whose (code, issuer) look-alikes a verified ticker,
// since a NULL-slug impersonator emits the verified CODE as slug and a slug-keyed consumer would badge it verified. No-op
// without a catalogue; type=global rows are skipped.
func (s *Server) stampListingCollisions(rows []AssetDetail) {
	if s.VerifiedCurrencies == nil {
		return
	}
	for i := range rows {
		if rows[i].Issuer == nil || *rows[i].Issuer == "" || rows[i].Code == "" {
			continue
		}
		if _, collision := s.VerifiedCurrencies.StellarCollision(rows[i].Code, *rows[i].Issuer); collision {
			rows[i].UnverifiedTickerCollision = true
		}
	}
}

// applyUnverifiedWarning stamps detail.UnverifiedWarning when the code matches a verified ticker but the issuer does not, and
// reports whether it did. A nil catalogue returns false.
func applyUnverifiedWarning(detail *AssetDetail, asset canonical.Asset, cat *currency.Catalogue) bool {
	if cat == nil {
		return false
	}
	if asset.Type != canonical.AssetClassic {
		return false
	}
	verified, collision := cat.StellarCollision(asset.Code, asset.Issuer)
	if !collision {
		return false
	}
	stellar := verified.StellarEntry()
	if stellar == nil {
		// Reference-only ticker: a well-known EXTERNAL asset (USDT, XRP, BTC) with no verified
		// Stellar issuance. StellarCollision returns true for these so look-alikes are flaggable,
		// and bailing here would drop the warning for the assets impersonators target hardest.
		// VerifiedAssetID stays empty, which consumers must read as "do not redirect; nothing on
		// Stellar is the real one".
		detail.UnverifiedWarning = &UnverifiedWarning{
			VerifiedSlug: verified.Slug,
			VerifiedName: verified.Name,
			Note: fmt.Sprintf(
				"Exercise caution — this asset uses the ticker %q, which matches a well-known asset that has NO verified issuance on Stellar. Any asset trading under this code on Stellar is third-party and is not %s.",
				verified.Ticker, verified.Name,
			),
		}
		return true
	}
	var note string
	if verified.VerifiedIssuerLabel != "" {
		note = fmt.Sprintf(
			"Exercise caution — this asset uses the ticker %q but is not the verified %s on Stellar. The verified %s on Stellar is issued by %s: %s.",
			verified.Ticker, verified.Ticker, verified.Ticker,
			verified.VerifiedIssuerLabel, stellar.AssetID,
		)
	} else {
		note = fmt.Sprintf(
			"Exercise caution — this asset uses the ticker %q but is not the verified %s on Stellar. The verified %s on Stellar is %s.",
			verified.Ticker, verified.Ticker, verified.Ticker,
			stellar.AssetID,
		)
	}
	detail.UnverifiedWarning = &UnverifiedWarning{
		VerifiedSlug:    verified.Slug,
		VerifiedAssetID: stellar.AssetID,
		VerifiedName:    verified.Name,
		VerifiedIssuer:  verified.VerifiedIssuerLabel,
		Note:            note,
	}
	return true
}

// AssetMetadata is the /v1/assets/{id}/metadata wire shape: the SEP-1 overlay slice of AssetDetail without the core canonical fields.
type AssetMetadata struct {
	AssetID         string  `json:"asset_id"`
	HomeDomain      *string `json:"home_domain,omitempty"`
	Sep1Status      string  `json:"sep1_status"`
	Name            *string `json:"name,omitempty"`
	Description     *string `json:"description,omitempty"`
	Image           *string `json:"image,omitempty"`
	OrgName         *string `json:"org_name,omitempty"`
	AnchorAsset     *string `json:"anchor_asset,omitempty"`
	AnchorAssetType *string `json:"anchor_asset_type,omitempty"`
	// SEP-1 issuance declarations from the same overlay as AssetDetail; metadata only, distinct
	// from the live-ledger F2 numbers.
	Conditions  *string `json:"conditions,omitempty"`
	FixedNumber *string `json:"fixed_number,omitempty"`
	MaxNumber   *string `json:"max_number,omitempty"`
	IsUnlimited *bool   `json:"is_unlimited,omitempty"`
	Sep1CurrencyAuthority
}

// Sep1CurrencyAuthority is the issuer's own [[CURRENCIES]] entry about standing and backing; issuer-declared, unverified, set only when sep1_status is "verified".
type Sep1CurrencyAuthority struct {
	// CurrencyStatus is SEP-1 `status` (live, dead, test, private).
	CurrencyStatus *string `json:"currency_status,omitempty"`
	// IsAssetAnchored and Regulated are nil when the issuer did not say,
	// distinct from an explicit false.
	IsAssetAnchored        *bool   `json:"is_asset_anchored,omitempty"`
	AttestationOfReserve   *string `json:"attestation_of_reserve,omitempty"`
	RedemptionInstructions *string `json:"redemption_instructions,omitempty"`
	Regulated              *bool   `json:"regulated,omitempty"`
	ApprovalServer         *string `json:"approval_server,omitempty"`
	ApprovalCriteria       *string `json:"approval_criteria,omitempty"`
}

// handleAssetMetadata serves GET /v1/assets/{asset_id}/metadata (SEP-1 fields only): 404 when not indexed, else 200 with
// sep1_status. Never 503: resolver failure shows in sep1_status, as on /v1/assets/{id}.
func (s *Server) handleAssetMetadata(w http.ResponseWriter, r *http.Request) {
	rawID := normaliseAssetIDInput(r.PathValue("asset_id"))

	parsed, err := canonical.ParseAsset(rawID)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest,
			"asset_id must match: native | <code>-<G-issuer> | <C-contract> | fiat:<CODE>")
		return
	}
	if err := parsed.Validate(); err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest,
			err.Error())
		return
	}

	reader := s.assetReaderOrNil()
	var detail AssetDetail
	if reader != nil {
		d, err := reader.GetAsset(r.Context(), parsed)
		if errors.Is(err, ErrAssetNotFound) {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/asset-not-found",
				"Asset not found", http.StatusNotFound,
				"no trades or oracle observations for "+parsed.String())
			return
		}
		if err != nil {
			if clientAborted(r, err) {
				return
			}
			s.logger.Error("GetAsset failed (metadata)", "err", err, "asset_id", parsed.String())
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/internal",
				"Internal error", http.StatusInternalServerError, "")
			return
		}
		detail = d
	} else {
		detail = detailFromAsset(parsed)
	}

	// Same on-chain-first backfill as handleAssetGet, keeping both surfaces in lockstep on SEP-1
	// status. A degraded read is folded into the stale flag.
	homeDomainDegraded := s.backfillHomeDomain(r.Context(), &detail)

	if s.Sep1Cache != nil {
		s.applySep1Overlay(r.Context(), &detail, parsed)
	} else if detail.HomeDomain != nil && *detail.HomeDomain != "" && detail.Sep1Status == "" {
		detail.Sep1Status = "not_fetched"
	}

	out := AssetMetadata{
		AssetID:         detail.AssetID,
		HomeDomain:      detail.HomeDomain,
		Sep1Status:      detail.Sep1Status,
		Name:            detail.Name,
		Description:     detail.Description,
		Image:           detail.Image,
		OrgName:         detail.OrgName,
		AnchorAsset:     detail.AnchorAsset,
		AnchorAssetType: detail.AnchorAssetType,
		Conditions:      detail.Conditions,
		FixedNumber:     detail.FixedNumber,
		MaxNumber:       detail.MaxNumber,
		IsUnlimited:     detail.IsUnlimited,

		Sep1CurrencyAuthority: detail.Sep1CurrencyAuthority,
	}
	writeJSON(w, out, Flags{Stale: homeDomainDegraded})
}

// sep1StatusForNoPayload separates two opposite findings for an issuer with no SEP-1 payload: `not_fetched` is ours (nothing has run, or
// only runs a systemic outage voided), `unreachable` is theirs (domain reached, nothing storable). Falls back to `not_fetched` without a
// fetch-state reader. applySep1Overlay never fetches live: a live HTTPS fetch per uncached request dominated p95.
func (s *Server) sep1StatusForNoPayload(ctx context.Context, issuer string) string {
	rd, ok := s.Sep1Cache.(Sep1FetchStateReader)
	if !ok {
		return "not_fetched"
	}
	unreachable, err := rd.IssuerSep1Unreachable(ctx, issuer)
	if err != nil {
		s.logger.Warn("sep1 overlay: fetch-state read failed",
			"issuer", issuer, "err", err)
		return "not_fetched"
	}
	if unreachable {
		return "unreachable"
	}
	return "not_fetched"
}

func (s *Server) applySep1Overlay(ctx context.Context, detail *AssetDetail, asset canonical.Asset) {
	// Soroban + native assets have no issuer row to look up. Mark
	// not_applicable so the response is shaped consistently.
	if asset.Type != canonical.AssetClassic || asset.Issuer == "" {
		detail.Sep1Status = "not_applicable"
		return
	}
	sep, err := s.Sep1Cache.GetIssuerSep1Cached(ctx, asset.Issuer)
	if err != nil {
		s.logger.Debug("sep1 cached lookup failed", "asset_id", asset.String(),
			"issuer", asset.Issuer, "err", err)
		// OUR read failed, which says nothing about the issuer.
		detail.Sep1Status = "not_fetched"
		return
	}
	if sep == nil {
		detail.Sep1Status = s.sep1StatusForNoPayload(ctx, asset.Issuer)
		return
	}
	if sep.OutlivedDomain {
		detail.Sep1Status = "unreachable"
		return
	}

	match := findMatchingCachedCurrency(sep, asset)
	if match == nil {
		detail.Sep1Status = "no_match"
		if name := strings.TrimSpace(sep.OrgName); name != "" {
			detail.OrgName = &name
		}
		return
	}

	detail.Sep1Status = "verified"
	applySep1VerifiedFields(detail, sep, match)
}

// applySep1VerifiedFields copies the issuer's own [[CURRENCIES]] entry onto the row, split from
// [Server.applySep1Overlay] so deciding which SEP-1 state applies stays separate from the field
// copy. A field is copied only when the issuer supplied it, so an absent value leaves the row's own
// answer in place instead of overwriting it with an empty string.
//
//nolint:gocyclo // linear field-overlay sequence; splitting would scatter the per-field nil checks across helpers.
func applySep1VerifiedFields(detail *AssetDetail, sep *timescale.IssuerSep1Cached, match *timescale.IssuerSep1Currency) {
	if name := strings.TrimSpace(sep.OrgName); name != "" {
		detail.OrgName = &name
	}
	if v := strings.TrimSpace(match.Name); v != "" {
		detail.Name = &v
	}
	if v := strings.TrimSpace(match.Description); v != "" {
		detail.Description = &v
	}
	if v := strings.TrimSpace(match.Image); isSafeImageURL(v) {
		detail.Image = &v
	}
	if v := strings.TrimSpace(match.AnchorAsset); v != "" {
		detail.AnchorAsset = &v
	}
	if v := strings.TrimSpace(match.AnchorAssetType); v != "" {
		detail.AnchorAssetType = &v
	}
	if v := strings.TrimSpace(match.Conditions); v != "" {
		detail.Conditions = &v
	}
	if v := strings.TrimSpace(match.FixedNumber); v != "" {
		detail.FixedNumber = &v
	}
	if v := strings.TrimSpace(match.MaxNumber); v != "" {
		detail.MaxNumber = &v
	}
	// TOML cannot distinguish an absent bool from an explicit false, so project IsUnlimited only
	// when the issuer addressed supply at all.
	if match.IsUnlimited || match.FixedNumber != "" || match.MaxNumber != "" {
		unlim := match.IsUnlimited
		detail.IsUnlimited = &unlim
	}

	// display_decimals is a UI rounding hint, not a unit scale: it gets its own field and
	// detail.Decimals stays the on-chain scale (7 for classic) so supply and market-cap math hold.
	// The non-standard SEP-1 `decimals` is likewise informational, never the divisor.
	if match.DisplayDecimals > 0 {
		dd := match.DisplayDecimals
		detail.DisplayDecimals = &dd
	} else if match.Decimals > 0 {
		dd := match.Decimals
		detail.DisplayDecimals = &dd
	}
	detail.Sep1CurrencyAuthority = sep1CurrencyAuthorityFrom(match)
}

// sep1CurrencyAuthorityFrom projects the entry's standing and backing declarations; the URL fields are served only if they pass
// the logo's http(s) and attribute-breakout check, since clients link them.
func sep1CurrencyAuthorityFrom(match *timescale.IssuerSep1Currency) Sep1CurrencyAuthority {
	nonEmpty := func(s string) *string {
		if v := strings.TrimSpace(s); v != "" {
			return &v
		}
		return nil
	}
	safeURL := func(s string) *string {
		if v := strings.TrimSpace(s); isSafeImageURL(v) {
			return &v
		}
		return nil
	}
	return Sep1CurrencyAuthority{
		CurrencyStatus:         nonEmpty(match.Status),
		IsAssetAnchored:        match.IsAssetAnchored,
		AttestationOfReserve:   safeURL(match.AttestationOfReserve),
		RedemptionInstructions: nonEmpty(match.RedemptionInstructions),
		Regulated:              match.Regulated,
		ApprovalServer:         safeURL(match.ApprovalServer),
		ApprovalCriteria:       nonEmpty(match.ApprovalCriteria),
	}
}

// findMatchingCachedCurrency returns the cached SEP-1 currency matching the asset: issuer exact, code per [matchAssetCode]; nil
// if none.
func findMatchingCachedCurrency(sep *timescale.IssuerSep1Cached, asset canonical.Asset) *timescale.IssuerSep1Currency {
	if asset.Type != canonical.AssetClassic {
		return nil
	}
	if asset.Code == "" || asset.Issuer == "" {
		return nil
	}
	codes := make([]string, 0, len(sep.Currencies))
	at := make([]int, 0, len(sep.Currencies))
	for i := range sep.Currencies {
		if c := &sep.Currencies[i]; c.Issuer != "" && c.Issuer == asset.Issuer {
			codes = append(codes, c.Code)
			at = append(at, i)
		}
	}
	if j := matchAssetCode(asset.Code, codes); j >= 0 {
		return &sep.Currencies[at[j]]
	}
	return nil
}

// isSafeImageURL reports whether s is a plausible http(s) image URL for `data.image` from a hostile stellar.toml: a scheme allow-list plus
// refusal of attribute-breakout characters. It does NOT validate the HOST (client-side SSRF: the explorer's `isSafePublicImageUrl` in
// web/explorer/src/lib/safe-domain.ts closes it; consumers must replicate) and is no escaping substitute. The image is never fetched.
func isSafeImageURL(s string) bool {
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		switch r {
		case '"', '\'', '<', '>', '`', '\\':
			return true
		}
		return r <= ' ' || r == 0x7f
	})
}

// resolveSACToClassic maps a SAC address to the classic (or native) asset it wraps; the resolved asset's deterministic SAC
// address must equal the queried contract, so spoofed metadata cannot redirect pricing.
func (s *Server) resolveSACToClassic(ctx context.Context, contractID string) (canonical.Asset, bool) {
	asset, ok, _ := s.resolveSAC(ctx, contractID)
	return asset, ok
}

// resolveSAC is [Server.resolveSACToClassic] that also returns the lake read error, so "not a SAC" is told apart from "unknown".
func (s *Server) resolveSAC(ctx context.Context, contractID string) (canonical.Asset, bool, error) {
	name, found, err := s.Explorer.SACClassicAssetName(ctx, contractID)
	if err != nil {
		return canonical.Asset{}, false, err
	}
	if !found {
		return canonical.Asset{}, false, nil
	}
	asset, ok := sacNameVerified(name, contractID)
	return asset, ok, nil
}

// sacNameVerified parses the lake's wrapped-asset name and keeps it only if its derived SAC address is contractID; metadata
// that fails either test is a definite "not this SAC", not a read error.
func sacNameVerified(name, contractID string) (canonical.Asset, bool) {
	asset := canonical.NativeAsset()
	if name != "native" {
		code, issuer, ok := strings.Cut(name, ":")
		if !ok {
			return canonical.Asset{}, false
		}
		var err error
		if asset, err = canonical.NewClassicAsset(code, issuer); err != nil {
			return canonical.Asset{}, false
		}
	}
	derived, err := asset.SacContractID()
	if err != nil || derived != contractID {
		return canonical.Asset{}, false
	}
	return asset, true
}

// sacSearchAsClassic rewrites a q that is a classic asset's SAC address to that asset's id, so the C-form finds the rows its
// asset_id does; the SAC's own listing row carries no code or issuer to match on.
func (s *Server) sacSearchAsClassic(r *http.Request, q string) string {
	if s.Explorer == nil || !canonical.IsContractID(q) {
		return q
	}
	if classic, ok := s.resolveSACToClassic(r.Context(), q); ok && classic.Type == canonical.AssetClassic {
		return classic.String()
	}
	return q
}

// attachSparklinesIfRequested honours ?include=sparkline24h and ?include=sparkline7d, one batch read each; it runs in every phase, so it must be safe on mixed pages.
// (1) Key on the row's Stellar asset id, never the wire asset_id (a catalogue slug matches no prices_1m row and reads as "never traded").
// (2) No published price means no chart: withheld, scam-suppressed and declared-peg rows would republish the number we declined to publish.
func (s *Server) attachSparklinesIfRequested(r *http.Request, rows []AssetDetail) {
	if s.AssetsReader == nil {
		return
	}
	include := r.URL.Query().Get("include")
	if strings.Contains(include, "sparkline24h") {
		s.attachSparkline(r, rows, "sparkline24h", s.AssetsReader.GetAssetsPriceHistory24hBatch,
			func(d *AssetDetail, w []AssetPricePoint) { d.PriceHistory24h = w })
	}
	if strings.Contains(include, "sparkline7d") {
		s.attachSparkline(r, rows, "sparkline7d", s.AssetsReader.GetAssetsPriceHistory7dBatch,
			func(d *AssetDetail, w []AssetPricePoint) { d.PriceHistory7d = w })
	}
}

// attachSparkline fills one listing series; only the 7d series feeds stellarindex_api_sparkline7d_rows_total.
func (s *Server) attachSparkline(r *http.Request, rows []AssetDetail, name string,
	batch func(context.Context, []string) (map[string][]timescale.AssetPricePoint, error),
	set func(*AssetDetail, []AssetPricePoint),
) {
	// Series asset_id → the page rows that render it. A catalogue row and
	// (on a mixed page) its classic twin resolve to the same id, so one
	// lookup can feed several rows.
	rowsByID := make(map[string][]int, len(rows))
	ids := make([]string, 0, len(rows))
	eligible := 0
	for i := range rows {
		if !sparkline7dEligible(&rows[i]) {
			continue
		}
		id := s.seriesAssetIDForRow(&rows[i])
		if id == "" {
			continue
		}
		eligible++
		if _, seen := rowsByID[id]; !seen {
			ids = append(ids, id)
		}
		rowsByID[id] = append(rowsByID[id], i)
	}
	if len(ids) == 0 {
		return
	}
	hist, err := batch(r.Context(), ids)
	if err != nil {
		s.logger.Warn(name+" batch", "err", err)
		return
	}
	served := 0
	for id, idx := range rowsByID {
		points := hist[id]
		if !hasPricedPoint(points) {
			// A map hit alone means nothing: the reader returns a bucket skeleton for every id.
			// Count what the chart can actually draw.
			if name == "sparkline7d" {
				obs.APISparkline7dRowsTotal.WithLabelValues("empty").Add(float64(len(idx)))
			}
			continue
		}
		if name == "sparkline7d" {
			obs.APISparkline7dRowsTotal.WithLabelValues("served").Add(float64(len(idx)))
		}
		served += len(idx)
		// The batch reader hands back RAW prices_1m ratios, unlike the
		// row's price_usd beside it (asset_price_snapshot is normalised
		// by its writer), so the series is corrected here or the chart
		// would sit a power of ten away from the number it illustrates.
		wire := s.sparkline7dToWire(id, points)
		for _, i := range idx {
			set(&rows[i], wire)
		}
	}
	if served == 0 {
		// One line per request, only when every priced row came back without a price point: that
		// is a broken lookup key, not a quiet market. Per-row detail is on the
		// stellarindex_api_sparkline7d_rows_total counter.
		s.logger.Warn(name+": no priced row on this page has a series",
			"requested_ids", len(ids), "priced_rows", eligible)
	}
}

// sparkline7dEligible reports whether a listing row may publish a price series. Both gates run
// before the attach and leave PriceUSD nil, so one predicate covers them and priceless rows.
func sparkline7dEligible(d *AssetDetail) bool {
	return priceSeriesPublishable(d)
}

// sparkline7dToWire projects one 7d series with [Server.normalizeCatalogueReadUSD]'s normalisation; a withheld point becomes a
// null-priced bucket. Never mutates the reader's points (may be a shared cache entry).
func (s *Server) sparkline7dToWire(assetID string, pts []timescale.AssetPricePoint) []AssetPricePoint {
	out := assetPointsToWire(pts)
	for i := range out {
		if out[i].P == nil {
			continue
		}
		if p, ok := s.normalizeCatalogueReadUSDByID(*out[i].P, assetID); ok {
			out[i].P = &p
		} else {
			out[i].P = nil
		}
	}
	return out
}

// normalizeCatalogueReadUSDByID is [Server.normalizeCatalogueReadUSD] for a string id. An unparseable id passes through unless
// it is on record as nonstandard, when it fails closed.
func (s *Server) normalizeCatalogueReadUSDByID(value, assetID string) (string, bool) {
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		if _, flagged := s.NonstandardDecimals.Lookup(assetID); flagged {
			return "", false
		}
		return value, true
	}
	return s.normalizeCatalogueReadUSD(value, asset)
}

// normalizeCatalogueReadUSD normalises a USD price the catalogue SQL emitted as ROUND(x, n)::text from raw ratios. A string with at least
// catalogueUSDRoundDigits + k places (correction 10^k) is already corrected at full precision; a shorter one was rounded on the raw scale
// and keeps the fail-closed floor, so a tiny 18-decimals price is withheld, not published as exactly 10 USD.
func (s *Server) normalizeCatalogueReadUSD(value string, asset canonical.Asset) (string, bool) {
	scaleUp := aggregate.ResolveDecimals(s.NonstandardDecimals, asset) - aggregate.StandardDecimals
	roundedAfter := scaleUp > 0 && decimalFractionPlaces(value) >= catalogueUSDRoundDigits+scaleUp
	return s.normalizeCatalogueUSD(value, asset, !roundedAfter)
}

// decimalFractionPlaces counts fraction places of a plain positional decimal ("0.0250000000" has 10); Postgres NUMERIC text has
// no exponent.
func decimalFractionPlaces(v string) int {
	dot := strings.IndexByte(v, '.')
	if dot < 0 {
		return 0
	}
	return len(v) - dot - 1
}

// priceSeriesPublishable is the one rule for whether a payload may carry a derived price-over-time claim (sparkline,
// price_history_*, all-time high): PriceUSD non-nil and not a declared-peg or global-market fill, whose provenance the Stellar
// market series lacks. Shared with [withholdPriceSeriesWhenUnpriced] so detail cannot draw what the listing declined to.
func priceSeriesPublishable(d *AssetDetail) bool {
	if d == nil {
		return false
	}
	return d.PriceUSD != nil && d.PriceBasis != priceBasisDeclaredPeg && d.PriceBasis != priceBasisGlobalMarket && !d.ThinMarket
}

// seriesAssetIDForRow resolves the asset_id behind a row's price series: a catalogue row's Stellar twin; "" when the entry has
// no Stellar issuance.
func (s *Server) seriesAssetIDForRow(d *AssetDetail) string {
	if d.Type != assetTypeGlobal {
		return d.AssetID
	}
	vc, ok := s.VerifiedCurrencies.LookupBySlug(d.Slug)
	if !ok {
		return ""
	}
	entry := vc.StellarEntry()
	if entry == nil {
		return ""
	}
	return entry.AssetID
}

// hasPricedPoint reports whether a series has a priced bucket; the batch read returns a null-price skeleton for every id asked,
// so map presence is not data.
func hasPricedPoint(pts []timescale.AssetPricePoint) bool {
	for i := range pts {
		if pts[i].P != nil && *pts[i].P != "" {
			return true
		}
	}
	return false
}

// suppressCatalogueTwins drops classic rows whose asset_id belongs to a verified catalogue entry, which absorbs the twin's
// stats (fillCatalogueStatsForPage), so USDC never appears twice.
func (s *Server) suppressCatalogueTwins(rows []AssetDetail) []AssetDetail {
	if s.VerifiedCurrencies == nil {
		return rows
	}
	out := rows[:0]
	for _, row := range rows {
		if _, dup := s.VerifiedCurrencies.LookupByStellarAssetID(row.AssetID); dup {
			continue
		}
		out = append(out, row)
	}
	return out
}

// foldAliasTwins collapses each non-canonical alias row (SAC wrapper, crypto:XLM) into its equivalence class's canonical row, since listing
// SQL joins literal asset_ids. Only 24h volume and trade count aggregate; price, supply and change stay the canonical form's. The merged
// row takes the rank of the class's first row. An alias whose canonical row is off-page is dropped, and
// [Server.restoreOffPageContractArms] adds its volume where that row ranks; keepLone (a filtered page, where that restore does
// not run) serves it as its own row instead. Runs before suppressCatalogueTwins.
func (s *Server) foldAliasTwins(ctx context.Context, rows []AssetDetail, keepLone bool) ([]AssetDetail, map[string]struct{}) {
	canon := s.listingCanonicalIDs(ctx, rows)
	var order []string
	primary := make(map[string]int, len(rows))
	aliases := make(map[string][]int)
	for i, row := range rows {
		id := canon[i]
		_, seenPrimary := primary[id]
		if _, seenAlias := aliases[id]; !seenPrimary && !seenAlias {
			order = append(order, id)
		}
		if id != row.AssetID {
			aliases[id] = append(aliases[id], i)
		} else if !seenPrimary {
			primary[id] = i
		}
	}
	out := make([]AssetDetail, 0, len(order))
	merged := make(map[string]struct{})
	for _, id := range order {
		p, ok := primary[id]
		if !ok {
			if keepLone {
				for _, a := range aliases[id] {
					out = append(out, rows[a])
				}
			}
			continue
		}
		row := rows[p]
		for _, a := range aliases[id] {
			mergeAliasVolume(&row, rows[a])
			merged[rows[a].AssetID] = struct{}{}
		}
		out = append(out, row)
	}
	return out, merged
}

// listingCanonicalIDs maps each row to its class's canonical asset_id: the alias registry's, else, for a contract row, the asset it
// is the SAC of (an on-page classic row's derived address first, then the lake lookup). A failed lookup leaves the row as itself.
func (s *Server) listingCanonicalIDs(ctx context.Context, rows []AssetDetail) []string {
	ids := make([]string, len(rows))
	bySAC := make(map[string]string)
	for i, row := range rows {
		ids[i] = canonicalAssetID(row.AssetID)
		if sac, ok := derivedSAC(row.AssetID); ok {
			bySAC[sac] = row.AssetID
		}
	}
	var lookup []int
	for i, row := range rows {
		if ids[i] != row.AssetID || !canonical.IsContractID(row.AssetID) {
			continue
		}
		if classic, ok := bySAC[row.AssetID]; ok {
			ids[i] = classic
			continue
		}
		lookup = append(lookup, i)
	}
	if len(lookup) == 0 || s.Explorer == nil {
		return ids
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	forEachBounded(s.logger, len(lookup), readFanoutConcurrency, func(j int) {
		i := lookup[j]
		if wrapped, ok := s.sacWrappedAssetID(lookupCtx, rows[i].AssetID); ok {
			ids[i] = wrapped
		}
	})
	return ids
}

// derivedSAC is a classic asset_id's SAC address.
func derivedSAC(assetID string) (string, bool) {
	a, err := canonical.ParseAsset(assetID)
	if err != nil || a.Type != canonical.AssetClassic {
		return "", false
	}
	sac, err := a.SacContractID()
	return sac, err == nil
}

// sacNotWrappedTTL bounds how long a "not a SAC" answer is reused; a contract instance can land in the lake after first sight.
const sacNotWrappedTTL = 10 * time.Minute

// sacWrappedAssetID is [Server.resolveSACToClassic] as an asset_id. A success is memoised for good (the address fixes the
// wrapped asset), a definite "not a SAC" for sacNotWrappedTTL; a read error is not cached.
func (s *Server) sacWrappedAssetID(ctx context.Context, contractID string) (string, bool) {
	if v, ok := s.sacWrapped.Load(contractID); ok {
		return v.(string), true
	}
	if v, ok := s.sacNotWrapped.Load(contractID); ok {
		if time.Now().Before(v.(time.Time)) {
			return "", false
		}
		s.sacNotWrapped.Delete(contractID)
	}
	asset, ok, err := s.resolveSAC(ctx, contractID)
	if !ok {
		if err == nil {
			s.sacNotWrapped.Store(contractID, time.Now().Add(sacNotWrappedTTL))
		}
		return "", false
	}
	id := asset.String()
	s.sacWrapped.Store(contractID, id)
	return id, true
}

// restoreOffPageContractArms adds, onto each classic row of an unfiltered page, the 24h volume of its SAC rows foldAliasTwins
// did not merge in-page, so the dust guard does not weigh an understated figure. Row filters keep the arm they admitted;
// ordering stays per-arm volume.
func (s *Server) restoreOffPageContractArms(ctx context.Context, rows []AssetDetail, merged map[string]struct{}) {
	if s.AssetsReader == nil || len(rows) == 0 {
		return
	}
	arm := s.tradedContractArm(ctx)
	counted := countedSACs(rows, merged)
	readCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	forEachBounded(s.logger, len(rows), readFanoutConcurrency, func(i int) {
		s.mergeContractArmVolumeExcept(readCtx, &rows[i], rows[i].AssetID, merged)
		s.mergeDerivedSACVolume(readCtx, &rows[i], arm, counted)
	})
}

// countedSACs is merged plus every row still served on the page: a SAC the fold could not confirm in time keeps its own row
// and volume, so a later lookup that succeeds must not add that volume to the classic row as well.
func countedSACs(rows []AssetDetail, merged map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(merged)+len(rows))
	for id := range merged {
		out[id] = struct{}{}
	}
	for i := range rows {
		out[rows[i].AssetID] = struct{}{}
	}
	return out
}

// contractArm is the listing's contract arm keyed by asset_id; complete is false when the read failed or hit its row cap.
type contractArm struct {
	rows     map[string]AssetDetail
	complete bool
}

// tradedContractArmLimit is ListAssetsExt's row cap; the arm holds only contracts with a 24h volume row (~60 on pubnet).
const tradedContractArmLimit = 501

// tradedContractArm reads the listing's contract arm, highest volume first.
func (s *Server) tradedContractArm(ctx context.Context) contractArm {
	armCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.AssetsReader.ListAssetsExt(armCtx, timescale.ListAssetsOptions{
		Type:  "soroban",
		Limit: tradedContractArmLimit,
		Order: timescale.AssetsOrderVolume24hUSDDesc,
	})
	if err != nil {
		s.logger.Warn("contract arm read failed; SAC volume served as a lower bound", "err", err)
		return contractArm{}
	}
	arm := contractArm{rows: make(map[string]AssetDetail, len(rows)), complete: len(rows) < tradedContractArmLimit}
	for _, row := range rows {
		arm.rows[row.AssetID] = assetDetailFromAssetRow(row)
	}
	return arm
}

// mergeDerivedSACVolume adds the arm volume of dst's derived SAC unless it was merged in-page or is a registry alias (added by
// mergeContractArmVolumeExcept). It adds only when the lake lookup confirms the SAC, the same test that folds the SAC's own
// row away, so a SAC served as its own row is never also counted here. Volume it cannot read or confirm marks a lower bound.
func (s *Server) mergeDerivedSACVolume(ctx context.Context, dst *AssetDetail, arm contractArm, merged map[string]struct{}) {
	sac, ok := derivedSAC(dst.AssetID)
	if !ok || canonicalAssetID(sac) != sac {
		return
	}
	if _, done := merged[sac]; done {
		return
	}
	row, ok := arm.rows[sac]
	if !ok {
		if !arm.complete {
			dst.VolumeLowerBound = true
		}
		return
	}
	if s.Explorer == nil {
		return
	}
	if wrapped, ok := s.sacWrappedAssetID(ctx, sac); ok && wrapped == dst.AssetID {
		mergeAliasVolume(dst, row)
		return
	}
	if _, cached := s.sacWrapped.Load(sac); !cached {
		dst.VolumeLowerBound = true
	}
}

// canonicalAssetID resolves an asset_id to its class's canonical id via the AliasRegistry; an unparseable id folds onto itself
// rather than being dropped.
func canonicalAssetID(assetID string) string {
	a, err := canonical.ParseAsset(assetID)
	if err != nil {
		return assetID
	}
	return canonical.CanonicalAsset(a).String()
}

// mergeAliasVolume adds an alias row's 24h volume and trade count onto its canonical row; every other field stays the canonical
// form's.
func mergeAliasVolume(dst *AssetDetail, alias AssetDetail) {
	dst.VolumeUSD24h = addDecimalStrings(dst.VolumeUSD24h, alias.VolumeUSD24h)
	dst.VolumeLowerBound = dst.VolumeLowerBound || alias.VolumeLowerBound
	if alias.TradeCount24h != nil {
		sum := *alias.TradeCount24h
		if dst.TradeCount24h != nil {
			sum += *dst.TradeCount24h
		}
		dst.TradeCount24h = &sum
	}
}

// addDecimalStrings sums two optional decimal strings (nil if both absent, the lone operand if one) with exact big.Rat
// arithmetic (ADR-0003), keeping the greater fractional precision, trailing zeros trimmed.
func addDecimalStrings(a, b *string) *string {
	ar := ratFromOptionalString(a)
	br := ratFromOptionalString(b)
	switch {
	case ar == nil && br == nil:
		return nil
	case ar == nil:
		return b
	case br == nil:
		return a
	}
	sum := new(big.Rat).Add(ar, br)
	frac := maxFractionDigits(*a, *b)
	s := sum.FloatString(frac)
	if frac > 0 && strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return &s
}

// ratFromOptionalString parses an optional decimal to a big.Rat; nil, blank or unparseable returns nil so a bad operand never
// corrupts a sum.
func ratFromOptionalString(s *string) *big.Rat {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	r, ok := new(big.Rat).SetString(t)
	if !ok {
		return nil
	}
	return r
}

// maxFractionDigits returns the greater fraction-digit count of two decimal strings, the precision addDecimalStrings formats
// to.
func maxFractionDigits(a, b string) int {
	f := func(s string) int {
		if i := strings.IndexByte(s, '.'); i >= 0 {
			return len(s) - i - 1
		}
		return 0
	}
	na, nb := f(a), f(b)
	if na > nb {
		return na
	}
	return nb
}

// fillCatalogueStatsForPage merges each row's Stellar twin (changes, 24h volume, supply, market cap), bounded and best-effort
// per row. `filters` picks the SAC-wrapped asset's arm as on the spine: unfiltered classic + contract, under a row filter the
// classic arm. unmeasured: the substance gate withheld a twin it could not measure; the caller stamps flags.stale.
func (s *Server) fillCatalogueStatsForPage(ctx context.Context, page []AssetDetail, filters assetListFilters) (unmeasured bool) {
	if s.AssetsReader == nil {
		return false
	}
	statsCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var arm contractArm
	if filters.unfiltered() && len(page) > 0 {
		arm = s.tradedContractArm(statsCtx)
	}
	counted := countedSACs(page, nil)
	var anyUnmeasured atomic.Bool
	forEachBounded(s.logger, len(page), readFanoutConcurrency, func(i int) {
		vc, ok := s.VerifiedCurrencies.LookupBySlug(page[i].Slug)
		if !ok {
			return
		}
		entry := vc.StellarEntry()
		if entry == nil || entry.AssetID == "" {
			return
		}
		// ListAssetsExt, not GetAssetByAssetID: only the listing query computes the windowed
		// change columns. Filter by issuer, not Q: Q substring-matches column values, so a full
		// asset id matches nothing.
		twinRow := s.lookupCatalogueTwin(statsCtx, entry.AssetID)
		if twinRow == nil {
			return
		}
		twin := []AssetDetail{assetDetailFromAssetRow(*twinRow)}
		// Contract arm before the cap fill, as foldAliasTwins does on the spine, so the
		// dust-liquidity guard weighs the same volume on both paths.
		if filters.unfiltered() {
			s.mergeContractArmVolume(statsCtx, &twin[0], entry.AssetID)
			s.mergeDerivedSACVolume(statsCtx, &twin[0], arm, counted)
		}
		// The classic-phase ordering: the twin's price backs the cap and the
		// pills merged below, so a dust price must be withheld before either.
		if s.applySubstanceGateToListing(statsCtx, twin, false) {
			anyUnmeasured.Store(true)
		}
		// Same supply-derived market-cap fill as the classic phase. The twin populates
		// source_count, so the dust-liquidity guard can fire here exactly as for a classic row;
		// mergeTwinStats carries market_cap_low_liquidity onto the catalogue row.
		s.fillMarketCapsFromSupply(statsCtx, twin, assetRowSourceCounts([]timescale.AssetRow{*twinRow}))
		// Only the twin carries the G-issuer the directory is keyed on (fillIssuerDirectoryTags
		// skips a catalogue row). Without this the valuation arm below would publish a valuation
		// for a flagged issuer that the listing spine already refused.
		s.fillIssuerDirectoryTags(statsCtx, twin)
		// Valuations run on the twin: it carries the asset_id the directory addresses derive
		// from, the price the gates ruled on and the dust flag. mergeTwinStats carries them over.
		s.applyListingValuations(statsCtx, twin)
		mergeTwinStats(&page[i], twin[0])
	})
	return anyUnmeasured.Load()
}

// mergeContractArmVolume adds a catalogue entry's SAC wrapper 24h volume and trade count onto its classic twin row, which
// foldAliasTwins/suppressCatalogueTwins would under-report. Classic issuance and SAC forms only: `crypto:XLM` is a disjoint
// off-chain CEX population that must not enter an on-Stellar figure. Only additive counters merge.
func (s *Server) mergeContractArmVolume(ctx context.Context, dst *AssetDetail, assetID string) {
	s.mergeContractArmVolumeExcept(ctx, dst, assetID, nil)
}

// mergeContractArmVolumeExcept is [Server.mergeContractArmVolume] minus
// the wrapper ids in skip, which the caller has already merged.
func (s *Server) mergeContractArmVolumeExcept(ctx context.Context, dst *AssetDetail, assetID string, skip map[string]struct{}) {
	asset, err := canonical.ParseAsset(assetID)
	if err != nil || asset.Type != canonical.AssetClassic {
		return
	}
	for _, alias := range canonical.AssetAliases(asset) {
		if alias.Type != canonical.AssetSoroban {
			continue
		}
		if _, done := skip[alias.String()]; done {
			continue
		}
		row, err := s.AssetsReader.GetAssetByAssetID(ctx, alias.String())
		if err != nil {
			continue
		}
		mergeAliasVolume(dst, assetDetailFromAssetRow(row))
	}
}

// lookupCatalogueTwin resolves a catalogue entry's Stellar asset id to its listing row (native reader for XLM; exact-issuer
// filter for classic, as Q never matches a full id); nil if not served. The row is the classic arm only; unfiltered, the caller
// adds the contract arm ([Server.mergeContractArmVolume]).
func (s *Server) lookupCatalogueTwin(ctx context.Context, assetID string) *timescale.AssetRow {
	if assetID == "native" {
		row, err := s.AssetsReader.GetNativeAssetRow(ctx)
		if err != nil {
			return nil
		}
		return &row
	}
	dashIx := strings.Index(assetID, "-")
	if dashIx < 0 {
		return nil
	}
	rows, err := s.AssetsReader.ListAssetsExt(ctx, timescale.ListAssetsOptions{
		Limit:  50,
		Issuer: assetID[dashIx+1:],
		Order:  timescale.AssetsOrderVolume24hUSDDesc,
	})
	if err != nil {
		return nil
	}
	for j := range rows {
		if rows[j].AssetID == assetID {
			return &rows[j]
		}
	}
	return nil
}

// mergeTwinStats fills a catalogue row's nil analytics from its
// Stellar-network twin without overwriting anything already set.
func mergeTwinStats(dst *AssetDetail, twin AssetDetail) {
	if dst.Change1hPct == nil {
		dst.Change1hPct = twin.Change1hPct
	}
	if dst.Change24hPct == nil {
		dst.Change24hPct = twin.Change24hPct
	}
	if dst.Change7dPct == nil {
		dst.Change7dPct = twin.Change7dPct
	}
	if dst.VolumeUSD24h == nil {
		dst.VolumeUSD24h = twin.VolumeUSD24h
		dst.VolumeLowerBound = twin.VolumeLowerBound
	}
	if dst.CirculatingSupply == nil {
		dst.CirculatingSupply = twin.CirculatingSupply
		// Carry the twin's scale with its supply. circulating_supply is a raw integer in the
		// smallest unit paired with `decimals`, while a catalogue row's own Decimals is 0 for
		// Stellar-issued entries. A stroop value on a decimals=0 row would render 10^7 too large.
		dst.Decimals = twin.Decimals
	}
	if dst.MarketCapUSD == nil {
		dst.MarketCapUSD = twin.MarketCapUSD
	}
	// The dust-liquidity flag must ride along with the (absent) cap, or the catalogue row would
	// show nothing where the twin shows market_cap_low_liquidity. When suppressed, null cap and
	// FDV too so a stale value never coexists with the flag, as in populateMarketCap.
	if twin.MarketCapLowLiquidity {
		dst.MarketCapLowLiquidity = true
		dst.MarketCapUSD = nil
		dst.FDVUSD = nil
	}
	// The listing valuation ran on the twin, which carries the asset_id, price and gate outcome it
	// reads. Carry the pair unconditionally: the catalogue row computes none of its own, and a
	// reference without its valuation would be a price with no statement of what it was used for.
	dst.ListingReference = twin.ListingReference
	dst.ListingValuation = twin.ListingValuation
	carryTwinIssuerVerdict(dst, twin)
}

// carryTwinIssuerVerdict moves the twin's issuer verdict (scam reason, directory labels) onto the catalogue row and re-applies price
// suppression. Not redundant with the twin's own: the global tier fills price first and mergeTwinStats' fill-only-if-nil leaves it standing,
// so a flagged issuer would serve a price while the classic row carries null. The G-address is not copied (rows stay issuer-less).
func carryTwinIssuerVerdict(dst *AssetDetail, twin AssetDetail) {
	if twin.IssuerScamReason != "" {
		dst.IssuerScamReason = twin.IssuerScamReason
	}
	if len(twin.IssuerDirectoryTags) > 0 {
		dst.IssuerDirectoryTags = twin.IssuerDirectoryTags
		dst.IssuerDirectoryDomain = twin.IssuerDirectoryDomain
		dst.IssuerDirectoryName = twin.IssuerDirectoryName
	}
	if twin.issuerDirectoryUnchecked {
		dst.issuerDirectoryUnchecked = true
	}
	suppressScamIssuerPricing(dst)
}

// filterCatalogueEntries applies the type/code/issuer filters to each entry's Stellar issuance, not the projected row (type
// "global", no issuer), else USDC would drop from `type=classic`. Native XLM has no on-chain code, so `code=` never selects it.
func filterCatalogueEntries(entries []*currency.VerifiedCurrency, f assetListFilters) []*currency.VerifiedCurrency {
	if f.typ == "" && f.code == "" && f.issuer == "" {
		return entries
	}
	out := make([]*currency.VerifiedCurrency, 0, len(entries))
	for _, vc := range entries {
		if stellarIssuanceMatches(vc.StellarEntry(), f) {
			out = append(out, vc)
		}
	}
	return out
}

// stellarIssuanceMatches reports whether one entry's Stellar issuance satisfies the filters; a nil issuance matches nothing.
func stellarIssuanceMatches(se *currency.IssuanceEntry, f assetListFilters) bool {
	if se == nil {
		return false
	}
	if f.code != "" && se.Code != f.code {
		return false
	}
	if f.issuer != "" && se.Issuer != f.issuer {
		return false
	}
	if f.typ == "" {
		return true
	}
	// The entry's asset_id is the canonical identity ("native",
	// "CODE-ISSUER", a contract id), so its parsed type IS the
	// structural class the filter names.
	asset, err := canonical.ParseAsset(se.AssetID)
	return err == nil && string(asset.Type) == f.typ
}

// filterCatalogueRowsByQuery applies the case-insensitive q= substring
// filter over in-process catalogue rows (code / asset id / slug / name).
func filterCatalogueRowsByQuery(rows []AssetDetail, rawQ string) []AssetDetail {
	q := strings.ToLower(strings.TrimSpace(rawQ))
	if q == "" {
		return rows
	}
	filtered := rows[:0]
	for _, row := range rows {
		hay := strings.ToLower(row.AssetID + " " + row.Code + " " + row.Slug)
		if row.Name != nil {
			hay += " " + strings.ToLower(*row.Name)
		}
		if strings.Contains(hay, q) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

// normalizeXLMAliases maps XLM's non-native forms (crypto:XLM, the XLM SAC) to native so supply, volume, market_cap, the
// dust-guard carve-out and cache key agree: supply.AssetKey errors on crypto:XLM and volume keys on trades.base_asset
// ("native"), so it would serve a price with NULL supply and volume.
func normalizeXLMAliases(a canonical.Asset) canonical.Asset {
	if a.Equal(canonical.NativeAsset()) {
		return a
	}
	for _, alias := range canonical.AssetAliases(canonical.NativeAsset()) {
		if a.Equal(alias) {
			return canonical.NativeAsset()
		}
	}
	return a
}
