package v1

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/currency"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// Sep1CachedReader is the narrow dependency /v1/assets/{id} uses to
// apply the SEP-1 overlay. Returns the parsed payload the
// `stellarindex-ops sep1-refresh` cron persisted in `issuers.sep1_payload`.
//
// Pre-2026-05-29 the assets handler resolved SEP-1 live via HTTPS
// (capped at 500ms via [sep1OverlayTimeout]) on every uncached
// request. That call dominated /v1/assets/{id} p95 (4+s on cold
// issuers) and triggered the slo_latency_burn alerts. The handler
// now uses the cron-populated DB column instead; the live-fetch
// path lives only in `cmd/stellarindex-ops/sep1_refresh.go`.
type Sep1CachedReader interface {
	GetIssuerSep1Cached(ctx context.Context, gStrkey string) (*timescale.IssuerSep1Cached, error)
}

// AssetReader is the storage-side interface for asset reads.
// Implementations:
//   - *timescale.Store (queries trades hypertable's distinct assets).
//   - in-memory stubs for tests.
type AssetReader interface {
	// GetAsset returns the canonical representation for the given
	// asset-id. Returns ErrAssetNotFound when the asset isn't yet
	// indexed (i.e. no trades observed for it).
	GetAsset(ctx context.Context, id canonical.Asset) (AssetDetail, error)

	// ListAssets returns a page of indexed assets. cursor = ""
	// starts at the beginning; limit clamped to [1, 500].
	ListAssets(ctx context.Context, cursor string, limit int) ([]AssetDetail, string, error)
}

// ErrAssetNotFound is what AssetReader.GetAsset returns for an
// unknown asset. Handlers translate it to HTTP 404 + problem+json.
var ErrAssetNotFound = errors.New("api: asset not found")

// TokenDecimalsReader resolves a Soroban token contract's on-chain
// `decimals()` from its captured contract-instance metadata (the token-sdk
// METADATA convention in the ClickHouse lake). found=false when the
// instance isn't captured or the contract stores no standard metadata —
// the caller then keeps the default. Production wiring is
// *clickhouse.ExplorerReader.
type TokenDecimalsReader interface {
	TokenDecimals(ctx context.Context, contractID string) (uint32, bool, error)
}

// AssetDetail is the payload for /v1/assets responses. Matches the
// shape in docs/reference/api-design.md §5.2.
type AssetDetail struct {
	AssetID    string  `json:"asset_id"`
	Type       string  `json:"type"`
	Code       string  `json:"code,omitempty"`
	Issuer     *string `json:"issuer,omitempty"`
	ContractID *string `json:"contract_id,omitempty"`
	HomeDomain *string `json:"home_domain,omitempty"`
	// Decimals is the on-chain smallest-unit scale (7 for classic /
	// native stroops; per-contract for SEP-41). It is the divisor for
	// EVERY unit computation — supply display AND market-cap / FDV math
	// (MarketCapUSD = circulating × price / 10^Decimals). It MUST NOT be
	// set from issuer-declared SEP-1 `display_decimals`, which is a
	// wallet rounding hint, not a unit scale (F-1321: doing so inflated
	// market_cap_usd by up to 10^(7-display_decimals)× on verified
	// classic assets and was an issuer-controlled manipulation vector).
	// Surface the display hint via DisplayDecimals instead.
	Decimals int `json:"decimals"`
	// DisplayDecimals is the issuer's SEP-1 `display_decimals` rounding
	// hint (e.g. USDC declares 2) — a UI preference for how many
	// fractional digits to show, NOT a unit scale. Omitted when the
	// SEP-1 overlay didn't declare one. Never feeds amount math.
	DisplayDecimals *int `json:"display_decimals,omitempty"`

	// Class is the cross-chain asset class for catalogue-backed rows:
	// "fiat" | "stablecoin" | "crypto". Omitted for Stellar-classic
	// rows that have no catalogue entry (the long tail of asset codes
	// observed in trades). Populated from `internal/currency`'s
	// verified-currency catalogue via the asset's slug. R-018
	// assets-unification — lets /v1/assets serve as the CMC/CoinGecko-
	// style global listing with class-filtered views (`?asset_class=fiat`).
	Class string `json:"class,omitempty"`
	// Sep1Status is the state of the SEP-1 overlay for this asset:
	//   - "not_applicable" — no home-domain (native, fiat, SAC-only).
	//   - "not_fetched"    — has a home-domain but overlay not configured.
	//   - "verified"       — SEP-1 fetched + matching [[CURRENCIES]] entry found.
	//   - "no_match"       — SEP-1 fetched but no matching issuer+code entry.
	//   - "unreachable"    — fetch / parse failed (see server logs).
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

	// CirculatingSupply / TotalSupply / MaxSupply are decimal
	// strings in the asset's smallest integer unit (stroops for
	// XLM / classic; contract-defined for SEP-41). Consumers divide
	// by 10^decimals for display. Null when no supply snapshot
	// exists for this asset (untracked, or supply orchestrator
	// hasn't run yet).
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

	// SupplyBasis identifies which ADR-0011 policy produced the
	// supply numbers; null when no snapshot exists. Lets consumers
	// decide how much to trust the absolute value (e.g. `override`
	// indicates an operator curated the locked-set or SEP-1
	// declared a max_supply).
	SupplyBasis *string `json:"supply_basis,omitempty"`

	// VolumeUSD24h is the trailing-24h USD-denominated trade
	// volume across every pair this asset participates in (as base
	// OR quote). Sourced from the prices_1m CAGG. Per Freighter V2
	// scope ("24h Trading Volume aggregate across indexed
	// markets").
	//
	// String-typed for the same reason as the supply / market-cap
	// fields: NUMERIC sums don't fit a fixed-width Go type cleanly.
	// "0" is a valid value (asset tracked, no trades in 24h);
	// null means "volume reader not wired" or "lookup failed" —
	// callers presenting the field should distinguish these.
	VolumeUSD24h *string `json:"volume_24h_usd,omitempty"`

	// Change24hPct is the trailing-24h price change as a signed
	// percentage with two fractional digits (e.g. "+1.27", "-0.05",
	// "0.00"). Computed as `(now_usd - then_usd) / then_usd * 100`
	// where `then_usd` anchors to the latest closed prices_1m bucket
	// whose end is at or before now-24h.
	//
	// Null when (a) no current USD price exists for the asset (no
	// indexed pair, or pair is fiat:USD itself), or (b) no closed
	// bucket exists in the 24h-ago window (asset first traded < 24h
	// ago, or trade history was retention-pruned). Surfacing the
	// distinction is intentional — clients render "—" rather than
	// fabricating "0%".
	Change24hPct *string `json:"change_24h_pct,omitempty"`

	// ─── Coin-equivalence extension (R-018 final) ────────────────
	//
	// Fields lifted from CoinSummary so /v1/assets/{id} is a
	// superset of /v1/coins/{slug}. Lets the explorer migrate every
	// /v1/coins consumer to /v1/assets without losing data. Populated
	// only when a CoinsReader is wired AND the asset has a row in
	// the coins catalogue (skipped for fiat:* and external:* assets).

	// PriceUSD is the latest VWAP/last-trade USD price as a
	// fixed-precision decimal string. Inlined so wallet UIs
	// (Freighter, retail apps) don't pay a second
	// `/v1/price?asset=…&quote=fiat:USD` round-trip on every
	// asset-detail render (F-1271 audit-2026-05-12). Populated by:
	//
	//   1. Coins overlay when a CoinsReader is wired AND the asset
	//      is in the coins catalogue (richer enrichment path —
	//      delegates to CoinSummary.PriceUSD).
	//   2. Direct USD-price lookup in populateMarketCap when the
	//      overlay didn't populate it (covers fiat:* / external:*
	//      / SEP-41 not in the coins catalogue / any asset whose
	//      market_cap path already paid for the price lookup).
	//
	// Null only when no USD price can be derived at all (no
	// on-chain trades, prices_1m has no row, and stablecoin-fiat
	// proxy is disabled).
	PriceUSD *string `json:"price_usd,omitempty"`

	// Change1hPct / Change7dPct round out the trailing-window set
	// alongside Change24hPct. Same shape — signed percentage with
	// two fractional digits, null when no past-bucket snapshot
	// exists within the window's tolerance.
	Change1hPct *string `json:"change_1h_pct,omitempty"`
	Change7dPct *string `json:"change_7d_pct,omitempty"`

	// TopMarkets is a preview of the asset's top markets by 24h USD
	// volume. Up to 5 entries; null when the coin reader doesn't
	// return any.
	TopMarkets []CoinTopMarket `json:"top_markets,omitempty"`

	// PriceHistory24h / PriceHistory7d are sparkline-grade USD-price
	// timeseries (24 hourly + 7 daily samples respectively).
	PriceHistory24h []CoinPricePoint `json:"price_history_24h,omitempty"`
	PriceHistory7d  []CoinPricePoint `json:"price_history_7d,omitempty"`

	// MarketsCount / TradeCount24h are the trailing-24h activity
	// counters mirrored from CoinSummary. Pointers so 0 ("silent
	// 24h") is distinguishable from "not computed" (no reader / lookup
	// error) in alerting.
	MarketsCount  *int64 `json:"markets_count,omitempty"`
	TradeCount24h *int64 `json:"trade_count_24h,omitempty"`

	// ATH is the asset's all-time-high USD price + the day it was
	// set. Sourced from prices_1d, USD-quotes only.
	ATH *CoinATH `json:"ath,omitempty"`

	// IssuerScamReason is non-empty when this asset's issuer appears
	// in the curated scam directory. Clients should render a
	// prominent warning when present.
	IssuerScamReason string `json:"issuer_scam_reason,omitempty"`

	// Slug is the friendly short identifier for the asset (e.g.
	// "USDC" for the canonical Circle USDC, or the issuer-
	// disambiguated form like "USDC-GA5Z…" for collisions). Mirror
	// of CoinSummary.Slug; lets consumers build canonical /assets/
	// URLs without a parallel /v1/coins lookup.
	Slug string `json:"slug,omitempty"`

	// FirstSeenLedger / LastSeenLedger / ObservationCount are the
	// trades-hypertable activity metadata. Mirrored from CoinRow so
	// the explorer's asset-detail page can drop its parallel
	// /v1/coins/{slug} fetch.
	FirstSeenLedger  *uint32 `json:"first_seen_ledger,omitempty"`
	LastSeenLedger   *uint32 `json:"last_seen_ledger,omitempty"`
	ObservationCount *int64  `json:"observation_count,omitempty"`

	// ─── SEP-1 issuance declarations ───────────────────────────
	//
	// Drawn directly from the issuer's stellar.toml [[CURRENCIES]]
	// entry; populated only when Sep1Status == "verified". These
	// are issuer-declared, not derived from the ledger — distinct
	// from the F2 fields above which observe live ledger state.

	// Conditions is the issuer's declared terms / conditions for
	// the asset (SEP-1 `conditions`). Free-form text.
	Conditions *string `json:"conditions,omitempty"`

	// FixedNumber is the SEP-1-declared fixed total supply, if the
	// issuer has committed to one. Decimal string (NUMERIC-safe per
	// ADR-0003); the asset's smallest integer unit. Distinct from
	// `total_supply` above: that's the live-ledger sum, this is
	// what the issuer publicly committed to.
	FixedNumber *string `json:"fixed_number,omitempty"`

	// MaxNumber is the SEP-1-declared maximum supply, if the
	// issuer has set a cap. Decimal string. Distinct from
	// `max_supply` above: that's the operator/policy-derived cap
	// (XLM hard cap, operator override, future-PR overlay), this
	// is the issuer's own self-declared ceiling.
	MaxNumber *string `json:"max_number,omitempty"`

	// IsUnlimited is the issuer's SEP-1 declaration that they reserve
	// the right to issue an unbounded amount. When true, FixedNumber
	// and MaxNumber are typically both empty. Pointer so the
	// "issuer didn't say" case is distinguishable from "issuer said
	// false".
	IsUnlimited *bool `json:"is_unlimited,omitempty"`

	// UnverifiedWarning points at the verified Stellar-canonical
	// asset when the requested asset uses a verified currency's
	// ticker code but is NOT the verified issuer (R-018 ticker
	// collision). Populated by handleAssetGet via the verified-
	// currency catalogue; nil for the verified asset itself and
	// for any code not claimed by a verified currency on Stellar.
	// Pairs with Flags.UnverifiedTickerCollision for client-side
	// detection.
	UnverifiedWarning *UnverifiedWarning `json:"unverified_warning,omitempty"`

	// UnverifiedTickerCollision is the per-row trust signal on the
	// /v1/assets LISTING: true when this row's (code, issuer) uses a
	// verified currency's Stellar ticker but is NOT the verified
	// issuer — i.e. a look-alike/impersonator. The listing serves
	// COALESCE(slug, code) AS slug, so an impersonator with a NULL
	// slug emits the VERIFIED asset's CODE as its slug; a consumer
	// keyed only on slug∈verified-set would then badge the
	// impersonator "verified". Consumers must AND the verified-slug
	// check with `!unverified_ticker_collision` (the real verified
	// row carries it false; look-alikes carry it true). The detail
	// path (/v1/assets/{id}) stamps the richer UnverifiedWarning body
	// + Flags.UnverifiedTickerCollision instead; this bool is the
	// lightweight listing-row equivalent. Omitted (false) for the
	// verified asset and for codes no verified currency claims on
	// Stellar.
	UnverifiedTickerCollision bool `json:"unverified_ticker_collision,omitempty"`
}

// UnverifiedWarning is the body attached to /v1/assets/{id} when
// the requested asset code-collides with a verified Stellar
// currency but the issuer doesn't match. Designed to be lifted
// directly into UI: every field is human-render-ready, and the
// `note` sentence is verbatim-safe to render in a warning banner.
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

// detailFromAsset populates an AssetDetail from the canonical shape.
// Nullable fields are nil-pointered when empty so JSON omits them
// cleanly.
//
// This is the SCAFFOLDING path used when no AssetReader is wired
// (tests, the dev binary before the storage layer is up). The
// production cmd/stellarindex-api path uses its own assetToDetail
// that consults the operator's curated home-domain map; this stub
// version doesn't have access to that map, so HomeDomain stays nil
// and the overlay handler stamps sep1_status="not_applicable" via
// the existing default.
func detailFromAsset(a canonical.Asset) AssetDetail {
	d := AssetDetail{
		AssetID: a.String(),
		Type:    string(a.Type),
		Code:    a.Code,
		// Classic + native are 7 by protocol (stroops) — that is the
		// CORRECT value, not a placeholder. Soroban tokens get their real
		// on-chain decimals() overlaid by the handler (applyTokenDecimals)
		// when the lake has the contract's instance metadata; 7 remains
		// the documented default otherwise.
		Decimals:   7,
		Sep1Status: "not_applicable",
	}
	if a.Issuer != "" {
		v := a.Issuer
		d.Issuer = &v
	}
	if a.ContractID != "" {
		v := a.ContractID
		d.ContractID = &v
	}
	// Classic + native assets carry their SAC address (board #40, RFP
	// audit: both RFPs put "Contract Address" in the classic-asset
	// metadata table). Deterministic derivation — valid even before
	// the SAC is deployed, since deployment is permissionless and
	// address-stable.
	if d.ContractID == nil && (a.Type == canonical.AssetClassic || a.Type == canonical.AssetNative) {
		if sac, err := a.SacContractID(); err == nil {
			d.ContractID = &sac
		}
	}
	return d
}

// ─── Asset reader on the Server ──────────────────────────────────

// assets is the AssetReader registered at server construction.
// May be nil during the /v1/assets scaffolding phase — handlers
// degrade gracefully to "feature unavailable" 503 when unset.
func (s *Server) assetReaderOrNil() AssetReader { return s.assets }

// ─── Handlers ─────────────────────────────────────────────────────

// assetListFilters holds the validated row-narrowing filters shared
// by the /v1/assets listing paths (BACKLOG #54, matching the
// TypeFilter / CodeFilter / IssuerFilter parameters in the OpenAPI
// spec). An empty field means "no filter". typ is normalised so both
// "any" and omitted collapse to "".
type assetListFilters struct {
	typ    string // "" | native | classic | soroban | fiat
	code   string // exact classic code, case-sensitive
	issuer string // G-strkey, CRC-checked
}

// parseAssetListFilters extracts + validates the type / code / issuer
// query filters for /v1/assets. It returns ok=false — after writing a
// problem+json 400 — when any value is malformed, so garbage input
// fails fast regardless of which backing path would serve the request.
// Empty values are valid and disable the corresponding filter.
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

	return f, true
}

// isValidClassicCode reports whether s is a valid Stellar classic
// asset code: 1-12 bytes, [A-Za-z0-9] only (XDR alphanum4/alphanum12).
// Mirrors canonical.validateClassicAssetCode, which is unexported.
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

// handleAssetList serves GET /v1/assets.
//
// Query params:
//   - cursor (optional): opaque, from a prior response's pagination.next.
//   - limit  (optional): integer 1-500, default 100.
//
// Row filters (BACKLOG #54), validated up front (garbage → 400)
// regardless of which backing path serves the request:
//   - type (optional): structural asset class, one of native |
//     classic | soroban | fiat | any. `any`/omitted disables it.
//   - code (optional): exact classic asset code, case-sensitive,
//     1-12 alphanumeric (e.g. `USDC`). Not unique on Stellar —
//     combine with issuer to pin a single asset.
//   - issuer (optional): a G-strkey (CRC-checked).
//
// The filters apply to the default classic-assets listing (the
// CoinsReader path — `code`/`issuer` push down to the indexed
// classic_assets columns; `type` folds against the homogeneously-
// classic backing table). Consistent with how `issuer` already
// scoped, they are NOT re-applied when `asset_class` dispatches to
// the catalogue / unified paths (asset_class is the major dispatch);
// validation still fires on every path so bad input never 200s.
//
// Returns an empty list when no AssetReader is wired (operator did
// not configure the asset-catalog reader). The Envelope shape is
// otherwise correct so clients can integrate against the wire
// contract regardless of whether the catalogue is populated.
func (s *Server) handleAssetList(w http.ResponseWriter, r *http.Request) {
	// Parse + validate query params FIRST — bad input is 400
	// regardless of whether the backing reader is wired.
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

	// Row filters (type / code / issuer). Validated BEFORE the
	// asset_class dispatch so malformed input 400s on every path
	// (BACKLOG #54), not just the coins-backed one.
	filters, ok := parseAssetListFilters(w, r)
	if !ok {
		return
	}

	// asset_class filter. Drives the class chip group on the
	// explorer's /assets page. Recognised values:
	//   - "fiat"        → catalogue fiat rows only (USD, EUR, …).
	//   - "stablecoin"  → catalogue stablecoin rows only (USDC, USDT, …).
	//   - "crypto"      → catalogue crypto rows only (XLM, …).
	//   - "all" / ""    → catalogue rows (all 3 classes, market-cap
	//                   ordered) THEN classic_assets via volume-desc.
	//                   See handleAssetListUnified.
	assetClass := normaliseAssetClass(r.URL.Query().Get("asset_class"))
	if assetClass == "fiat" || assetClass == "stablecoin" || assetClass == "crypto" {
		s.handleAssetListFromCatalogue(w, r, assetClass, limit, cursor)
		return
	}

	// Unified "All" listing — catalogue rows first (market-cap
	// ordered) then classic_assets via the existing volume-desc
	// path. Cursor encodes the phase so the explorer's paginate-
	// forward UX walks both naturally. OPT-IN via asset_class=all
	// so existing SDK consumers integrated against the legacy
	// classic-only listing don't see a wire-shape change. The
	// explorer's /assets page passes asset_class=all explicitly.
	if assetClass == "all" {
		s.handleAssetListUnified(w, r, limit, cursor)
		return
	}

	// Coins-backed listing — when a CoinsReader is wired, source the
	// listing from the same ListCoinsExt path /v1/coins uses. Gives
	// each row the price / volume / change / sparkline / ATH fields
	// (R-018 finish — assets-unification endgame). Falls through to
	// the lean AssetReader path when no CoinsReader is configured.
	if s.coins != nil {
		s.handleAssetListFromCoins(w, r, filters, cursor, limit)
		return
	}

	reader := s.assetReaderOrNil()
	if reader == nil {
		// Feature not wired yet — empty list is consistent with
		// the contract and doesn't force a 503.
		writeJSON(w, []AssetDetail{}, Flags{})
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
	// Defensive: a reader that returns a nil slice on empty would
	// marshal as "data": null, which violates the OpenAPI
	// required-array contract. The production adapter already
	// returns non-nil empty, but we don't want correctness to depend
	// on that for every future reader.
	if rows == nil {
		rows = []AssetDetail{}
	}

	env := Envelope{
		Data:  rows,
		Flags: Flags{},
	}
	if next != "" {
		env.Pagination = &Pagination{Next: next}
	}
	writeEnvelope(w, env)
}

// handleAssetListFromCoins serves /v1/assets when a CoinsReader is
// wired. Sources rows from ListCoinsExt and projects each CoinRow
// into an AssetDetail with the coin-overlay fields populated — same
// shape as /v1/coins listings, just under the /v1/assets URL.
//
// Honors the type / code / issuer row filters (BACKLOG #54):
//   - code + issuer push down to the indexed classic_assets columns
//     via ListCoinsExt.
//   - type folds against the backing table: classic_assets is
//     homogeneously classic, so a structural type filter that
//     excludes classic (native / soroban / fiat) matches nothing and
//     short-circuits to an empty page WITHOUT a DB round-trip.
//     `classic` / `any` / omitted are a no-op. (Native XLM, Soroban,
//     and fiat rows live on the catalogue / unified paths.)
//
// The default order is observation_count_desc; cursor passes through
// unchanged.
func (s *Server) handleAssetListFromCoins(
	w http.ResponseWriter,
	r *http.Request,
	filters assetListFilters,
	cursor string,
	limit int,
) {
	if filters.typ != "" && filters.typ != "classic" {
		writeEnvelope(w, Envelope{Data: []AssetDetail{}, Flags: Flags{}})
		return
	}
	// Overfetch-by-one: request limit+1 so the (limit+1)th row signals
	// a next page. `limit` is validated to [1,500] by the caller, so the
	// store sees at most 501 (F-1326: previously this passed `limit`
	// itself, so len(rows) > limit was never true and /v1/assets never
	// emitted a next cursor — only the first page of ~440K assets was
	// reachable).
	opts := timescale.ListCoinsOptions{
		Limit:  limit + 1,
		Issuer: filters.issuer,
		Code:   filters.code,
		Cursor: cursor,
	}
	rows, err := s.coins.ListCoinsExt(r.Context(), opts)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("ListCoinsExt (assets listing) failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	// Overfetch-by-one for cursor pagination — same shape as
	// handleCoins. The +1th row determines whether there's a next
	// page; it isn't returned to the caller.
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	out := make([]AssetDetail, 0, len(rows))
	for _, row := range rows {
		out = append(out, assetDetailFromCoinRow(row))
	}
	s.stampListingCollisions(out)
	s.fillMarketCapsFromSupply(r.Context(), out)
	s.fillImagesFromSep1(r.Context(), out)
	env := Envelope{Data: out, Flags: Flags{}}
	if hasMore && len(out) > 0 {
		last := rows[len(rows)-1]
		env.Pagination = &Pagination{
			Next: fmt.Sprintf("%d:%s", last.ObservationCount, last.AssetID),
		}
	}
	writeEnvelope(w, env)
}

// fillMarketCapsFromSupply fills market_cap (and circulating_supply) on
// listing rows WHERE the supply pipeline covers the asset and we have a
// USD price. The base listing query deliberately leaves market_cap null
// rather than fabricate (it won't run a per-row supply lookup), so the
// covered (major) assets showed an empty market-cap column (audit
// 2026-06-19). This surfaces it for them; coverage grows as the supply
// pipeline expands. Type-asserted so coins readers without the method
// (test stubs) simply skip — best-effort, never fails the response.
func (s *Server) fillMarketCapsFromSupply(ctx context.Context, rows []AssetDetail) {
	// Precise supply — the three-domain pipeline (supply_1d, ~9 assets).
	// Authoritative (includes claimable + LP-locked holdings); preferred.
	var precise map[string]string
	if sr, ok := s.coins.(interface {
		LatestCirculatingSupply(context.Context) (map[string]string, error)
	}); ok {
		if m, err := sr.LatestCirculatingSupply(ctx); err == nil {
			precise = m
		}
	}
	// Broad-coverage fallback — trustline-balance sums for EVERY classic
	// asset, derived from the ClickHouse lake and cached (audit 2026-06-19
	// item 4: market_cap was null for all but the ~9 precise-supply assets).
	// Slightly undercounts (omits claimable + LP), so it only fills rows the
	// precise pipeline doesn't cover.
	broad := s.cachedClassicSupply(ctx)
	if len(precise) == 0 && len(broad) == 0 {
		return
	}
	for i := range rows {
		if rows[i].MarketCapUSD != nil || rows[i].PriceUSD == nil {
			continue
		}
		circ := precise[rows[i].AssetID]
		if circ == "" {
			circ = broad[rows[i].AssetID]
		}
		if circ == "" {
			continue
		}
		if mc := computeMarketCapUSD(circ, *rows[i].PriceUSD, rows[i].Decimals); mc != "" {
			rows[i].MarketCapUSD = &mc
			if rows[i].CirculatingSupply == nil {
				c := circ
				rows[i].CirculatingSupply = &c
			}
		}
	}
}

// classicSupplyTTL bounds how long the broad trustline-derived supply map
// is reused before a refresh. The underlying GROUP BY is ~0.5s and the
// totals move slowly, so a 10-minute TTL keeps it off the API hot path.
const classicSupplyTTL = 10 * time.Minute

// cachedClassicSupply returns the broad-coverage classic circulating-supply
// map (canonical CODE-ISSUER → raw 7dp total) from the explorer reader,
// cached per-server with a TTL + single-flight. The backing ClickHouse
// GROUP BY is far too heavy to run per request. Returns nil when no
// explorer reader exposing the method is wired (test stubs) — callers then
// degrade to the precise supply set only. Serves the last good map on a
// refresh error.
func (s *Server) cachedClassicSupply(ctx context.Context) map[string]string {
	er, ok := s.explorer.(interface {
		ClassicCirculatingSupply(context.Context) (map[string]string, error)
	})
	if !ok {
		return nil
	}
	s.classicSupplyMu.Lock()
	if s.classicSupplyCache != nil && time.Since(s.classicSupplyAt) < classicSupplyTTL {
		m := s.classicSupplyCache
		s.classicSupplyMu.Unlock()
		return m
	}
	if ch := s.classicSupplyFlight; ch != nil {
		s.classicSupplyMu.Unlock()
		select {
		case <-ch:
			s.classicSupplyMu.Lock()
			m := s.classicSupplyCache
			s.classicSupplyMu.Unlock()
			return m
		case <-ctx.Done():
			return nil
		}
	}
	done := make(chan struct{})
	s.classicSupplyFlight = done
	s.classicSupplyMu.Unlock()

	m, err := er.ClassicCirculatingSupply(ctx)

	s.classicSupplyMu.Lock()
	if err == nil && len(m) > 0 {
		s.classicSupplyCache = m
		s.classicSupplyAt = time.Now()
	} else {
		m = s.classicSupplyCache // serve last good on error/empty
	}
	s.classicSupplyFlight = nil
	s.classicSupplyMu.Unlock()
	close(done)
	if err != nil {
		s.logger.Warn("classic supply refresh failed", "err", err)
	}
	return m
}

// computeMarketCapUSD = (circulating / 10^decimals) × priceUSD, as a
// 2-dp decimal string. circulating is raw integer units (stroops-scale);
// dividing by 10^decimals yields whole-asset units before the price
// multiply. Returns "" on unparseable input. big.Float throughout so a
// 50-billion-unit supply doesn't lose precision (ADR-0003).
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
	if mc.Sign() <= 0 {
		return ""
	}
	return mc.Text('f', 2)
}

// sep1ImagesTTL bounds how long the SEP-1 logo map is reused before a
// refresh. The payloads only move on the sep1-refresh cron cadence
// (hours), so a 10-minute TTL keeps the scan off the API hot path while
// staying fresh enough for a newly-verified issuer's logo to appear.
const sep1ImagesTTL = 10 * time.Minute

// sep1ImageKey is the join key shared by the logo-map build + lookup.
// Asset codes are matched case-insensitively (as the per-asset SEP-1
// overlay does via EqualFold); the issuer G-strkey must match exactly.
func sep1ImageKey(code, issuer string) string {
	return strings.ToUpper(strings.TrimSpace(code)) + "-" + issuer
}

// cachedSep1Images returns the SEP-1 logo map (case-folded CODE-ISSUER →
// safe image URL) built from every verified issuer's cached payload,
// cached per-server with a TTL + single-flight. The backing scan is one
// indexed SELECT over the few-dozen issuers carrying a sep1_payload;
// caching it means the /v1/assets listing image overlay costs a map
// lookup per row, never a per-row JOIN into the hot listing query.
// Returns nil when no reader exposing AllSep1Images is wired (test stubs
// / overlay disabled) — the listing then simply omits images, exactly as
// before. Serves the last good map on a refresh error.
func (s *Server) cachedSep1Images(ctx context.Context) map[string]string {
	reader, ok := s.sep1Cache.(interface {
		AllSep1Images(context.Context) ([]timescale.Sep1Image, error)
	})
	if !ok {
		return nil
	}
	s.sep1ImagesMu.Lock()
	if s.sep1ImagesCache != nil && time.Since(s.sep1ImagesAt) < sep1ImagesTTL {
		m := s.sep1ImagesCache
		s.sep1ImagesMu.Unlock()
		return m
	}
	if ch := s.sep1ImagesFlight; ch != nil {
		s.sep1ImagesMu.Unlock()
		select {
		case <-ch:
			s.sep1ImagesMu.Lock()
			m := s.sep1ImagesCache
			s.sep1ImagesMu.Unlock()
			return m
		case <-ctx.Done():
			return nil
		}
	}
	done := make(chan struct{})
	s.sep1ImagesFlight = done
	s.sep1ImagesMu.Unlock()

	imgs, err := reader.AllSep1Images(ctx)
	var built map[string]string
	if err == nil {
		built = make(map[string]string, len(imgs))
		for _, img := range imgs {
			if !isSafeImageURL(img.Image) {
				continue
			}
			built[sep1ImageKey(img.Code, img.Issuer)] = img.Image
		}
	}

	s.sep1ImagesMu.Lock()
	if err == nil {
		s.sep1ImagesCache = built
		s.sep1ImagesAt = time.Now()
	} else {
		built = s.sep1ImagesCache // serve last good on error
	}
	s.sep1ImagesFlight = nil
	s.sep1ImagesMu.Unlock()
	close(done)
	if err != nil {
		s.logger.Warn("sep1 image refresh failed", "err", err)
	}
	return built
}

// fillImagesFromSep1 overlays the SEP-1 [[CURRENCIES]] logo URL onto
// listing rows that don't already carry one. It runs OFF the hot listing
// query — which deliberately doesn't JOIN the sep1_payload JSONB — by
// reading the once-per-TTL-window logo map: a map lookup per row for the
// returned page (bounded by the page limit, ≤500). Best-effort: a row
// with no verified image is left as-is (the explorer renders a fallback
// avatar), and a missing/failed reader is a no-op. This is why the
// homepage grid used to show fallback avatars even for verified issuers —
// the single-asset path overlaid the image but the listing never did.
func (s *Server) fillImagesFromSep1(ctx context.Context, rows []AssetDetail) {
	images := s.cachedSep1Images(ctx)
	if len(images) == 0 {
		return
	}
	for i := range rows {
		if rows[i].Image != nil || rows[i].Issuer == nil || rows[i].Code == "" {
			continue
		}
		if img := images[sep1ImageKey(rows[i].Code, *rows[i].Issuer)]; img != "" {
			v := img
			rows[i].Image = &v
		}
	}
}

// assetDetailFromCoinRow projects a storage CoinRow into the
// AssetDetail wire shape. Mirrors the scalar field population in
// applyCoinRowToDetail but for listing rows (so the listing endpoint
// returns the same per-row shape /v1/coins users were getting).
func assetDetailFromCoinRow(row timescale.CoinRow) AssetDetail {
	asset, err := canonical.ParseAsset(row.AssetID)
	d := AssetDetail{
		AssetID:    row.AssetID,
		Code:       row.Code,
		Decimals:   7,
		Sep1Status: "not_applicable",
	}
	if err == nil {
		d.Type = string(asset.Type)
	}
	if row.IssuerGStrkey != "" {
		v := row.IssuerGStrkey
		d.Issuer = &v
	}
	// Slug + activity metadata.
	if row.Slug != "" {
		d.Slug = row.Slug
	}
	if row.FirstSeenLedger != 0 {
		v := row.FirstSeenLedger
		d.FirstSeenLedger = &v
	}
	if row.LastSeenLedger != 0 {
		v := row.LastSeenLedger
		d.LastSeenLedger = &v
	}
	if row.ObservationCount != 0 {
		v := row.ObservationCount
		d.ObservationCount = &v
	}
	// Coin-overlay scalars (price / volume / change percentages).
	if row.PriceUSD != nil {
		d.PriceUSD = row.PriceUSD
	}
	if row.Volume24hUSD != nil {
		d.VolumeUSD24h = row.Volume24hUSD
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

// normaliseAssetClass folds surface labels into the catalogue's
// internal class identifiers. "blockchain" / "cryptocurrency" are
// accepted as back-compat aliases for the catalogue's "crypto"
// class. Empty + "all" both fall through unchanged; the handler
// treats them as the legacy classic_assets path.
func normaliseAssetClass(raw string) string {
	c := strings.ToLower(strings.TrimSpace(raw))
	switch c {
	case "blockchain", "cryptocurrency", "cryptocurrencies":
		return "crypto"
	}
	return c
}

// handleAssetListFromCatalogue serves /v1/assets?asset_class={fiat,
// stablecoin,crypto} from the verified-currency catalogue (45
// rows total today). The catalogue is the source of cross-chain
// identities (USDC the currency, GBP the fiat) — distinct from
// the classic_assets table which carries per-issuer-on-Stellar
// rows (USDC-GA5Z..., USDT-GCQT...).
//
// Returns rows with:
//   - asset_id = slug ("usdc", "us-dollar", "xlm") — the route
//     /v1/assets/{slug} dispatches to GlobalAssetView for these.
//   - type = "global"; consumers distinguishing wire shape can
//     check (a) the type field or (b) the absence of issuer.
//   - market_cap_usd populated for fiat via the same fxHistory-
//     backed path /v1/assets/verified uses (R-018 wave 140 fix).
//     Crypto + stablecoin rows lack supply data in the catalogue
//     today; their market_cap stays null until the crypto-supply
//     pipeline lands.
//   - issuer/contract_id/home_domain/sep1_status absent — those
//     are Stellar-asset-specific and belong on the canonical
//     /v1/assets/{asset_id} detail route.
//
// Pagination via offset cursor — the result set is bounded at
// ≤45 catalogue rows per class, so a simple offset is sufficient.
func (s *Server) handleAssetListFromCatalogue(w http.ResponseWriter, r *http.Request, class string, limit int, cursor string) {
	if s.verifiedCurrencies == nil {
		writeJSON(w, []AssetDetail{}, Flags{})
		return
	}
	// StellarIssued (not Browseable): /v1/assets is Stellar-only post-split
	// (LC-001), so asset_class=fiat yields nothing here (fiat lives on
	// /v1/external/assets); stablecoin/crypto yield only Stellar-issued rows.
	matched := filterCatalogueByClass(s.verifiedCurrencies.StellarIssued(), currency.AssetClass(class))
	caps := s.computeCatalogueMarketCaps(r.Context(), matched, class)
	rows := projectCatalogueRows(matched, caps)
	sortAssetDetailsByMarketCapDesc(rows)
	s.writeCataloguePage(w, r, rows, limit, cursor)
}

// handleExternalAssetList serves GET /v1/external/assets — the NON-Stellar
// assets split off /v1/assets (LC-001): fiat currencies (USD, EUR, …) and
// reference-only coins (BTC, ETH, …) that have no Stellar issuance. An
// optional ?asset_class=fiat|crypto|stablecoin narrows to one class. Same
// GlobalAssetView catalogue wire shape + offset-cursor pagination as the
// Stellar catalogue-class listing; market_cap_usd is populated per row
// (fiat via the fxHistory path, others null until crypto-supply lands).
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

	if s.verifiedCurrencies == nil {
		writeJSON(w, []AssetDetail{}, Flags{})
		return
	}
	entries := s.verifiedCurrencies.External()
	if class := normaliseAssetClass(r.URL.Query().Get("asset_class")); class == "fiat" || class == "stablecoin" || class == "crypto" {
		entries = filterCatalogueByClass(entries, currency.AssetClass(class))
	}
	caps := s.computeAllCatalogueMarketCaps(r.Context(), entries)
	rows := projectCatalogueRows(entries, caps)
	sortAssetDetailsByMarketCapDesc(rows)
	s.writeCataloguePage(w, r, rows, limit, cursor)
}

// filterCatalogueByClass returns the catalogue entries matching the
// target class in source order. Helper extracted from
// handleAssetListFromCatalogue to keep that handler under the
// gocognit budget.
func filterCatalogueByClass(entries []*currency.VerifiedCurrency, target currency.AssetClass) []*currency.VerifiedCurrency {
	out := make([]*currency.VerifiedCurrency, 0, len(entries))
	for _, vc := range entries {
		if vc.Class == target {
			out = append(out, vc)
		}
	}
	return out
}

// computeCatalogueMarketCaps fans out market_cap_usd computation
// per row. Only fiat rows carry a market cap (fiatMarketCapUSD —
// M2 × FX rate via fxHistory); crypto/stablecoin catalogue rows
// have no catalogue-level market cap (their per-Stellar-asset F2
// fields on /v1/assets/{asset_id} are the canonical source).
//
// Returns a parallel-indexed slice of market_cap strings (empty for
// every non-fiat row, and for fiat rows where no FX rate resolved).
func (s *Server) computeCatalogueMarketCaps(ctx context.Context, matched []*currency.VerifiedCurrency, class string) []string {
	caps := make([]string, len(matched))
	if class != "fiat" {
		return caps
	}
	forEachBounded(len(matched), readFanoutConcurrency, func(i int) {
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

// projectCatalogueRows applies projectCatalogueRow + the parallel
// market_cap slice to produce the wire shape.
func projectCatalogueRows(matched []*currency.VerifiedCurrency, caps []string) []AssetDetail {
	rows := make([]AssetDetail, len(matched))
	for i, vc := range matched {
		rows[i] = projectCatalogueRow(vc)
		if caps[i] != "" {
			c := caps[i]
			rows[i].MarketCapUSD = &c
		}
	}
	return rows
}

// parseOffsetCursor parses an offset-style pagination cursor. The
// cursor is the integer offset emitted as pagination.next by the
// catalogue listing paths. An empty cursor means "start at page 1".
//
// A non-empty, non-integer (or negative) cursor is a client error:
// we 400 it rather than silently restarting at page 1, matching the
// opaque-cursor markets.go pattern (G3-08/G3-10). Silently swallowing
// it made a typo'd cursor look like a successful first page.
//
// Reports ok=false after writing a problem+json on parse failure.
func parseOffsetCursor(w http.ResponseWriter, r *http.Request, cursor string) (int, bool) {
	if cursor == "" {
		return 0, true
	}
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 0 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor must be the integer pagination.next value from a prior response, or omitted to start at page 1.")
		return 0, false
	}
	return n, true
}

// writeCataloguePage applies offset-cursor pagination + writes the
// envelope. Catalogue paging is small (≤45 rows per class) so offset
// is sufficient.
//
// Fills the headline USD price on the sliced page (not the whole
// catalogue) before writing, so a class-filtered listing row carries
// the same price_usd as the single-asset /v1/assets/{slug} view —
// previously these rows came from the price-less catalogue projection,
// so every crypto/stablecoin row (even XLM) listed price_usd: null
// (audit 2026-06-19 item 4).
func (s *Server) writeCataloguePage(w http.ResponseWriter, r *http.Request, rows []AssetDetail, limit int, cursor string) {
	offset, ok := parseOffsetCursor(w, r, cursor)
	if !ok {
		return
	}
	if offset >= len(rows) {
		writeJSON(w, []AssetDetail{}, Flags{})
		return
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	page := rows[offset:end]
	s.fillCataloguePricesForPage(r.Context(), page)
	s.fillCatalogueStatsForPage(r.Context(), page)
	s.attachSparkline7dIfRequested(r, page)
	env := Envelope{Data: page, Flags: Flags{}}
	if end < len(rows) {
		next := strconv.Itoa(end)
		env.Pagination = &Pagination{Next: next}
	}
	writeEnvelope(w, env)
}

// fillCataloguePricesForPage resolves the headline USD price for each
// row on an already-sliced catalogue page, reusing buildGlobalAssetView's
// three-tier fallback chain so listing rows match the single-asset view.
//
// Bounded to the page (≤limit rows), NOT the whole catalogue, so the
// unified "all" listing's first page doesn't fan a price computation
// over every catalogue entry on every request. Each lookup is a point
// read on prices_1m / the triangulated FX cache (cheap); the fan-out is
// parallel under the caller's request context. Rows that already carry a
// price (or whose slug no longer resolves) are skipped. Market cap is
// adopted from the view only when the row doesn't already have one (fiat
// rows are pre-filled by the catalogue market-cap path).
func (s *Server) fillCataloguePricesForPage(ctx context.Context, page []AssetDetail) {
	if s.verifiedCurrencies == nil {
		return
	}
	priceCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	forEachBounded(len(page), readFanoutConcurrency, func(i int) {
		if page[i].PriceUSD != nil {
			return
		}
		vc, ok := s.verifiedCurrencies.LookupBySlug(page[i].Slug)
		if !ok {
			return
		}
		price, mcap := s.catalogueRowPricing(priceCtx, vc)
		page[i].PriceUSD = price
		if page[i].MarketCapUSD == nil {
			page[i].MarketCapUSD = mcap
		}
	})
}

// catalogueRowPricing resolves the headline USD price (and market cap)
// for one catalogue entry from buildGlobalAssetView — which serves the
// global three-tier CEX/aggregator price and, for Stellar-only tokens
// (AQUA, yXLM, SHX, …) the global tier can't reach, falls back to the
// same on-chain trades-derived price the classic /v1/assets listing
// shows (fillGlobalPriceFromOnChain) so a catalogue row matches the
// classic asset row instead of listing null.
func (s *Server) catalogueRowPricing(ctx context.Context, vc *currency.VerifiedCurrency) (priceUSD, marketCapUSD *string) {
	view := s.buildGlobalAssetView(ctx, vc)
	return view.PriceUSD, view.MarketCapUSD
}

// onChainListingPriceUSD returns the on-chain per-Stellar-asset USD
// price the /v1/assets listing shows for assetID (via the per-asset
// listing reader), or nil when no reader is wired, the row is absent,
// or it carries no price. The GlobalAssetView on-chain fallback
// (fillGlobalPriceFromOnChain) uses it to price Stellar-only verified
// tokens (AQUA, yXLM, SHX, …) the global CEX/aggregator tier misses.
func (s *Server) onChainListingPriceUSD(ctx context.Context, assetID string) *string {
	if s.coins == nil || assetID == "" {
		return nil
	}
	row, err := s.coins.GetCoinByAssetID(ctx, assetID)
	if err != nil || row.PriceUSD == nil {
		return nil
	}
	return row.PriceUSD
}

// projectCatalogueRow maps a catalogue entry to the listing's
// AssetDetail wire shape. NO issuer / contract_id / home_domain
// / sep1_status — those are Stellar-asset-specific and belong
// on the canonical /v1/assets/{asset_id} detail route.
func projectCatalogueRow(vc *currency.VerifiedCurrency) AssetDetail {
	name := vc.Name
	d := AssetDetail{
		AssetID:    vc.Slug,
		Type:       "global",
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

// sortAssetDetailsByMarketCapDesc sorts rows in place by
// market_cap_usd descending. Nil market_cap sinks to the bottom
// (preserves the catalogue's source order among equally-unknown
// rows via stable sort). Used by the catalogue-listing path; the
// classic-assets path orders server-side in SQL.
func sortAssetDetailsByMarketCapDesc(rows []AssetDetail) {
	sort.SliceStable(rows, func(i, j int) bool {
		ai := bigFloatFromOptionalString(rows[i].MarketCapUSD)
		aj := bigFloatFromOptionalString(rows[j].MarketCapUSD)
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

// handleAssetListUnified serves /v1/assets when no asset_class
// filter is set and no issuer filter is in play — the CMC/CoinGecko-
// style "All Assets" view (R-018 assets-unification endgame).
//
// Wire shape: catalogue rows (≤45 across all 3 classes) come first,
// ordered by market_cap_usd desc (fiats top the chart at $44T/$21T/
// $18T/…, then crypto + stablecoin catalogue entries which lack
// supply data today and sort below the fiats). Classic_assets pages
// follow, ordered by trailing-24h volume_usd desc (the existing
// CoinsOrderVolume24hUSDDesc path — most-actively-traded Stellar-
// classic rows surface first within their phase).
//
// Cursor protocol — phase-prefixed:
//   - empty cursor             → catalogue phase, offset 0.
//   - "catalogue:<offset>"     → catalogue phase resumed at offset.
//   - "classic:<inner_cursor>" → classic phase via ListCoinsExt; the
//     inner cursor is whatever
//     CoinsOrderVolume24hUSDDesc emits.
//   - "classic:"               → classic phase fresh start (catalogue
//     just exhausted; inner cursor empty).
//
// Page sequence on a default limit=100 (catalogue has ~45 rows):
//
//	page 1 → 45 catalogue rows + 55 classic rows (single call to
//	         this handler that exhausts catalogue then continues
//	         into classic for the remainder).
//	page 2+ → 100 classic rows each via the classic phase.
//
// Phase transitions within a single page are NOT supported in v1
// of this protocol — each page is either fully catalogue or fully
// classic. (Mixed pages would complicate downstream consumers
// expecting a single ordering predicate per page.) When catalogue
// has fewer remaining than `limit`, this handler returns the
// catalogue tail and signals "classic:" next cursor; the client
// fires the next page to fetch classic rows.
func (s *Server) handleAssetListUnified(w http.ResponseWriter, r *http.Request, limit int, cursor string) {
	phase, inner := parseUnifiedCursor(cursor)

	if phase == "catalogue" {
		s.serveCatalogueUnifiedPage(w, r, limit, inner)
		return
	}
	// phase == "classic"
	s.serveClassicUnifiedPage(w, r, limit, inner)
}

// parseUnifiedCursor decodes the phase-prefixed cursor format. An
// empty cursor maps to catalogue phase offset 0.
func parseUnifiedCursor(cursor string) (phase, inner string) {
	if cursor == "" {
		return "catalogue", "0"
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

// serveCatalogueUnifiedPage projects the catalogue, computes
// market_cap, sorts, slices to the requested offset/limit, and
// writes the envelope with the appropriate next-cursor.
func (s *Server) serveCatalogueUnifiedPage(w http.ResponseWriter, r *http.Request, limit int, innerCursor string) {
	if s.verifiedCurrencies == nil {
		// No catalogue → skip directly to classic phase.
		s.serveClassicUnifiedPage(w, r, limit, "")
		return
	}
	// StellarIssued (not Browseable): the unified /v1/assets listing is
	// Stellar-only post-split (LC-001) — fiat + reference-only coins move to
	// /v1/external/assets. classic_assets (the classic phase) are all Stellar.
	entries := s.verifiedCurrencies.StellarIssued()
	caps := s.computeAllCatalogueMarketCaps(r.Context(), entries)
	rows := projectCatalogueRows(entries, caps)
	sortAssetDetailsByMarketCapDesc(rows)
	// q= filter over the catalogue phase (S-011). The classic phase
	// filters server-side via ListCoinsOptions.Q; the catalogue is a
	// ~30-row in-process slice.
	rows = filterCatalogueRowsByQuery(rows, r.URL.Query().Get("q"))

	offset := 0
	if innerCursor != "" {
		if n, err := strconv.Atoi(innerCursor); err == nil && n > 0 {
			offset = n
		}
	}
	if offset >= len(rows) {
		// Catalogue done → transition to classic.
		s.serveClassicUnifiedPage(w, r, limit, "")
		return
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	page := rows[offset:end]
	// Fill the headline USD price on the sliced page (same bounded
	// fan-out as the class-filtered path) so the unified "all" listing's
	// catalogue rows (XLM, USDC, …) carry price_usd instead of null
	// (audit 2026-06-19 item 4).
	s.fillCataloguePricesForPage(r.Context(), page)
	// AM-10 twin-stats merge — THIS is the function that serves the
	// unified page 1; the first three attempts landed in
	// writeCataloguePage (the class-filtered path) because both share
	// a byte-identical price-fill line and the edits anchored on the
	// first occurrence. Keep both call sites.
	s.fillCatalogueStatsForPage(r.Context(), page)
	s.attachSparkline7dIfRequested(r, page)
	env := Envelope{Data: page, Flags: Flags{}}
	if end < len(rows) {
		env.Pagination = &Pagination{Next: "catalogue:" + strconv.Itoa(end)}
		writeEnvelope(w, env)
		return
	}
	// Catalogue exhausted below the requested limit: FILL the remainder
	// of the page from the classic stream (S-002 — this function's own
	// doc always promised the fill; without it page 1 returned the
	// 11-row catalogue tail regardless of limit, and the /assets page
	// presented the curated sliver as the entire asset universe).
	if remaining := limit - len(page); remaining > 0 {
		classicRows, nextInner, ok := s.fetchClassicUnifiedRows(w, r, remaining, "")
		if !ok {
			return
		}
		env.Data = append(page, classicRows...)
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

// serveClassicUnifiedPage delegates to the existing CoinsReader
// path with Volume24hUSDDesc ordering. The inner cursor is what
// that path returned on the prior call. Next-cursor gets phase-
// prefixed before going out the wire.
func (s *Server) serveClassicUnifiedPage(w http.ResponseWriter, r *http.Request, limit int, innerCursor string) {
	out, nextInner, ok := s.fetchClassicUnifiedRows(w, r, limit, innerCursor)
	if !ok {
		return
	}
	env := Envelope{Data: out, Flags: Flags{}}
	if nextInner != "" {
		env.Pagination = &Pagination{Next: "classic:" + nextInner}
	}
	writeEnvelope(w, env)
}

// fetchClassicUnifiedRows reads one page of the classic (long-tail)
// phase. ok=false means a response (error or empty terminator) was
// already written. nextInner is empty when the stream is exhausted.
// Shared by the classic phase AND the page-1 fill (S-002: page 1 used
// to return just the 11-row catalogue tail regardless of limit,
// making the curated sliver look like the asset universe).
func (s *Server) fetchClassicUnifiedRows(w http.ResponseWriter, r *http.Request, limit int, innerCursor string) ([]AssetDetail, string, bool) {
	if s.coins == nil {
		// No coins reader wired → empty terminator.
		writeJSON(w, []AssetDetail{}, Flags{})
		return nil, "", false
	}
	opts := timescale.ListCoinsOptions{
		Cursor: innerCursor,
		Order:  timescale.CoinsOrderVolume24hUSDDesc,
		// S-011: the storage layer has supported Q since the coins
		// store landed; the unified path never passed it, so the
		// explorer's search box round-tripped to the same page.
		Q: strings.TrimSpace(r.URL.Query().Get("q")),
	}
	// Overfetch-by-one (same shape as handleAssetListFromCoins) to
	// drive the cursor advance.
	opts.Limit = limit + 1
	rows, err := s.coins.ListCoinsExt(r.Context(), opts)
	if err != nil {
		if clientAborted(r, err) {
			return nil, "", false
		}
		s.logger.Error("ListCoinsExt (unified) failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return nil, "", false
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	out := make([]AssetDetail, 0, len(rows))
	for _, row := range rows {
		out = append(out, assetDetailFromCoinRow(row))
	}
	out = s.suppressCatalogueTwins(out)
	s.stampListingCollisions(out)
	s.fillMarketCapsFromSupply(r.Context(), out)
	s.fillImagesFromSep1(r.Context(), out)
	s.attachSparkline7dIfRequested(r, out)
	nextInner := ""
	if hasMore && len(out) > 0 {
		last := rows[len(rows)-1]
		// Volume24hUSDDesc cursor shape: <vol_or_blank>:<asset_id>.
		volStr := ""
		if last.Volume24hUSD != nil {
			volStr = *last.Volume24hUSD
		}
		nextInner = volStr + ":" + last.AssetID
	}
	return out, nextInner, true
}

// computeAllCatalogueMarketCaps fans out market_cap_usd across the
// full catalogue. Only fiat rows carry a market cap (fiatMarketCapUSD
// — M2 × FX rate); crypto/stablecoin catalogue rows have no
// catalogue-level market cap (their per-Stellar-asset F2 fields on
// /v1/assets/{asset_id} are the canonical source).
//
// Parallel-indexed to entries; empty at index i means the row has
// no market_cap available.
func (s *Server) computeAllCatalogueMarketCaps(ctx context.Context, entries []*currency.VerifiedCurrency) []string {
	caps := make([]string, len(entries))
	forEachBounded(len(entries), readFanoutConcurrency, func(i int) {
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

// normaliseAssetIDInput rescues the most common case-typo on
// /v1/assets/{asset_id} — uppercase "NATIVE". The canonical
// asset_id format demands lowercase `native` (per ADR-0010 +
// asset_fiat_test.go's "case-significance is intentional" pin),
// so callers passing `NATIVE` got 400 invalid-asset-id pre-fix.
//
// Scope is deliberately narrow: only the bare-`native` string
// (case-insensitive) collapses. Other compound forms
// (`USDC-Gxxxx`, `CDLZF…`, `fiat:USD`) preserve case-significance
// because:
//   - Stellar protocol allows issuers to mint case-different
//     classic codes; uppercasing `usdc-Gxxxx` would silently
//     merge two distinct assets.
//   - Soroban contract IDs (C-strkeys) are uppercase-only by
//     SEP-23 — the parser already accepts uppercase, no rescue
//     needed.
//   - `fiat:USD` is case-significant per asset_fiat.go (NewFiatAsset
//     rejects lowercase ISO codes).
func normaliseAssetIDInput(raw string) string {
	if strings.EqualFold(raw, "native") {
		return "native"
	}
	return raw
}

// handleAssetGet serves GET /v1/assets/{asset_id}.
//
// Dispatch order (R-018 Phase 1.4a):
//  1. If the path parameter matches a verified-currency slug
//     (USDC, EURC, AQUA, …), route to the global view at
//     [handleGlobalAsset] — cross-chain identity with per-network
//     entries.
//  2. Otherwise parse as a canonical asset_id (native | CODE-G… |
//     C… | fiat:CODE) and serve the per-Stellar-asset view.
//
// Slug lookup is case-insensitive (via Catalogue.LookupBySlug) and
// runs before canonical-id parsing — slugs never collide with
// canonical-id shapes because canonical ids have anchored prefixes
// (a single bare ticker like "usdc" doesn't parse as any canonical
// shape, so dispatch order is the only deciding factor).
func (s *Server) handleAssetGet(w http.ResponseWriter, r *http.Request) {
	rawID := normaliseAssetIDInput(r.PathValue("asset_id"))

	if s.tryServeGlobalAsset(w, r, rawID) {
		return
	}

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

	// SAC → classic identity (board #40, RFP audit): a C-address that
	// is a Stellar Asset Contract resolves to the CLASSIC asset it
	// wraps, so the response carries the classic price/supply/detail.
	// Trust anchor is the lake instance's StellarAsset executable
	// (only stellar-core mints it); belt-and-braces, the classic
	// asset must re-derive to the queried address.
	if parsed.Type == canonical.AssetSoroban && s.explorer != nil {
		if classic, ok := s.resolveSACToClassic(r.Context(), parsed.ContractID); ok {
			parsed = classic
		}
	}

	// Response-cache check. Drift-safe by construction — the cached
	// entry was produced by this same handler within the last 30s.
	// Covers the full F2 chain (Volume24hUSDForAsset / supply.LatestSupply
	// / 2× lookupUSDPrice / fetchSupplySnapshot / populateMarketCap)
	// plus applySep1Overlay + applyCoinExtensionFields + the verified-
	// currency overlay. Each of those costs ~50-200ms warm; together
	// they dominate the ~700-900ms warm latency observed pre-cache
	// (rc.63 on r1, 2026-05-21).
	cacheKey := parsed.String()
	if entry, ok := s.assetDetailCache.get(cacheKey); ok {
		writeCachedAssetDetail(w, entry)
		return
	}

	detail, served := s.resolveAssetDetail(w, r, parsed)
	if served {
		return
	}

	// Real-decimals overlay for Soroban tokens (from the lake's captured
	// instance METADATA). MUST run before applyF2Fields — the market-cap /
	// FDV math divides by 10^detail.Decimals. Classic + native are always
	// 7 by protocol and are never consulted (a SAC-address request was
	// already re-pointed at its classic identity above).
	s.applyTokenDecimals(r.Context(), &detail, parsed)

	// Backfill HomeDomain from the curated known-issuers map when
	// the storage row doesn't carry one (classic assets ingested
	// before `stellarindex-ops sep1-refresh` ran, or issuers whose
	// SEP-1 lookup never persisted because they aren't in the
	// watched set). Mirrors enrichIssuer's policy on /v1/issuers,
	// which is how that endpoint returns home_domain="centre.io"
	// for USDC while /v1/assets was reporting null.
	// R-016 in `docs/review-2026-05-10.md`.
	if (detail.HomeDomain == nil || *detail.HomeDomain == "") && detail.Issuer != nil && *detail.Issuer != "" {
		hd, _ := enrichIssuer(*detail.Issuer, "", "")
		if hd != "" {
			detail.HomeDomain = &hd
		}
	}

	// SEP-1 overlay — reads the cached payload `sep1-refresh` cron
	// persisted in `issuers.sep1_payload`. NO live HTTPS fetch.
	if s.sep1Cache != nil {
		s.applySep1Overlay(r.Context(), &detail, parsed)
	} else if detail.HomeDomain != nil && *detail.HomeDomain != "" && detail.Sep1Status == "" {
		detail.Sep1Status = "not_fetched"
	}

	// F2 overlay — supply / market-cap / FDV. Best-effort. The
	// market-cap math reads detail.Decimals, which is the on-chain
	// scale (the SEP-1 overlay surfaces display_decimals separately and
	// no longer touches Decimals — F-1321), so overlay ordering no
	// longer affects the unit math.
	s.applyF2Fields(r.Context(), &detail, parsed)

	// Coin-equivalence overlay (R-018 final) — lifts price / top_markets
	// / history / changes / ATH / scam_reason from the coins catalogue
	// so /v1/assets/{id} is a superset of /v1/coins/{slug}. Skipped
	// for fiat:* (no coin row); a no-op when no coins reader is wired
	// or the asset has no coin row.
	s.applyCoinExtensionFields(r.Context(), &detail, parsed)

	// Verified-currency overlay (R-018 Phase 1.1) — attaches the
	// `unverified_warning` body + flips flags.unverified_ticker_collision
	// when the asset code matches a verified Stellar ticker but the
	// issuer doesn't. No-op when no catalogue is wired or the asset
	// isn't a classic Stellar asset.
	flags := s.verifiedCurrencyFlags(&detail, parsed)

	// Render to bytes once, cache them, write them. The cache check at
	// the top of this function short-circuits subsequent requests for
	// the same asset_id within the TTL window — see [assetDetailResponseCache].
	body, err := renderAssetDetailEnvelope(detail, flags)
	if err != nil {
		// Marshal failure is exceedingly rare (AssetDetail is plain
		// struct tags); fall back to writeJSON so client still gets a
		// (potentially less-optimal) response rather than a 500.
		s.logger.Debug("asset detail envelope render failed; falling back to direct write", "asset_id", cacheKey, "err", err)
		writeJSON(w, detail, flags)
		return
	}
	s.assetDetailCache.put(cacheKey, body)
	writeCachedAssetDetail(w, &assetDetailEntry{body: body, cachedAt: time.Now()})
}

// resolveAssetDetail fetches the AssetDetail for parsed: from the
// reader when wired, else a canonical-echo. Returns served=true
// when a problem response was already written (404 / 500 / client-
// abort), and the caller must bail out without further work.
// Extracted from handleAssetGet to keep that function's gocognit
// complexity under the 20-line ceiling.
func (s *Server) resolveAssetDetail(w http.ResponseWriter, r *http.Request, parsed canonical.Asset) (AssetDetail, bool) {
	reader := s.assetReaderOrNil()
	if reader == nil {
		// No reader wired — echo back a pure-canonical representation.
		// Useful for clients integrating against the wire contract
		// before we have an asset catalogue populated.
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

// applyTokenDecimals overlays a Soroban token's real on-chain decimals()
// onto detail.Decimals, read from the lake's captured contract-instance
// METADATA (token-sdk convention — see clickhouse.TokenDecimals). Only
// Soroban assets are consulted: classic + native assets ARE 7 by protocol
// (stroops), so their default is already correct, not an approximation.
// Best-effort: an uncaptured instance, a non-standard token with no stored
// metadata, or a read error all leave the documented default of 7 in place.
func (s *Server) applyTokenDecimals(ctx context.Context, detail *AssetDetail, a canonical.Asset) {
	if s.tokenDecimals == nil || a.Type != canonical.AssetSoroban || a.ContractID == "" {
		return
	}
	d, found, err := s.tokenDecimals.TokenDecimals(ctx, a.ContractID)
	if err != nil {
		s.logger.Debug("token decimals overlay failed; keeping default", "contract_id", a.ContractID, "err", err)
		return
	}
	if found {
		detail.Decimals = int(d)
	}
}

// tryServeGlobalAsset returns true when `raw` matched a verified-
// currency catalogue entry (by slug OR by ticker) and the global
// view was served. Caller bails out of the canonical-id path on a
// true return.
//
// Both lookups are case-insensitive. Ticker fallback lets
// /v1/assets/USD dispatch to the same view as /v1/assets/us-dollar
// (and /USDC = /usdc, /EUR = /euro, etc.) — useful for clients
// that have an ISO ticker on hand but not the friendly slug.
// Slug match wins over ticker match if both resolve.
func (s *Server) tryServeGlobalAsset(w http.ResponseWriter, r *http.Request, raw string) bool {
	if s.verifiedCurrencies == nil {
		return false
	}
	vc := s.lookupCatalogue(raw)
	if vc == nil {
		return false
	}
	// LC-001: /v1/assets is Stellar-only. An external catalogue slug (a fiat
	// currency or a reference-only coin — no Stellar issuance) is NOT served
	// here; its detail lives on /v1/external/assets/{slug}. 404 with a pointer
	// (no redirect, per the split's contract).
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

// lookupCatalogue resolves a slug OR ticker to a verified currency (slug wins),
// case-insensitive. Returns nil when neither matches.
func (s *Server) lookupCatalogue(raw string) *currency.VerifiedCurrency {
	if s.verifiedCurrencies == nil {
		return nil
	}
	if vc, ok := s.verifiedCurrencies.LookupBySlug(raw); ok {
		return vc
	}
	if vc, ok := s.verifiedCurrencies.LookupByTicker(raw); ok {
		return vc
	}
	return nil
}

// handleExternalAssetGet serves GET /v1/external/assets/{slug} — the detail
// view for a NON-Stellar asset (fiat currency or reference-only coin). A
// Stellar asset 404s here (its detail lives on /v1/assets/{slug}); LC-001.
func (s *Server) handleExternalAssetGet(w http.ResponseWriter, r *http.Request) {
	raw := normaliseAssetIDInput(r.PathValue("slug"))
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

// verifiedCurrencyFlags applies the unverified-collision warning
// to detail and returns the matching envelope flags. Separated
// from handleAssetGet so the latter stays linear-readable.
func (s *Server) verifiedCurrencyFlags(detail *AssetDetail, asset canonical.Asset) Flags {
	flags := Flags{}
	if applyUnverifiedWarning(detail, asset, s.verifiedCurrencies) {
		flags.UnverifiedTickerCollision = true
	}
	return flags
}

// stampListingCollisions sets AssetDetail.UnverifiedTickerCollision on
// every LISTING row whose (code, issuer) is a look-alike of a verified
// currency — a classic asset using a verified Stellar ticker but NOT
// the verified issuer. The listing query serves COALESCE(slug, code)
// AS slug, so a NULL-slug impersonator emits the verified asset's CODE
// as its slug and would otherwise be badged "verified" by a
// slug-keyed consumer. Stamping the per-row flag lets consumers
// withhold the badge from impersonators while the real verified row
// (StellarCollision → false) keeps it.
//
// No-op when no catalogue is wired. Catalogue rows on the unified
// listing carry no issuer (type=global) and are skipped — they ARE the
// verified identities.
func (s *Server) stampListingCollisions(rows []AssetDetail) {
	if s.verifiedCurrencies == nil {
		return
	}
	for i := range rows {
		if rows[i].Issuer == nil || *rows[i].Issuer == "" || rows[i].Code == "" {
			continue
		}
		if _, collision := s.verifiedCurrencies.StellarCollision(rows[i].Code, *rows[i].Issuer); collision {
			rows[i].UnverifiedTickerCollision = true
		}
	}
}

// applyUnverifiedWarning stamps detail.UnverifiedWarning when asset
// is a classic Stellar asset whose code matches a verified-currency
// ticker but whose issuer doesn't. Returns true when the warning
// was attached so the caller can set the matching envelope flag.
//
// Lookups against a nil catalogue are safe and return false — the
// handler can call this unconditionally.
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
		// Defensive — StellarCollision only returns true when the
		// catalogue has a Stellar entry for the code, so this branch
		// is unreachable in practice. Bail safely if the invariant
		// ever changes.
		return false
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

// AssetMetadata is the wire shape of /v1/assets/{id}/metadata —
// the SEP-1 overlay slice of AssetDetail without the core
// canonical fields. Distinct endpoint for clients (Freighter,
// wallets) that just want the metadata and not the per-asset
// canonical info they already have. Reuses the same pointer-
// elision-on-empty pattern as AssetDetail.
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
	// SEP-1 issuance declarations — projected from the same overlay
	// as AssetDetail. Distinct from any live-ledger F2 numbers; this
	// surface is metadata-only.
	Conditions  *string `json:"conditions,omitempty"`
	FixedNumber *string `json:"fixed_number,omitempty"`
	MaxNumber   *string `json:"max_number,omitempty"`
	IsUnlimited *bool   `json:"is_unlimited,omitempty"`
}

// handleAssetMetadata serves GET /v1/assets/{asset_id}/metadata.
// Same overlay-resolution path as handleAssetGet but returns ONLY
// the SEP-1 fields. Useful for clients that need refresh of the
// metadata slice without re-fetching the canonical asset core.
//
// Returns 404 when the asset isn't indexed; 200 with sep1_status =
// "not_applicable" / "not_fetched" / "unreachable" / "no_match" /
// "verified" otherwise. Never 503 (the resolver fail mode is
// reflected in sep1_status, not HTTP status — same behaviour as
// /v1/assets/{id}).
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

	// Same known-issuers backfill as handleAssetGet (R-016) — keeps
	// the two surfaces in lockstep on the SEP-1 status they report.
	if (detail.HomeDomain == nil || *detail.HomeDomain == "") && detail.Issuer != nil && *detail.Issuer != "" {
		hd, _ := enrichIssuer(*detail.Issuer, "", "")
		if hd != "" {
			detail.HomeDomain = &hd
		}
	}

	if s.sep1Cache != nil {
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
	}
	writeJSON(w, out, Flags{})
}

// applySep1Overlay attaches the issuer's cached SEP-1 metadata to
// detail by reading `issuers.sep1_payload` (populated by the
// `sep1-refresh` cron). Sets `sep1_status` to one of: `verified`
// (cached payload matched a [[CURRENCIES]] entry), `no_match`
// (issuer cached but no per-asset currency entry), `not_fetched`
// (no cached payload yet — cron hasn't visited), `not_applicable`
// (asset has no issuer to look up).
//
// Pre-2026-05-29 this method resolved SEP-1 via live HTTPS on every
// uncached request. That call dominated /v1/assets/{id} p95
// (~4s long tail on cold issuers); it now lives only in the
// `sep1-refresh` cron, which the API reads from.
//
//nolint:gocyclo // linear field-overlay sequence; splitting would scatter the per-field nil checks across helpers.
func (s *Server) applySep1Overlay(ctx context.Context, detail *AssetDetail, asset canonical.Asset) {
	// Soroban + native assets have no issuer row to look up. Mark
	// not_applicable so the response is shaped consistently.
	if asset.Type != canonical.AssetClassic || asset.Issuer == "" {
		detail.Sep1Status = "not_applicable"
		return
	}
	sep, err := s.sep1Cache.GetIssuerSep1Cached(ctx, asset.Issuer)
	if err != nil {
		s.logger.Debug("sep1 cached lookup failed", "asset_id", asset.String(),
			"issuer", asset.Issuer, "err", err)
		detail.Sep1Status = "not_fetched"
		return
	}
	if sep == nil {
		detail.Sep1Status = "not_fetched"
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
	// IsUnlimited: TOML doesn't distinguish "absent" from "explicitly
	// false" (parser zero-value is false either way). Project the bool
	// only when the issuer addressed supply at all.
	if match.IsUnlimited || match.FixedNumber != "" || match.MaxNumber != "" {
		unlim := match.IsUnlimited
		detail.IsUnlimited = &unlim
	}

	// display_decimals is a UI rounding hint, NOT a unit scale — surface
	// it on its own field and leave detail.Decimals as the on-chain scale
	// (7 for classic) so supply display and market-cap math stay correct
	// (F-1321). The non-standard SEP-1 `decimals` field is treated the
	// same way: informational, never the amount divisor.
	if match.DisplayDecimals > 0 {
		dd := match.DisplayDecimals
		detail.DisplayDecimals = &dd
	} else if match.Decimals > 0 {
		dd := match.Decimals
		detail.DisplayDecimals = &dd
	}
}

// findMatchingCachedCurrency returns the SEP-1 currency entry in a
// cached issuer payload whose (code, issuer) matches the requested
// classic asset, or nil when there is no match. Code comparison is
// case-insensitive; issuer must match exactly. Walks the
// [timescale.IssuerSep1Cached] currencies slice. (The live-fetched
// twin that this once mirrored was removed.)
func findMatchingCachedCurrency(sep *timescale.IssuerSep1Cached, asset canonical.Asset) *timescale.IssuerSep1Currency {
	if asset.Type != canonical.AssetClassic {
		return nil
	}
	if asset.Code == "" || asset.Issuer == "" {
		return nil
	}
	for i := range sep.Currencies {
		c := &sep.Currencies[i]
		if !strings.EqualFold(c.Code, asset.Code) {
			continue
		}
		if c.Issuer == "" || c.Issuer != asset.Issuer {
			continue
		}
		return c
	}
	return nil
}

// isSafeImageURL reports whether s is a plausible http(s) image
// URL. Refuses the tricky schemes (javascript:, data:, file:,
// blob:) that an issuer could embed in a hostile stellar.toml's
// [[CURRENCIES]] image field. An API consumer rendering
// `<img src={data.image}>` without further validation must be
// safe on today's browsers; this makes it so.
//
// We don't try to validate the host or fetch the image — that
// would leak outbound requests. Scheme-only check is enough to
// rule out script-execution vectors.
func isSafeImageURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// resolveSACToClassic maps a SAC contract address to the classic (or
// native) asset it wraps, using the lake instance metadata with a
// derivation cross-check: the resolved asset's deterministic SAC
// address must equal the queried contract, so a spoofed metadata name
// can never redirect pricing (defence-in-depth on top of the
// StellarAsset-executable trust anchor).
func (s *Server) resolveSACToClassic(ctx context.Context, contractID string) (canonical.Asset, bool) {
	name, found, err := s.explorer.SACClassicAssetName(ctx, contractID)
	if err != nil || !found {
		return canonical.Asset{}, false
	}
	var asset canonical.Asset
	if name == "native" {
		asset = canonical.NativeAsset()
	} else {
		code, issuer, ok := strings.Cut(name, ":")
		if !ok {
			return canonical.Asset{}, false
		}
		asset, err = canonical.NewClassicAsset(code, issuer)
		if err != nil {
			return canonical.Asset{}, false
		}
	}
	derived, err := asset.SacContractID()
	if err != nil || derived != contractID {
		return canonical.Asset{}, false
	}
	return asset, true
}

// attachSparkline7dIfRequested honours ?include=sparkline7d on the
// unified listing (AM-03: the explorer's directory requested it since
// the coins→assets dissolution and the server silently ignored it —
// a dead chart column on every row). One batch read for the page.
func (s *Server) attachSparkline7dIfRequested(r *http.Request, rows []AssetDetail) {
	if s.coins == nil || !strings.Contains(r.URL.Query().Get("include"), "sparkline7d") {
		return
	}
	ids := make([]string, 0, len(rows))
	for i := range rows {
		if rows[i].AssetID != "" {
			ids = append(ids, rows[i].AssetID)
		}
	}
	if len(ids) == 0 {
		return
	}
	hist, err := s.coins.GetCoinsPriceHistory7dBatch(r.Context(), ids)
	if err != nil {
		s.logger.Warn("sparkline7d batch", "err", err)
		return
	}
	for i := range rows {
		if h, ok := hist[rows[i].AssetID]; ok {
			rows[i].PriceHistory7d = coinPointsToWire(h)
		}
	}
}

// suppressCatalogueTwins drops classic rows whose asset_id belongs to
// a verified catalogue entry — the catalogue row (which now absorbs
// the twin's stats via fillCatalogueStatsForPage) is the single
// canonical listing row (AM-10: USDC appeared twice on page 1 with
// two ranks and slightly different prices, nothing marking them as
// the same entity).
func (s *Server) suppressCatalogueTwins(rows []AssetDetail) []AssetDetail {
	if s.verifiedCurrencies == nil {
		return rows
	}
	out := rows[:0]
	for _, row := range rows {
		if _, dup := s.verifiedCurrencies.LookupByStellarAssetID(row.AssetID); dup {
			continue
		}
		out = append(out, row)
	}
	return out
}

// fillCatalogueStatsForPage merges each catalogue row's Stellar-network
// twin (the classic_assets row for the currency's registered Stellar
// asset) into the row: 24h/1h/7d changes, 24h volume, circulating
// supply and market cap (AM-10 + the catalogue-dash residual: rows
// 1-11 of the unified listing rendered "—" across every analytics
// column while the same asset's classic row two screens down carried
// them all). Bounded fan-out, best-effort per row.
func (s *Server) fillCatalogueStatsForPage(ctx context.Context, page []AssetDetail) {
	if s.coins == nil {
		return
	}
	statsCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	forEachBounded(len(page), readFanoutConcurrency, func(i int) {
		vc, ok := s.verifiedCurrencies.LookupBySlug(page[i].Slug)
		if !ok {
			return
		}
		entry := vc.StellarEntry()
		if entry == nil || entry.AssetID == "" {
			return
		}
		// ListCoinsExt, not GetCoinByAssetID: only the listing query
		// computes the windowed change columns (the per-asset reader's
		// row carries nil changes — live-debugged 2026-07-03 when the
		// deployed enrichment merged nothing). Q= is an exact-enough
		// filter here: the canonical asset_id substring-matches only
		// its own row.
		// Filter by ISSUER, not Q: Q substring-matches code/slug/issuer
		// COLUMN VALUES, so a full asset id (longer than any column)
		// matches nothing — the v0.7.4 attempt still merged nothing
		// live. Issuer is exact; pick the exact asset id from the
		// issuer's (typically 1-row) result.
		twinRow := s.lookupCatalogueTwin(statsCtx, entry.AssetID)
		if twinRow == nil {
			return
		}
		twin := []AssetDetail{assetDetailFromCoinRow(*twinRow)}
		// Same supply-derived market-cap fill the classic phase gets —
		// the raw listing row carries no mcap.
		s.fillMarketCapsFromSupply(statsCtx, twin)
		mergeTwinStats(&page[i], twin[0])
	})
}

// lookupCatalogueTwin resolves a catalogue entry's Stellar asset id to
// its listing row: the dedicated native reader for XLM (no
// classic_assets twin exists), the exact-issuer listing filter for
// classic ids (Q substring-matches column VALUES, so a full asset id
// can never match — the lesson of v0.7.4/v0.7.5). Nil when the twin
// isn't in the served store.
func (s *Server) lookupCatalogueTwin(ctx context.Context, assetID string) *timescale.CoinRow {
	if assetID == "native" {
		row, err := s.coins.GetNativeCoinRow(ctx)
		if err != nil {
			return nil
		}
		return &row
	}
	dashIx := strings.Index(assetID, "-")
	if dashIx < 0 {
		return nil
	}
	rows, err := s.coins.ListCoinsExt(ctx, timescale.ListCoinsOptions{
		Limit:  50,
		Issuer: assetID[dashIx+1:],
		Order:  timescale.CoinsOrderVolume24hUSDDesc,
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
	}
	if dst.CirculatingSupply == nil {
		dst.CirculatingSupply = twin.CirculatingSupply
	}
	if dst.MarketCapUSD == nil {
		dst.MarketCapUSD = twin.MarketCapUSD
	}
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
