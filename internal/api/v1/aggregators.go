package v1

import (
	"context"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// AggregatorsReader is the seam the /v1/aggregators handler reads through; timescale.Store satisfies it.
type AggregatorsReader interface {
	// AggregatorRollup returns every routers-registry entry
	// (migration 0025) joined with its routed-trade stats for
	// trades whose ts >= since.
	AggregatorRollup(ctx context.Context, since time.Time) ([]timescale.AggregatorRollupRow, error)
}

// AggregatorRow is one /v1/aggregators entry; RoutedVolume24hUSD is a decimal string, null (never "0") when no routed trade carried a USD valuation.
type AggregatorRow struct {
	ContractID     string `json:"contract_id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"` // "router" | "aggregator-vault"
	Protocol       string `json:"protocol"`
	AutoDiscovered bool   `json:"auto_discovered"`

	RoutedTrades24h    int64     `json:"routed_trades_24h"`
	RoutedVolume24hUSD *string   `json:"routed_volume_24h_usd"`
	LastRoutedAt       *WireTime `json:"last_routed_at"`

	// Notes are honest coverage caveats for THIS row's stats — same
	// idiom as ProtocolBespoke.Notes. Server-computed from Kind /
	// AutoDiscovered, not stored; absent when the row needs none.
	Notes []string `json:"notes,omitempty"`
}

// aggregatorRowNotes returns coverage caveats for a registry row: auto_discovered rows are evidence-observed, not
// audit-verified, and router rows merge direct and wrapped calls under one name (timescale.TagTradesRoutedVia).
func aggregatorRowNotes(kind string, autoDiscovered bool) []string {
	switch {
	case kind != "router":
		return nil
	case autoDiscovered:
		return []string{
			"Evidence-observed contract, not vendor- or WASM-audit-verified " +
				"(see the registry seed migration's notes).",
		}
	default:
		return []string{
			"routed_trades_24h combines direct calls to this router and calls " +
				"wrapped by an aggregator this registry doesn't recognise — the " +
				"two can't be told apart yet.",
		}
	}
}

// handleAggregators serves GET /v1/aggregators with routed-via attribution (trades, USD volume) over a rolling 24h recomputed
// per request, NOT closed buckets: treat like /v1/network/stats, not /v1/vwap. Attribution goes to the outermost registered
// 'router' wrapper, else the plain router. Test nets serve an empty list.
func (s *Server) handleAggregators(w http.ResponseWriter, r *http.Request) {
	if !s.servesPubnetReference() {
		writeJSON(w, []AggregatorRow{}, Flags{})
		return
	}
	if s.Aggregators == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/aggregators-unavailable",
			"Aggregators listing unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the aggregators reader yet.")
		return
	}

	rows, err := s.Aggregators.AggregatorRollup(r.Context(), time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Warn("aggregator rollup", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/aggregators-error",
			"Aggregators rollup failed", http.StatusInternalServerError,
			"Storage layer returned an error.")
		return
	}

	out := make([]AggregatorRow, len(rows))
	for i, row := range rows {
		out[i] = AggregatorRow{
			ContractID:         row.ContractID,
			Name:               row.Name,
			Kind:               row.Kind,
			Protocol:           row.ProtocolSlug,
			AutoDiscovered:     row.AutoDiscovered,
			RoutedTrades24h:    row.RoutedTrades,
			RoutedVolume24hUSD: row.RoutedVolume,
			LastRoutedAt:       wireTimePtr(row.LastRoutedAt),
			Notes:              aggregatorRowNotes(row.Kind, row.AutoDiscovered),
		}
	}
	writeJSON(w, out, Flags{})
}
