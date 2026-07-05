package v1

import (
	"context"
	"net/http"
	"strconv"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
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

// MarketSourceReader is the seam the /v1/markets/sources handler reads
// through. timescale.Store satisfies it via PairSourceStats /
// AssetSourceStats. Each asset argument is the full set of canonical
// FORMS to match (the handler expands via assetAliases), so a
// multi-form asset's legs — XLM's `native` (SDEX) and `crypto:XLM`
// (CEX) — aggregate into one per-source breakdown instead of
// undercounting whichever form the query named.
type MarketSourceReader interface {
	PairSourceStats(ctx context.Context, base, quote []string) ([]timescale.SourceStats, error)
	AssetSourceStats(ctx context.Context, asset []string) ([]timescale.SourceStats, error)
}

// SourceVolume is one source's trailing-24h contribution to a market
// (or asset). VolumeUSD24h is stringified per ADR-0003; omitted when no
// trade in the window carried a derivable USD volume (e.g. a pure
// SEP-41/SEP-41 pair with no XLM leg). SharePct is this source's share
// of the total derivable USD volume across all sources (0 when the
// total is unknown).
type SourceVolume struct {
	Source        string  `json:"source"`
	VolumeUSD24h  *string `json:"volume_24h_usd,omitempty"`
	TradeCount24h int64   `json:"trade_count_24h"`
	SharePct      float64 `json:"share_pct"`
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
// Returns the trailing-24h USD-volume + trade-count breakdown grouped
// by source for either a single market pair (?base=&quote=) or an
// asset across every pair it appears in (?asset=). Backs the
// volume-by-source pie on the market-pair + asset pages — the
// /v1/history feed only samples recent trades, so an accurate 24h
// share needs this server-side aggregate.
//
// Volume derivation matches /v1/sources?include=stats (XLM/USD fallback
// for native / XLM-SAC legs); sources whose trades carry no derivable
// USD volume still appear with their trade count and a null volume.
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

	if s.marketSources == nil {
		// Not wired — empty list is consistent with the contract.
		writeJSON(w, MarketSourcesResp{Base: base, Quote: quote, Asset: asset, WindowSecs: 86_400, Sources: []SourceVolume{}}, Flags{})
		return
	}

	var (
		rows []timescale.SourceStats
		err  error
	)
	if asset != "" {
		rows, err = s.marketSources.AssetSourceStats(r.Context(), sourceStatsAliases(asset))
	} else {
		rows, err = s.marketSources.PairSourceStats(r.Context(),
			sourceStatsAliases(base), sourceStatsAliases(quote))
	}
	if err != nil {
		s.logger.Warn("market sources", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/market-sources-error",
			"Market sources failed", http.StatusInternalServerError,
			"Storage layer returned an error.")
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
		sv := SourceVolume{Source: ss.Source, TradeCount24h: ss.TradeCount24h}
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

// sourceStatsAliases expands a raw asset_id query param into every
// canonical FORM the per-source aggregate should match, reusing the
// price path's assetAliases (rc.89). XLM is the live multi-form case:
// SDEX writes `native`, every CEX writes `crypto:XLM`, so filtering on
// one form alone undercounts the market's volume-by-source split.
// Falls back to the literal id when it doesn't parse as a canonical
// asset, so a malformed param still produces a (single-form) query
// rather than an error.
func sourceStatsAliases(id string) []string {
	a, err := canonical.ParseAsset(id)
	if err != nil {
		return []string{id}
	}
	forms := assetAliases(a)
	out := make([]string, 0, len(forms))
	for _, f := range forms {
		out = append(out, f.String())
	}
	return out
}
