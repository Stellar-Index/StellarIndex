package v1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// GlobalAssetView is the wire shape of `/v1/assets/{slug}` for a verified-currency catalogue
// entry: the currency identity plus a headline USD price. Per-issuance Stellar detail lives on
// `/v1/assets/{asset_id}`.
type GlobalAssetView struct {
	// Kind is the wire-shape discriminator for the /v1/assets/{asset_id} oneOf (ADR-0042); always
	// "catalogue" here (contrast AssetDetail.Kind vs AssetDetail.Type).
	Kind string `json:"kind"`

	// ─── Identity (from the verified-currency catalogue) ──────────
	Ticker         string `json:"ticker"`
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Class          string `json:"class"` // crypto / stablecoin / fiat
	VerifiedIssuer string `json:"verified_issuer,omitempty"`
	CoinGeckoID    string `json:"coingecko_id,omitempty"`
	CMCID          string `json:"coinmarketcap_id,omitempty"`

	// ─── Headline price (ComputeGlobalPrice's three-tier fallback chain) ───
	//
	// All four fields are null/empty together when nothing produced a price (typically a
	// Stellar-only token with no CEX or reference-aggregator coverage; see /v1/assets/{asset_id}).
	//
	// This is a GLOBAL (CEX/aggregator) price, not the on-chain closed-bucket VWAP /v1/price
	// serves, so the two are expected to differ. PriceAsOf is the tier's own observation time and
	// the field to read for freshness; for fiat it is a point from a DAILY series, so it can
	// legitimately be days old over a weekend. It is null only under price_authority
	// onchain_listing, whose listing row carries no observation time. The package doc's
	// "Current-price surfaces and their windows" section maps every price surface.
	PriceUSD       *string                  `json:"price_usd,omitempty"`
	PriceAuthority aggregate.PriceAuthority `json:"price_authority,omitempty"`
	PriceSources   []string                 `json:"price_sources,omitempty"`
	PriceAsOf      *WireTime                `json:"price_as_of,omitempty"`

	// ─── Supply + market cap (catalogue-sourced) ──────────────────
	//
	// CirculatingSupply is the natural-unit amount in circulation. For fiat this is M2 broad money
	// from central-bank reporting (`internal/currency/data/seed.yaml`). For crypto / stablecoin
	// classes it is unset; the per-Stellar-asset fields on /v1/assets/{asset_id} are canonical.
	CirculatingSupply *string `json:"circulating_supply,omitempty"`
	// SupplyDecimals exponent maps the integer in CirculatingSupply
	// to a display value. 0 for fiat (raw dollars/yen/yuan).
	SupplyDecimals int `json:"supply_decimals,omitempty"`
	// MarketCapUSD = CirculatingSupply × PriceUSD, a decimal string with 2 fractional digits.
	// Populated for fiat (M2 × FX rate); null when supply or price is unavailable (crypto and
	// stablecoin market cap lives on /v1/assets/{asset_id}).
	MarketCapUSD *string `json:"market_cap_usd,omitempty"`

	// MarketCapLowLiquidity is true when market_cap_usd was deliberately SUPPRESSED (served null)
	// because the backing price came from negligible liquidity: a single venue AND trailing-24h USD
	// volume below aggregate.min_market_cap_volume_usd. It separates "suppressed on purpose" from
	// "no supply/price data" and exists for wire parity with AssetDetail. The fiat market cap
	// (M2 x deep FX rate) is never dust-gated, so it stays false here today.
	MarketCapLowLiquidity bool `json:"market_cap_low_liquidity,omitempty"`
}

// buildGlobalAssetView composes a GlobalAssetView from a catalogue entry; the price block stays
// empty when no global-price reader is wired or all three tiers miss.
func (s *Server) buildGlobalAssetView(ctx context.Context, vc *currency.VerifiedCurrency) GlobalAssetView {
	view := GlobalAssetView{
		Kind:           "catalogue",
		Ticker:         vc.Ticker,
		Slug:           vc.Slug,
		Name:           vc.Name,
		Description:    vc.Description,
		Class:          string(vc.Class),
		VerifiedIssuer: vc.VerifiedIssuerLabel,
		CoinGeckoID:    vc.CoinGeckoID,
		CMCID:          vc.CoinMarketCapID,
	}
	// Catalogue supply: M2 for fiat; empty for crypto/stablecoin, whose supply pipeline is the source.
	if vc.CirculatingSupply != "" {
		supply := vc.CirculatingSupply
		view.CirculatingSupply = &supply
		view.SupplyDecimals = vc.SupplyDecimals
	}

	// Fiat is priced from FX reference rates via [Server.fiatUSDPriceFor], not
	// ComputeGlobalPrice, because fiat:fiat rates are not in the prices_1m CAGG. It runs BEFORE
	// the s.globalPrice nil guard so a deployment with only those readers still prices fiat.
	if vc.Class == currency.ClassFiat {
		return s.populateFiatView(ctx, view, vc)
	}

	// Crypto / stablecoin: try the global CEX/aggregator tier, then fall back to the on-chain
	// per-asset price for Stellar-only tokens the global tier does not cover, so the headline
	// agrees with the /v1/assets listing.
	view = s.populateGlobalCryptoPrice(ctx, view, vc)
	return s.fillGlobalPriceFromOnChain(ctx, view, vc)
}

// populateGlobalCryptoPrice fills the price block for a crypto or stablecoin catalogue entry via
// the three-tier global chain (vwap_native → aggregator_avg → triangulated). It leaves the
// fields nil when no GlobalPriceReader is wired or every tier misses; the caller then applies
// the on-chain fallback.
func (s *Server) populateGlobalCryptoPrice(ctx context.Context, view GlobalAssetView, vc *currency.VerifiedCurrency) GlobalAssetView {
	if s.globalPrice == nil {
		return view
	}

	base, ok := assetForCurrency(vc)
	if !ok {
		return view
	}
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		// "USD" is always on the fiat allow-list; failing here is a code bug.
		s.logger.Error("global asset view: USD asset construction failed", "err", err)
		return view
	}

	opts := s.globalPriceOpts
	if opts.AggregatorSources == nil {
		// No wired source list: leave the aggregator tier disabled rather than fail.
		opts.AggregatorSources = nil
	}

	res, err := aggregate.ComputeGlobalPrice(ctx, base, quote, s.globalPrice, opts)
	if err != nil {
		if errors.Is(err, aggregate.ErrNoPrice) {
			return view // expected miss; leave price fields nil for the on-chain fallback
		}
		s.logger.Warn("global asset view: ComputeGlobalPrice failed",
			"ticker", vc.Ticker, "err", err)
		return view
	}

	// The same substance/scam gate as the /v1/assets listing and /v1/price. This tier has no
	// other gate, so without it a scam-flagged issuer or dust-thin market withheld elsewhere
	// could headline here.
	if withheldBy(ctx, s.Substance, s.Scam, base, quote, "global_asset") != pricingguard.NotWithheld {
		return view
	}

	price := res.Price
	view.PriceUSD = &price
	view.PriceAuthority = res.Authority
	view.PriceSources = res.Sources
	asOf := res.AsOf
	view.PriceAsOf = wireTimePtr(&asOf)
	// Crypto / stablecoin market cap stays on /v1/assets/{asset_id}: the catalogue supply is empty
	// for those classes.
	return view
}

// fillGlobalPriceFromOnChain is the headline-price fallback for a Stellar-only verified token:
// when the global CEX/aggregator tier produced no price, serve the SAME on-chain per-asset USD
// price the /v1/assets listing shows, from the catalogue entry's Stellar-network twin. It is only
// set when the per-asset reader returns a non-nil PriceUSD for the twin asset_id.
//
// Disclosure: authority is AuthorityOnChainListing, not vwap_native, because the listing price
// clears no trade-count floor and no freshness ceiling. price_sources carries a "stellar_onchain"
// marker and price_as_of stays nil (no observation time). No-op when the price is already set, no
// per-asset reader is wired, or the catalogue entry has no Stellar issuance.
func (s *Server) fillGlobalPriceFromOnChain(ctx context.Context, view GlobalAssetView, vc *currency.VerifiedCurrency) GlobalAssetView {
	if view.PriceUSD != nil || s.AssetsReader == nil {
		return view
	}
	se := vc.StellarEntry()
	if se == nil || se.AssetID == "" {
		return view
	}
	price := s.onChainListingPriceUSD(ctx, se.AssetID)
	if price == nil {
		return view
	}
	view.PriceUSD = price
	view.PriceAuthority = aggregate.AuthorityOnChainListing
	view.PriceSources = []string{"stellar_onchain"}
	return view
}

// populateFiatView fills the price and market-cap block for a fiat currency. USD is
// identity-priced; other fiats use the fiat:CCY/fiat:USD rate from [Server.fiatUSDPriceFor].
// The view is returned as-is when no rate is available.
func (s *Server) populateFiatView(ctx context.Context, view GlobalAssetView, vc *currency.VerifiedCurrency) GlobalAssetView {
	if strings.EqualFold(vc.Ticker, "USD") {
		identity := "1.00000000000000"
		asOf := time.Now().UTC()
		view.PriceUSD = &identity
		view.PriceAuthority = aggregate.AuthorityIdentity
		view.PriceSources = []string{"identity"}
		view.PriceAsOf = wireTimePtr(&asOf)
		view.MarketCapUSD = computeFiatMarketCap(vc.CirculatingSupply, vc.SupplyDecimals, identity)
		return view
	}
	// Shared with the listing path ([Server.fiatUSDPriceFor]); PriceReader alone would serve null
	// price_usd / market_cap_usd for every non-USD fiat.
	price, obs, sources, ok := s.fiatUSDPriceFor(ctx, vc.Ticker)
	if !ok {
		return view
	}
	view.PriceUSD = &price
	view.PriceAuthority = aggregate.AuthorityReferenceRate
	view.PriceSources = sources
	view.PriceAsOf = wireTimePtr(&obs)
	view.MarketCapUSD = computeFiatMarketCap(vc.CirculatingSupply, vc.SupplyDecimals, price)
	return view
}

// decimalsCorrectedGlobalReader is where the global view's vwap_native tier becomes a served
// price. [aggregate.GlobalPriceReader] returns the RAW prices_1m quote/base ratio with no
// decimals, so [New] wraps every reader in this: LatestVWAP applies aggregate.AdjustPrice with
// the legs the bucket was read under (the alias ComputeGlobalPrice asked for, not the requested
// spelling). The other two tiers are already whole-unit prices and pass through.
type decimalsCorrectedGlobalReader struct {
	aggregate.GlobalPriceReader
	decimals *NonstandardDecimalsCache
}

// newDecimalsCorrectedGlobalReader wraps r; a nil r stays nil so the "not wired" check holds.
func newDecimalsCorrectedGlobalReader(r aggregate.GlobalPriceReader, decimals *NonstandardDecimalsCache) aggregate.GlobalPriceReader {
	if r == nil {
		return nil
	}
	return decimalsCorrectedGlobalReader{GlobalPriceReader: r, decimals: decimals}
}

// LatestVWAP returns the inner tier-1 VWAP decimals-corrected (byte-identical when the legs share
// a scale). A flagged pair whose ratio cannot be parsed reads as a miss: serving it raw is the defect.
func (g decimalsCorrectedGlobalReader) LatestVWAP(ctx context.Context, base, quote canonical.Asset) (string, time.Time, int64, []string, bool, error) {
	vwap, asOf, tradeCount, sources, ok, err := g.GlobalPriceReader.LatestVWAP(ctx, base, quote)
	if err != nil || !ok {
		return vwap, asOf, tradeCount, sources, ok, err
	}
	baseDec := aggregate.ResolveDecimals(g.decimals, base)
	quoteDec := aggregate.ResolveDecimals(g.decimals, quote)
	if baseDec == quoteDec {
		return vwap, asOf, tradeCount, sources, true, nil
	}
	raw := ratFromDecimal(vwap)
	if raw == nil || raw.Sign() <= 0 {
		return "", time.Time{}, 0, nil, false, nil
	}
	return ratToDecimal(aggregate.AdjustPrice(raw, baseDec, quoteDec), ohlcPriceDigits), asOf, tradeCount, sources, true, nil
}

// assetForCurrency builds the canonical asset to query global pricing for: `fiat:CCY` for fiat
// slugs, `crypto:TICKER` otherwise. Non-allow-listed tickers return ok=false and the caller
// renders the identity without a price block.
func assetForCurrency(vc *currency.VerifiedCurrency) (canonical.Asset, bool) {
	if vc.Class == currency.ClassFiat {
		a, err := canonical.NewFiatAsset(vc.Ticker)
		if err != nil {
			return canonical.Asset{}, false
		}
		return a, true
	}
	return globalBaseForTicker(vc.Ticker)
}

// fiatUSDPriceFor resolves a non-USD fiat's USD price, when it was observed and what produced it.
// It is the single fallback chain shared by the LISTING ([Server.fiatMarketCapUSD]) and DETAIL
// ([Server.populateFiatView]) paths so they cannot drift. PriceReader alone returns
// ErrPriceNotFound for any fiat-quoted request (no on-chain fiat/fiat trades).
//
// Order is load-bearing: fx_quotes first (the authoritative store the forex worker lands in),
// PriceReader only as a last resort for deployments without fx_quotes wiring.
//
// InverseUSD, not RateUSD: rate_usd is UNITS-OF-TICKER PER 1 USD (JPY 163.09), inverse_usd is "1
// unit in USD". Swapping them is a 24,000x error for JPY.
func (s *Server) fiatUSDPriceFor(ctx context.Context, ticker string) (price string, asOf time.Time, sources []string, ok bool) {
	// Path 1: fx_quotes.
	if s.FXHistory != nil {
		now := time.Now().UTC()
		points, err := s.FXHistory.ListFXHistory(ctx, ticker, now.AddDate(0, 0, -7), now)
		if err == nil && len(points) > 0 {
			// oldest→newest; take the latest usable point. Served price is the stored NUMERIC text, never the float.
			for i := len(points) - 1; i >= 0; i-- {
				if _, usable := points[i].inverseUSDRat(); usable {
					// FXQuotePoint has no source label; fx_quotes is Massive-backed.
					return points[i].InverseUSDText, points[i].Bucket, []string{"fx_quotes"}, true
				}
			}
		}
	}
	// Path 2: PriceReader fallback.
	if s.Prices == nil {
		return "", time.Time{}, nil, false
	}
	base, err := canonical.NewFiatAsset(ticker)
	if err != nil {
		return "", time.Time{}, nil, false
	}
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		return "", time.Time{}, nil, false
	}
	snap, srcs, _, err := s.Prices.LatestPrice(ctx, base, quote)
	if err != nil {
		return "", time.Time{}, nil, false
	}
	return snap.Price, snap.ObservedAt.Time(), srcs, true
}

// fiatMarketCapUSD computes market_cap_usd for a fiat catalogue entry. USD is special-cased to
// identity (price = 1.00); every other fiat goes through the FX-rate fallback chain:
//
//  1. fxHistory (fx_quotes): the latest point in a trailing 7-day window, using its InverseUSD as
//     the fiat to USD price. This is authoritative because the continuous forex worker lands there.
//  2. PriceReader.LatestPrice as a last resort. It cannot go first: storePriceReader fast-paths a
//     `quote.Type==fiat` request to ErrPriceNotFound (see cmd/stellarindex-api/main.go), so it
//     never returns a value for fiat to fiat.
//
// Returns nil when neither path returns a price or the supply parse fails.
func (s *Server) fiatMarketCapUSD(ctx context.Context, vc *currency.VerifiedCurrency) *string {
	if vc.CirculatingSupply == "" {
		return nil
	}
	if strings.EqualFold(vc.Ticker, "USD") {
		return computeFiatMarketCap(vc.CirculatingSupply, vc.SupplyDecimals, "1.00000000000000")
	}
	price, _, _, ok := s.fiatUSDPriceFor(ctx, vc.Ticker)
	if !ok {
		return nil
	}
	return computeFiatMarketCap(vc.CirculatingSupply, vc.SupplyDecimals, price)
}

// computeFiatMarketCap returns supplyStr / 10^decimals x priceStr to 2 fractional digits, or nil
// when an operand cannot be parsed. Scaling goes through fiatSupplyWholeUnits so this figure and
// the market-cap chart agree for any catalogue supply_decimals.
func computeFiatMarketCap(supplyStr string, decimals int, priceStr string) *string {
	if supplyStr == "" || priceStr == "" {
		return nil
	}
	// Exact big.Rat, no float64 on served money (ADR-0003): exact even for a supply beyond a
	// float64's 53-bit mantissa.
	supply, ok := fiatSupplyWholeUnits(supplyStr, decimals)
	if !ok {
		return nil
	}
	price, ok := new(big.Rat).SetString(priceStr)
	if !ok {
		return nil
	}
	out := new(big.Rat).Mul(supply, price).FloatString(2)
	return &out
}

// globalBaseForTicker returns the canonical asset to query the global price for. The
// `crypto:<TICKER>` form lets CEX trades and aggregators fill tiers 1 and 2 against one key.
// Non-allow-listed tickers (typically Stellar-only tokens) return ok=false, so their headline
// price is unavailable here and consumers use the Stellar network's deep_link.
func globalBaseForTicker(ticker string) (canonical.Asset, bool) {
	a, err := canonical.NewCryptoAsset(ticker)
	if err != nil {
		return canonical.Asset{}, false
	}
	return a, true
}

// handleGlobalAsset serves the verified-currency global view at `/v1/assets/{slug}`, called by
// `handleAssetGet` when the path matches a catalogue slug.
//
// It always returns 200 with whatever the reader chain produced: `price_usd: null` is the
// legitimate state for a Stellar-only token with no global price.
func (s *Server) handleGlobalAsset(w http.ResponseWriter, r *http.Request, vc *currency.VerifiedCurrency) {
	view := s.buildGlobalAssetView(r.Context(), vc)
	writeJSON(w, view, Flags{})
}

// VerifiedCurrencyListItem is one entry of `GET /v1/assets/verified`, the verified-currency
// catalogue directory listing.
//
// Distinct from [GlobalAssetView] in two ways:
//
//  1. No price block: pricing every catalogue entry per request would multiply storage round-trips
//     by N, so the listing is identity-only; consumers fetch `/v1/assets/{slug}` for pricing.
//  2. Description is omitted to keep payloads small; the detail page carries it.
type VerifiedCurrencyListItem struct {
	Ticker         string `json:"ticker"`
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Class          string `json:"class"`
	VerifiedIssuer string `json:"verified_issuer,omitempty"`
	CoinGeckoID    string `json:"coingecko_id,omitempty"`
	// Image is the logo URL from the issuer's SEP-1 TOML (sanitized, https only); empty when the
	// TOML is missing or has no image for the currency.
	Image             string `json:"image,omitempty"`
	CMCID             string `json:"coinmarketcap_id,omitempty"`
	CirculatingSupply string `json:"circulating_supply,omitempty"`
	SupplyDecimals    int    `json:"supply_decimals,omitempty"`
	// MarketCapUSD is computed for fiat rows only (M2 × current FX rate), with the FX lookups
	// fanned out in parallel. It is empty for crypto and stablecoin rows, whose market cap
	// lives on /v1/assets/{asset_id}; computing it here would multiply storage round-trips.
	MarketCapUSD string `json:"market_cap_usd,omitempty"`
	// AssetID is the canonical Stellar id ("native" or "<code>-<issuer>"); Issuer is credit assets
	// only. Both are empty for entries with no Stellar issuance (fiat).
	AssetID string `json:"asset_id,omitempty"`
	Issuer  string `json:"issuer,omitempty"`
}

// handleAssetsVerified serves GET /v1/assets/verified, the full verified-currency catalogue.
// Order is the seed-file order, which the loader preserves, so it is deterministic. It returns
// 503 when no catalogue is wired and an empty list on a test net, where no issuer exists.
func (s *Server) handleAssetsVerified(w http.ResponseWriter, r *http.Request) {
	if !s.servesPubnetReference() {
		writeJSON(w, []VerifiedCurrencyListItem{}, Flags{})
		return
	}
	if s.VerifiedCurrencies == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/verified-currencies-unavailable",
			"Verified-currency catalogue not wired", http.StatusServiceUnavailable,
			"This deployment hasn't loaded the verified-currency catalogue.")
		return
	}
	entries := s.VerifiedCurrencies.Browseable()
	out := projectVerifiedCurrencyList(entries)
	s.attachFiatMarketCaps(r.Context(), entries, out)
	s.attachVerifiedImages(r.Context(), entries, out)
	writeJSON(w, out, Flags{})
}

// attachVerifiedImages fills each verified row's Image from the issuer's cached SEP-1 currency
// entry. It is bounded by the catalogue size, reads only the sep1_payload cache (no live
// HTTPS) and is best-effort per row. The same isSafeImageURL gate as the detail overlay applies.
func (s *Server) attachVerifiedImages(ctx context.Context, entries []*currency.VerifiedCurrency, out []VerifiedCurrencyListItem) {
	if s.Sep1Cache == nil {
		return
	}
	for i, vc := range entries {
		se := vc.StellarEntry()
		if se == nil || se.Issuer == "" || se.Code == "" {
			continue
		}
		asset, err := canonical.NewClassicAsset(se.Code, se.Issuer)
		if err != nil {
			continue
		}
		sep, err := s.Sep1Cache.GetIssuerSep1Cached(ctx, se.Issuer)
		if err != nil || sep == nil || sep.OutlivedDomain {
			continue
		}
		match := findMatchingCachedCurrency(sep, asset)
		if match == nil {
			continue
		}
		if v := strings.TrimSpace(match.Image); isSafeImageURL(v) {
			out[i].Image = v
		}
	}
}

// projectVerifiedCurrencyList projects each catalogue entry into a VerifiedCurrencyListItem.
func projectVerifiedCurrencyList(entries []*currency.VerifiedCurrency) []VerifiedCurrencyListItem {
	out := make([]VerifiedCurrencyListItem, len(entries))
	for i, vc := range entries {
		out[i] = VerifiedCurrencyListItem{
			Ticker:            vc.Ticker,
			Slug:              vc.Slug,
			Name:              vc.Name,
			Class:             string(vc.Class),
			VerifiedIssuer:    vc.VerifiedIssuerLabel,
			CoinGeckoID:       vc.CoinGeckoID,
			CMCID:             vc.CoinMarketCapID,
			CirculatingSupply: vc.CirculatingSupply,
			SupplyDecimals:    vc.SupplyDecimals,
		}
		out[i].AssetID, out[i].Issuer = verifiedStellarIdentity(vc.StellarEntry())
	}
	return out
}

// verifiedStellarIdentity derives the asset id from the entry's (code, issuer), not the seed's
// asset_id string, so the listing can never serve a malformed or code-only id.
func verifiedStellarIdentity(se *currency.IssuanceEntry) (assetID, issuer string) {
	if se == nil {
		return "", ""
	}
	if se.Code == "" && se.Issuer == "" {
		if se.AssetID == canonical.NativeAsset().String() {
			return se.AssetID, ""
		}
		return "", ""
	}
	a, err := canonical.NewClassicAsset(se.Code, se.Issuer)
	if err != nil {
		return "", ""
	}
	return a.String(), a.Issuer
}

// attachFiatMarketCaps computes market_cap_usd for fiat rows in parallel via fiatMarketCapUSD
// (M2 x FX rate). No-op only when NEITHER rate source is wired: fiatMarketCapUSD tries fxHistory
// first and the USD ticker needs no reader, so gating on `s.Prices == nil` alone would skip every
// fiat market cap on a deployment that wired fxHistory but not PriceReader.
func (s *Server) attachFiatMarketCaps(ctx context.Context, entries []*currency.VerifiedCurrency, out []VerifiedCurrencyListItem) {
	if s.Prices == nil && s.FXHistory == nil {
		return
	}
	forEachBounded(s.logger, len(entries), readFanoutConcurrency, func(i int) {
		vc := entries[i]
		if vc.Class != currency.ClassFiat || vc.CirculatingSupply == "" {
			return
		}
		if capStr := s.fiatMarketCapUSD(ctx, vc); capStr != nil {
			out[i].MarketCapUSD = *capStr
		}
	})
}
