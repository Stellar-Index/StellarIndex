package v1

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// parseFloatOr0 returns 0 on parse failure and is precision-lossy: for the cosmetic SharePct only, never a served amount (ADR-0003).
func parseFloatOr0(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// MarketSourceReader is the seam for /v1/markets/sources; each asset argument is the full alias-expanded set of canonical forms so XLM's native and crypto:XLM legs aggregate together.
type MarketSourceReader interface {
	PairSourceStats(ctx context.Context, base, quote []string) ([]timescale.SourceStats, error)
	AssetSourceStats(ctx context.Context, asset []string) ([]timescale.SourceStats, error)
}

// SourceVolume is one source's trailing-24h market contribution; VolumeUSD24h is a decimal string omitted when unpriced, with VolumeLowerBound true when unpriced trades were excluded.
type SourceVolume struct {
	Source           string  `json:"source"`
	VolumeUSD24h     *string `json:"volume_24h_usd,omitempty"`
	VolumeLowerBound bool    `json:"volume_lower_bound,omitempty"`
	TradeCount24h    int64   `json:"trade_count_24h"`
	SharePct         float64 `json:"share_pct"`
}

// MarketSourcesResp is the /v1/markets/sources shape; exactly one of {base+quote} or {asset} is echoed back.
type MarketSourcesResp struct {
	Base       string         `json:"base,omitempty"`
	Quote      string         `json:"quote,omitempty"`
	Asset      string         `json:"asset,omitempty"`
	WindowSecs int            `json:"window_secs"`
	Sources    []SourceVolume `json:"sources"`
}

// handleMarketSources serves GET /v1/markets/sources: trailing-24h USD volume and trade count per source for ?base=&quote= or ?asset=.
// Volume is trade-time usd_volume only (as /v1/sources?include=stats); unpriced trades set volume_lower_bound, none priced gives null.
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

// writeMarketSourcesError is silent on client disconnect, a retryable 503 naming the endpoint on the handler's 8s budget, else 500.
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

// sourceStatsAliases expands an asset_id param to every form via [canonical.AssetAliases] (native, crypto:XLM, SAC); one form
// undercounts.
// The SQL binds [canonical.NativeSACContractID], so the filter must include that SAC or valued Soroban XLM trades drop. Unparsable ids
// pass through.
func sourceStatsAliases(id string) []string {
	a, err := canonical.ParseAsset(id)
	if err != nil {
		return []string{id}
	}
	return canonical.AssetAliasStrings(a)
}
