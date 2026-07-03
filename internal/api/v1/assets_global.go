package v1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/StellarIndex/stellar-index/internal/aggregate"
	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/currency"
)

// GlobalAssetView is the wire shape served by `/v1/assets/{slug}`
// when `{slug}` resolves to a verified-currency catalogue entry —
// the catalogue identity (USDC the currency) plus a headline USD
// price. Per-issuance Stellar detail (USDC-GA5Z... on Stellar)
// lives on the canonical `/v1/assets/{asset_id}` surface.
type GlobalAssetView struct {
	// ─── Identity (from the verified-currency catalogue) ──────────
	Ticker         string `json:"ticker"`
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Class          string `json:"class"` // crypto / stablecoin / fiat
	VerifiedIssuer string `json:"verified_issuer,omitempty"`
	CoinGeckoID    string `json:"coingecko_id,omitempty"`
	CMCID          string `json:"coinmarketcap_id,omitempty"`

	// ─── Headline price (from ComputeGlobalPrice's three-tier
	// fallback chain, R-018 Phase 1.3a) ─────────────────────────────
	//
	// All four fields are null/empty together when no tier produced
	// a price (typically a Stellar-only token like AQUA where neither
	// CEX nor reference-aggregator coverage exists — consumers
	// should drill into the canonical /v1/assets/{asset_id} surface
	// to reach the Stellar-issued price).
	PriceUSD       *string                  `json:"price_usd,omitempty"`
	PriceAuthority aggregate.PriceAuthority `json:"price_authority,omitempty"`
	PriceSources   []string                 `json:"price_sources,omitempty"`
	PriceAsOf      *time.Time               `json:"price_as_of,omitempty"`

	// ─── Supply + market cap (catalogue-sourced) ──────────────────
	//
	// CirculatingSupply is the natural-unit amount in circulation.
	// For fiat this is M2 (broad money: cash + checkable deposits
	// + savings + money-market funds), populated from central-bank
	// reporting via `internal/currency/data/seed.yaml`. For crypto /
	// stablecoin classes the catalogue field is unset today; the
	// per-Stellar-asset F2 fields on /v1/assets/{asset_id} are the
	// canonical source.
	CirculatingSupply *string `json:"circulating_supply,omitempty"`
	// SupplyDecimals exponent maps the integer in CirculatingSupply
	// to a display value. 0 for fiat (raw dollars/yen/yuan).
	SupplyDecimals int `json:"supply_decimals,omitempty"`
	// MarketCapUSD = CirculatingSupply × PriceUSD when both are
	// available. Decimal string with 2 fractional digits. Populated
	// for fiat (M2 × FX rate); null when supply or price is
	// unavailable (crypto/stablecoin market cap lives on the
	// per-Stellar-asset /v1/assets/{asset_id} F2 fields).
	MarketCapUSD *string `json:"market_cap_usd,omitempty"`
}

// buildGlobalAssetView composes a GlobalAssetView from a catalogue
// entry. The price block stays empty when no global-price reader is
// wired or all three tiers come up empty.
func (s *Server) buildGlobalAssetView(ctx context.Context, vc *currency.VerifiedCurrency) GlobalAssetView {
	view := GlobalAssetView{
		Ticker:         vc.Ticker,
		Slug:           vc.Slug,
		Name:           vc.Name,
		Description:    vc.Description,
		Class:          string(vc.Class),
		VerifiedIssuer: vc.VerifiedIssuerLabel,
		CoinGeckoID:    vc.CoinGeckoID,
		CMCID:          vc.CoinMarketCapID,
	}
	// Catalogue-sourced supply (M2 for fiat; usually empty for
	// crypto/stablecoin — the F2 supply pipeline is the source of
	// truth there).
	if vc.CirculatingSupply != "" {
		supply := vc.CirculatingSupply
		view.CirculatingSupply = &supply
		view.SupplyDecimals = vc.SupplyDecimals
	}

	// Fiat goes through PriceReader (s.prices) — different reader
	// from crypto/stablecoin's ComputeGlobalPrice because fiat:fiat
	// FX rates live in the aggregator's Redis triangulated cache,
	// not in the prices_1m CAGG. See fiatMarketCapUSD's docstring
	// for the full rationale. Handled BEFORE the s.globalPrice nil
	// guard so a deployment with only PriceReader wired still
	// populates fiat fully.
	if vc.Class == currency.ClassFiat {
		return s.populateFiatView(ctx, view, vc)
	}

	// Populate the price block via the three-tier fallback chain
	// (vwap_native → aggregator_avg → triangulated). Skipped when
	// the binary didn't wire a GlobalPriceReader, leaving the price
	// fields nil — consumers fall back to the canonical
	// /v1/assets/{asset_id} surface.
	if s.globalPrice == nil {
		return view
	}

	base, ok := assetForCurrency(vc)
	if !ok {
		return view
	}
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		// "USD" must be on the canonical-fiat allow-list; failing here
		// is a code bug rather than a runtime data issue.
		s.logger.Error("global asset view: USD asset construction failed", "err", err)
		return view
	}

	opts := s.globalPriceOpts
	if opts.AggregatorSources == nil {
		// Leave aggregator tier disabled if the binary didn't wire a
		// source list — better to skip than fail.
		opts.AggregatorSources = nil
	}

	res, err := aggregate.ComputeGlobalPrice(ctx, base, quote, s.globalPrice, opts)
	if err != nil {
		if errors.Is(err, aggregate.ErrNoPrice) {
			return view // expected miss; leave price fields nil
		}
		s.logger.Warn("global asset view: ComputeGlobalPrice failed",
			"ticker", vc.Ticker, "err", err)
		return view
	}

	price := res.Price
	view.PriceUSD = &price
	view.PriceAuthority = res.Authority
	view.PriceSources = res.Sources
	asOf := res.AsOf
	view.PriceAsOf = &asOf
	// Crypto / stablecoin market cap stays on the per-asset surface
	// (/v1/assets/{asset_id}'s F2 fields) — catalogue.CirculatingSupply
	// is empty for those classes, so no inline computation here.
	// Fiat already returned above with both price + cap populated.
	return view
}

// populateFiatView fills the price + market-cap block for a fiat
// currency. USD is identity-priced; other fiats consult
// PriceReader.LatestPrice for the fiat:CCY/fiat:USD rate. Returns
// the view as-is when the rate isn't available (PriceReader nil,
// rate not found, ticker not in canonical allow-list).
func (s *Server) populateFiatView(ctx context.Context, view GlobalAssetView, vc *currency.VerifiedCurrency) GlobalAssetView {
	if strings.EqualFold(vc.Ticker, "USD") {
		identity := "1.00000000000000"
		asOf := time.Now().UTC()
		view.PriceUSD = &identity
		view.PriceAuthority = aggregate.AuthorityVWAPNative
		view.PriceSources = []string{"identity"}
		view.PriceAsOf = &asOf
		view.MarketCapUSD = computeFiatMarketCap(vc.CirculatingSupply, identity)
		return view
	}
	if s.prices == nil {
		return view
	}
	base, err := canonical.NewFiatAsset(vc.Ticker)
	if err != nil {
		return view
	}
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		return view
	}
	snap, sources, _, err := s.prices.LatestPrice(ctx, base, quote)
	if err != nil {
		return view
	}
	price := snap.Price
	view.PriceUSD = &price
	view.PriceAuthority = aggregate.AuthorityVWAPNative
	view.PriceSources = sources
	obs := snap.ObservedAt
	view.PriceAsOf = &obs
	view.MarketCapUSD = computeFiatMarketCap(vc.CirculatingSupply, price)
	return view
}

// assetForCurrency builds the canonical asset to query global pricing
// for. Fiat slugs map to a `fiat:CCY` asset (FX feeds populate
// prices_1m for these pairs). Crypto / stablecoin slugs map to
// `crypto:TICKER`. Non-allow-listed tickers return ok=false; the
// caller renders the catalogue identity without a price block.
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

// fiatMarketCapUSD computes market_cap_usd for a fiat catalogue
// entry. USD is special-cased to identity (price = 1.00); every
// other fiat goes through the FX-rate fallback chain:
//
//  1. fxHistory (fx_quotes table — Frankfurter-backed daily ECB
//     reference rates back to 1999). Reads the latest point in a
//     trailing 7-day window and uses its InverseUSD as the
//     fiat→USD price. This is the authoritative path because
//     fx_quotes is where the Frankfurter backfill + the
//     continuous forex worker both land.
//  2. PriceReader.LatestPrice as a last resort. Pre-fix the
//     ordering was reversed: PriceReader was tried first and
//     storePriceReader fast-paths a `quote.Type==fiat` request to
//     ErrPriceNotFound (see cmd/stellarindex-api/main.go:1935),
//     so this layer never returned a value for fiat→fiat. With
//     fx_quotes tried first, the 19 catalogue fiats with a
//     populated circulating_supply now all get a market_cap_usd
//     (verified on r1 with EUR/CNY/JPY/GBP/CAD/CHF/…).
//
// Returns nil when neither path returns a price or the supply
// parse fails.
func (s *Server) fiatMarketCapUSD(ctx context.Context, vc *currency.VerifiedCurrency) *string {
	if vc.CirculatingSupply == "" {
		return nil
	}
	if strings.EqualFold(vc.Ticker, "USD") {
		return computeFiatMarketCap(vc.CirculatingSupply, "1.00000000000000")
	}
	// Path 1: fx_quotes (authoritative for fiat→USD rates).
	if s.fxHistory != nil {
		now := time.Now().UTC()
		from := now.AddDate(0, 0, -7)
		points, err := s.fxHistory.ListFXHistory(ctx, vc.Ticker, from, now)
		if err == nil && len(points) > 0 {
			// ListFXHistory returns oldest→newest; take the most
			// recent point with a usable InverseUSD.
			for i := len(points) - 1; i >= 0; i-- {
				if points[i].InverseUSD > 0 {
					price := strconv.FormatFloat(points[i].InverseUSD, 'f', -1, 64)
					return computeFiatMarketCap(vc.CirculatingSupply, price)
				}
			}
		}
	}
	// Path 2: PriceReader fallback (kept for deployments without
	// fx_quotes wiring — e.g. test fixtures).
	if s.prices == nil {
		return nil
	}
	base, err := canonical.NewFiatAsset(vc.Ticker)
	if err != nil {
		return nil
	}
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		return nil
	}
	snap, _, _, err := s.prices.LatestPrice(ctx, base, quote)
	if err != nil {
		return nil
	}
	return computeFiatMarketCap(vc.CirculatingSupply, snap.Price)
}

// computeFiatMarketCap returns market_cap_usd = supplyStr × priceStr
// formatted to 2 fractional digits, or nil when either operand can't
// be parsed as a decimal. supplyStr is the natural-unit amount
// (raw yen/yuan/etc.; supply_decimals = 0 for fiat).
func computeFiatMarketCap(supplyStr, priceStr string) *string {
	if supplyStr == "" || priceStr == "" {
		return nil
	}
	supply, ok := new(big.Float).SetPrec(128).SetString(supplyStr)
	if !ok {
		return nil
	}
	price, ok := new(big.Float).SetPrec(128).SetString(priceStr)
	if !ok {
		return nil
	}
	product := new(big.Float).SetPrec(128).Mul(supply, price)
	out := product.Text('f', 2)
	return &out
}

// globalBaseForTicker returns the canonical asset to query the
// global price for. We use the `crypto:<TICKER>` form so the
// existing CEX trades (Binance / Coinbase / Kraken / Bitstamp) and
// aggregators (CG / CMC) populate tier 1 + tier 2 against the same
// canonical key. Non-allow-listed tickers (typically Stellar-only
// tokens we haven't added to the crypto allow-list) return ok=false
// — those tokens' headline price isn't available via the global
// view; consumers drill into the Stellar network's deep_link.
func globalBaseForTicker(ticker string) (canonical.Asset, bool) {
	a, err := canonical.NewCryptoAsset(ticker)
	if err != nil {
		return canonical.Asset{}, false
	}
	return a, true
}

// handleGlobalAsset serves the verified-currency global view at
// `/v1/assets/{slug}`. Called by `handleAssetGet` when the path
// parameter matches a slug in the verified-currency catalogue.
//
// Always returns 200 with whatever data the reader chain produced
// — a global view with `price_usd: null` is the legitimate state
// for a Stellar-only token whose price we don't aggregate at the
// global level. Consumers drill into `/v1/assets/{asset_id}` for
// the per-Stellar-asset surface.
func (s *Server) handleGlobalAsset(w http.ResponseWriter, r *http.Request, vc *currency.VerifiedCurrency) {
	view := s.buildGlobalAssetView(r.Context(), vc)
	writeJSON(w, view, Flags{})
}

// VerifiedCurrencyListItem is one entry in the response to
// `GET /v1/assets/verified` — the verified-currency catalogue
// directory listing.
//
// Distinct from [GlobalAssetView] in two ways:
//
//  1. No price block. Computing per-currency global prices for
//     every catalogue entry on every request would N-multiply the
//     storage round-trips for a directory listing; the listing
//     surface is identity-only. Consumers fetch
//     `/v1/assets/{slug}` per row when they need pricing.
//  2. Description is omitted to keep payloads small — the detail
//     page already surfaces it.
type VerifiedCurrencyListItem struct {
	Ticker         string `json:"ticker"`
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Class          string `json:"class"`
	VerifiedIssuer string `json:"verified_issuer,omitempty"`
	CoinGeckoID    string `json:"coingecko_id,omitempty"`
	// Image is the asset's logo URL from the issuer's SEP-1 TOML
	// (sanitized; https only). Wallets bulk-load logos from this
	// listing (board #47). Empty when the issuer's TOML is missing
	// or carries no image for the matched currency.
	Image             string `json:"image,omitempty"`
	CMCID             string `json:"coinmarketcap_id,omitempty"`
	CirculatingSupply string `json:"circulating_supply,omitempty"`
	SupplyDecimals    int    `json:"supply_decimals,omitempty"`
	// MarketCapUSD is computed for fiat rows only — M2 × current
	// FX rate. Empty string for crypto/stablecoin rows (their market
	// cap lives on /v1/assets/{asset_id}'s per-Stellar-asset F2
	// fields; computing it inline here would N-multiply storage
	// round-trips on the listing). Fiat fan-out is parallel; ~19
	// FX lookups happen concurrently per request.
	MarketCapUSD string `json:"market_cap_usd,omitempty"`
}

// handleAssetsVerified serves GET /v1/assets/verified — the full
// verified-currency catalogue as a directory listing. Drives the
// explorer's "verified currencies" section at the top of the
// /assets page (R-018 Phase 1.5d).
//
// Order matches the seed-file order (deterministic; the catalogue
// loader preserves entry order). 503 when no catalogue is wired.
func (s *Server) handleAssetsVerified(w http.ResponseWriter, r *http.Request) {
	if s.verifiedCurrencies == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/verified-currencies-unavailable",
			"Verified-currency catalogue not wired", http.StatusServiceUnavailable,
			"This deployment hasn't loaded the verified-currency catalogue.")
		return
	}
	entries := s.verifiedCurrencies.Browseable()
	out := projectVerifiedCurrencyList(entries)
	s.attachFiatMarketCaps(r.Context(), entries, out)
	s.attachVerifiedImages(r.Context(), entries, out)
	writeJSON(w, out, Flags{})
}

// attachVerifiedImages fills each verified row's Image from the
// issuer's cached SEP-1 currency entry (board #47 — wallets bulk-load
// logos from this listing). Bounded by the catalogue size (~50 rows);
// reads only the sep1_payload cache (no live HTTPS), best-effort per
// row. The same isSafeImageURL gate as the detail overlay applies.
func (s *Server) attachVerifiedImages(ctx context.Context, entries []*currency.VerifiedCurrency, out []VerifiedCurrencyListItem) {
	if s.sep1Cache == nil {
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
		sep, err := s.sep1Cache.GetIssuerSep1Cached(ctx, se.Issuer)
		if err != nil || sep == nil {
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

// projectVerifiedCurrencyList projects each catalogue entry into
// VerifiedCurrencyListItem. Extracted from handleAssetsVerified
// to keep the handler under the gocognit ceiling.
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
	}
	return out
}

// attachFiatMarketCaps computes market_cap_usd for fiat rows in
// parallel via fiatMarketCapUSD (M2 × FX rate). No-op when no
// PriceReader is wired.
func (s *Server) attachFiatMarketCaps(ctx context.Context, entries []*currency.VerifiedCurrency, out []VerifiedCurrencyListItem) {
	if s.prices == nil {
		return
	}
	forEachBounded(len(entries), readFanoutConcurrency, func(i int) {
		vc := entries[i]
		if vc.Class != currency.ClassFiat || vc.CirculatingSupply == "" {
			return
		}
		if capStr := s.fiatMarketCapUSD(ctx, vc); capStr != nil {
			out[i].MarketCapUSD = *capStr
		}
	})
}
