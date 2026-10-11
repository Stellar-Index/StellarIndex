package v1

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// parseFloatOr0 parses a numeric string for ratio maths (the share
// split), returning 0 on any parse failure. Precision-lossy by design —
// used ONLY for the cosmetic SharePct percentage, never for a served
// amount (those stay stringified per ADR-0003).
func parseFloatOr0(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// MarketSourceReader is the seam the /v1/markets/sources handler reads through; timescale.Store
// satisfies it. Each asset argument is the full set of canonical FORMS to match (expanded via
// assetAliases), so XLM's `native` (SDEX) and `crypto:XLM` (CEX) legs aggregate into one
// per-source breakdown instead of undercounting whichever form the query named.
type MarketSourceReader interface {
	PairSourceStats(ctx context.Context, base, quote []string) ([]timescale.SourceStats, error)
	AssetSourceStats(ctx context.Context, asset []string) ([]timescale.SourceStats, error)
}

// SourceVolume is one source's trailing-24h contribution to a market (or asset). VolumeUSD24h
// is the sum of trade-time usd_volume as a decimal string (ADR-0003), omitted when no trade
// was priced. VolumeLowerBound is true when unpriced trades were excluded. SharePct is the
// source's share of total priced USD volume (0 when the total is unknown).
type SourceVolume struct {
	Source           string  `json:"source"`
	VolumeUSD24h     *string `json:"volume_24h_usd,omitempty"`
	VolumeLowerBound bool    `json:"volume_lower_bound,omitempty"`
	TradeCount24h    int64   `json:"trade_count_24h"`
	SharePct         float64 `json:"share_pct"`
}

// MarketSourcesResp is the wire shape for /v1/markets/sources. Exactly
// one of {base+quote} or {asset} is echoed back depending on the query.
type MarketSourcesResp struct {
	Base       string         `json:"base,omitempty"`
	Quote      string         `json:"quote,omitempty"`
	Asset      string         `json:"asset,omitempty"`
	WindowSecs int            `json:"window_secs"`
	Sources    []SourceVolume `json:"sources"`
}

// handleMarketSources serves GET /v1/markets/sources.
//
// Returns the trailing-24h USD-volume and trade-count breakdown by source for a market pair
// (?base=&quote=) or an asset across every pair (?asset=). /v1/history only samples recent
// trades, so an accurate 24h share needs this server-side aggregate.
//
// Volume derivation matches /v1/sources?include=stats: trade-time usd_volume only, never
// today's XLM price; a source with unpriced trades is flagged volume_lower_bound, and one with
// no priced trade still appears with its trade count and a null volume.
func (s *Server) handleMarketSources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	base := q.Get("base")
	quote := q.Get("quote")
	asset := q.Get("asset")

	if asset != "" && (base != "" || quote != "") {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/conflicting-filters",
			"Conflicting filters", http.StatusBadRequest,
			"pass either asset=<id> OR base=<id>&quote=<id>, not both.")
		return
	}
	if asset == "" && (base == "" || quote == "") {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-params",
			"Missing parameters", http.StatusBadRequest,
			"provide base=<id>&quote=<id> for a pair, or asset=<id> for an asset.")
		return
	}

	if s.MarketSources == nil {
		// Not wired — empty list is consistent with the contract.
		writeJSON(w, MarketSourcesResp{Base: base, Quote: quote, Asset: asset, WindowSecs: 86_400, Sources: []SourceVolume{}}, Flags{})
		return
	}

	// 8s ceiling on the per-source aggregate, matching the other aggregation endpoints. Derived
	// from r.Context() so a client hang-up cancels early; the deadline is the resource bound.
	msCtx, msCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer msCancel()

	var (
		rows []timescale.SourceStats
		err  error
	)
	if asset != "" {
		rows, err = s.MarketSources.AssetSourceStats(msCtx, sourceStatsAliases(asset))
	} else {
		rows, err = s.MarketSources.PairSourceStats(msCtx,
			sourceStatsAliases(base), sourceStatsAliases(quote))
	}
	if err != nil {
		s.writeMarketSourcesError(w, r, msCtx, err)
		return
	}

	// Total derivable USD volume across sources, for the share split.
	var total float64
	for _, ss := range rows {
		if ss.VolumeUSD24h.Valid {
			total += parseFloatOr0(ss.VolumeUSD24h.String)
		}
	}

	out := MarketSourcesResp{
		Base:       base,
		Quote:      quote,
		Asset:      asset,
		WindowSecs: 86_400,
		Sources:    make([]SourceVolume, 0, len(rows)),
	}
	for _, ss := range rows {
		sv := SourceVolume{
			Source:           ss.Source,
			TradeCount24h:    ss.TradeCount24h,
			VolumeLowerBound: ss.UnpricedTrades24h > 0,
		}
		if ss.VolumeUSD24h.Valid {
			v := ss.VolumeUSD24h.String
			sv.VolumeUSD24h = &v
			if total > 0 {
				sv.SharePct = parseFloatOr0(v) / total * 100
			}
		}
		out.Sources = append(out.Sources, sv)
	}
	writeJSON(w, out, Flags{})
}

// writeMarketSourcesError reports a /v1/markets/sources storage failure:
// silent on a client disconnect, a retryable 503 naming the endpoint on
// the handler's own 8s budget expiring, otherwise a plain 500.
func (s *Server) writeMarketSourcesError(w http.ResponseWriter, r *http.Request, msCtx context.Context, err error) {
	if clientAborted(r, err) {
		return
	}
	if handlerTimedOut(msCtx, err) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/market-sources-timeout",
			"Market sources query timed out", http.StatusServiceUnavailable,
			"the underlying trades-hypertable scan didn't return in 8s. Try again shortly.")
		return
	}
	s.logger.Warn("market sources", "err", err)
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/market-sources-error",
		"Market sources failed", http.StatusInternalServerError,
		"Storage layer returned an error.")
}

// sourceStatsAliases expands a raw asset_id query param into every canonical FORM the
// per-source aggregate should match, via [canonical.AssetAliases]. XLM is the multi-form case:
// SDEX writes `native`, CEXes write `crypto:XLM`, Soroban AMMs write the SAC C-address, so one
// form alone undercounts the volume-by-source split.
//
// Membership and valuation must agree: the per-source volume CASE values the XLM SAC leg at
// the XLM/USD rate, so a filter without the SAC would drop Soroban XLM trades that are valued
// as XLM. The SQL binds [canonical.NativeSACContractID], the same SAC this expansion emits.
//
// It falls back to the literal id when the param does not parse, giving a single-form query
// rather than an error.
func sourceStatsAliases(id string) []string {
	a, err := canonical.ParseAsset(id)
	if err != nil {
		return []string{id}
	}
	return canonical.AssetAliasStrings(a)
}
