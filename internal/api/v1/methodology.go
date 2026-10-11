package v1

import (
	"net/http"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// Methodology is the wire shape of /v1/methodology, a machine-readable summary of the active
// aggregation policy. Every field maps to a constant in this package, a row in
// `internal/sources/external.Registry`, or an operator config value; nothing is fabricated for
// display. pkg/client mirrors this shape: adding optional fields is compatible, removing or
// renaming needs an SDK minor bump.
type Methodology struct {
	// Version is the shape version of the response; bump on any breaking change.
	Version string `json:"version"`

	// Aggregation describes the VWAP method, outlier filter, stablecoin proxy and closed-bucket
	// contract that govern the served price.
	Aggregation MethodologyAggregation `json:"aggregation"`

	// SourceClasses enumerates EVERY class a row in Sources can carry, and which contribute to the
	// VWAP. "Every" is load-bearing: a missing class leaves sources stamped with a class this
	// document never defines (TestMethodology_EveryServedSourceClassIsDescribed).
	SourceClasses []MethodologySourceClass `json:"source_classes"`

	// Sources is the flat list of registered venues, the same rows as `/v1/sources` without live
	// trade-count stats, so the policy can be verified in one round trip.
	Sources []MethodologySource `json:"sources"`

	// References point at the ADRs governing each section, as repo-relative paths.
	References []MethodologyReference `json:"references"`
}

// MethodologyAggregation is the price-derivation policy slice.
type MethodologyAggregation struct {
	// PriceMethod is the formula for the headline served price; only "vwap" today (TWAP is reserved).
	PriceMethod string `json:"price_method"`

	// OutlierFilter describes the per-endpoint σ-trim applied before aggregation; `default_sigma=0`
	// means no filter by default.
	OutlierFilter MethodologyOutlierFilter `json:"outlier_filter"`

	// StablecoinFiatProxy is the operator's allow-list of classic USD-pegged tokens that the
	// aggregator and the read-time fallback chain treat as proxies for `fiat:USD`. Empty when no
	// pegs are declared (only direct quotes are served).
	StablecoinFiatProxy []MethodologyStablecoinPeg `json:"stablecoin_fiat_proxy"`

	// ClosedBucketWindowSeconds is the granularity rate endpoints snap "now" to, which makes
	// "all regions return the same rate" hold.
	ClosedBucketWindowSeconds int `json:"closed_bucket_window_seconds"`
}

// MethodologyOutlierFilter captures one filter rule.
type MethodologyOutlierFilter struct {
	// Endpoint is the API surface the rule applies to (e.g. "/v1/ohlc"); empty means global.
	Endpoint string `json:"endpoint,omitempty"`

	// DefaultSigma is the σ threshold when the caller sets no `?outlier_sigma=`; 0 disables it.
	DefaultSigma float64 `json:"default_sigma"`

	// Note is free-form context, e.g. why a surface needs an explicit filter.
	Note string `json:"note,omitempty"`
}

// MethodologyStablecoinPeg is one (token → fiat) mapping.
type MethodologyStablecoinPeg struct {
	AssetID string `json:"asset_id"`
	PegsTo  string `json:"pegs_to"`
}

// MethodologySourceClass describes one registry class (exchange, aggregator, oracle, lending, ...).
type MethodologySourceClass struct {
	Name              string `json:"name"`
	ContributesToVWAP bool   `json:"contributes_to_vwap"`
	Description       string `json:"description"`
}

// MethodologySource is one venue, projected from
// `external.Registry`.
type MethodologySource struct {
	Name              string `json:"name"`
	Class             string `json:"class"`
	Subclass          string `json:"subclass,omitempty"`
	DefaultWeight     int    `json:"default_weight"`
	IncludeInVWAP     bool   `json:"include_in_vwap"`
	Paid              bool   `json:"paid"`
	BackfillAvailable bool   `json:"backfill_available"`
	BackfillSafe      bool   `json:"backfill_safe"`
	// OnChain mirrors Source.OnChain: the source observes Stellar directly, not an off-chain vendor API.
	OnChain bool `json:"on_chain"`
}

// MethodologyReference is a pointer to an ADR or other narrative
// doc that governs one slice of the policy.
type MethodologyReference struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// methodologyVersion is the shape version; bump on breaking changes only, not new optional fields.
const methodologyVersion = "1.0"

// handleMethodology serves GET /v1/methodology from constants, the in-memory registry and
// [Options.USDPeggedClassics]; no DB call. Content changes only on deploy or config reload, so it
// is safe to cache aggressively.
func (s *Server) handleMethodology(w http.ResponseWriter, r *http.Request) {
	pegs := make([]MethodologyStablecoinPeg, 0, len(s.USDPeggedClassics))
	for _, peg := range s.USDPeggedClassics {
		pegs = append(pegs, MethodologyStablecoinPeg{
			AssetID: peg.String(),
			PegsTo:  canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}.String(),
		})
	}

	// One entry per class the registry can stamp, including those with no price signal: `sources` is
	// the whole registry, so every `class` value must be defined here. framework.go explains each exclusion.
	classes := []MethodologySourceClass{
		{Name: string(external.ClassExchange), ContributesToVWAP: true, Description: "Real trading venues — DEXes (Soroswap, Phoenix, Aquarius, Comet, sdex), CEXes (Coinbase, Binance, Kraken, Bitstamp), FX vendors. The only sources that contribute to the VWAP."},
		{Name: string(external.ClassAggregator), ContributesToVWAP: false, Description: "Third-party aggregators (CoinGecko, CoinMarketCap) that already aggregate the same upstream venues. Including them in our VWAP would double-count; surfaced separately for divergence checks."},
		{Name: string(external.ClassOracle), ContributesToVWAP: false, Description: "Reflector, Band, Redstone, Chainlink. Each runs its own methodology; adding their output to our VWAP would impose theirs on top of ours. Surfaced as parallel readings + used for cross-checks."},
		{Name: string(external.ClassAuthoritySanity), ContributesToVWAP: false, Description: "Stellar-blessed reference points (anchor home-domains, canonical fiat rates) used as sanity bounds, not price input."},
		{Name: string(external.ClassLending), ContributesToVWAP: false, Description: "On-chain lending protocols (Blend, SoroCredit). Their events are directional state changes — supply, borrow, liquidation auction, bad debt — taken on top of some other oracle's price rather than new price observations. Surfaced as a secondary validation and protocol-health surface."},
		{Name: string(external.ClassRouter), ContributesToVWAP: false, Description: "Soroban DEX routers and aggregator vaults (soroswap-router, defindex, upshift). They emit no independent trades — they invoke the contracts that do — so counting them would double-count the underlying pool's swap. Indexed for per-transaction attribution (which router drove this swap) and requested-vs-realised path."},
		{Name: string(external.ClassBridge), ContributesToVWAP: false, Description: "Cross-chain transfer protocols (Circle CCTP, Rozo). These move tokens between chains rather than exchanging them at a price: a deposit_for_burn on Stellar plus a mint_and_withdraw elsewhere is one logical transfer, not a two-leg trade. Surfaced for cross-chain flow attribution and USDC supply accounting."},
	}

	names := make([]string, 0, len(external.Registry))
	for name := range external.Registry {
		names = append(names, name)
	}
	sort.Strings(names)

	sources := make([]MethodologySource, 0, len(names))
	for _, name := range names {
		md := external.Registry[name]
		sources = append(sources, MethodologySource{
			Name:              name,
			Class:             string(md.Class),
			Subclass:          string(md.Subclass),
			DefaultWeight:     md.DefaultWeight,
			IncludeInVWAP:     md.IncludeInVWAP,
			Paid:              md.Paid,
			BackfillAvailable: md.BackfillAvailable,
			BackfillSafe:      md.BackfillSafe(),
			OnChain:           external.IsOnChain(name),
		})
	}

	out := Methodology{
		Version: methodologyVersion,
		Aggregation: MethodologyAggregation{
			PriceMethod: "vwap",
			OutlierFilter: MethodologyOutlierFilter{
				Endpoint:     "/v1/ohlc",
				DefaultSigma: ohlcDefaultOutlierSigma,
				// Weighting does not dampen outliers: VWAP is Σquote/Σbase, so an attacker supplying the
				// QUOTE asset dominates the numerator, and a lone print carries its whole TWAP hour.
				Note: "OHLC's High/Low have no statistical robustness; a single dust trade can pin them. The same default sigma applies to /v1/twap, whose time weight is unrelated to trade size. /v1/vwap defaults to 0, i.e. UNFILTERED. Neither volume-weighting nor time-weighting is an outlier defence: on a sparse window a single print can carry the whole result, and on VWAP an attacker supplying the quote asset dominates the sum. Pass outlier_sigma explicitly on /v1/vwap if you need filtering; pass outlier_sigma=0 on /v1/ohlc or /v1/twap to disable it.",
			},
			StablecoinFiatProxy:       pegs,
			ClosedBucketWindowSeconds: int(closedBucketWindow.Seconds()),
		},
		SourceClasses: classes,
		Sources:       sources,
		// Titles are the ADRs' own titles verbatim (TestMethodology_ReferenceTitlesMatchTheADRs): a
		// paraphrase can name a document that does not exist.
		References: []MethodologyReference{
			{ID: "ADR-0007", Title: "Redis as hot-path cache + rate-limit + ephemeral state", URL: "/research/adr/0007"},
			{ID: "ADR-0015", Title: "API rates served from last-closed bucket, never in-progress", URL: "/research/adr/0015"},
			{ID: "ADR-0018", Title: "API consistency surfaces — closed-bucket, tip, and observations", URL: "/research/adr/0018"},
			{ID: "ADR-0019", Title: "Anomaly response policy and confidence scoring — per-asset statistical baselines", URL: "/research/adr/0019"},
			{ID: "ADR-0020", Title: "Chart API contract — timeframe + granularity + price_type", URL: "/research/adr/0020"},
			{ID: "ADR-0026", Title: "Stablecoin → fiat proxy is late-binding aggregator policy, not eager ingest normalisation", URL: "/research/adr/0026"},
		},
	}

	writeJSON(w, out, Flags{})
}
