package v1

import (
	"context"
	"net/http"
	"time"

	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// AggregatorsReader is the seam the /v1/aggregators handler reads
// through. timescale.Store satisfies it via AggregatorRollup.
type AggregatorsReader interface {
	// AggregatorRollup returns every routers-registry entry
	// (migration 0025) joined with its routed-trade stats for
	// trades whose ts >= since.
	AggregatorRollup(ctx context.Context, since time.Time) ([]timescale.AggregatorRollupRow, error)
}

// AggregatorRow is the wire shape for one /v1/aggregators entry: a
// routers-registry row (router or aggregator-vault contract) plus
// its routed-via attribution rollup over the trailing 24 h.
//
// RoutedVolume24hUSD is a decimal string (ADR-0003), null when none
// of the window's routed trades carried a USD valuation (usd_volume
// is aggregator-backfilled and can lag) — distinct from "0", which
// never appears (a zero-trade router reports routed_trades_24h=0
// and a null volume). Vault-kind rows always report zero routed
// trades today: per-tx routed_via tagging applies to kind='router'
// only; vault capital state lives on the protocol surfaces.
type AggregatorRow struct {
	ContractID     string `json:"contract_id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"` // "router" | "aggregator-vault"
	Protocol       string `json:"protocol"`
	AutoDiscovered bool   `json:"auto_discovered"`

	RoutedTrades24h    int64      `json:"routed_trades_24h"`
	RoutedVolume24hUSD *string    `json:"routed_volume_24h_usd"`
	LastRoutedAt       *time.Time `json:"last_routed_at"`
}

// handleAggregators serves GET /v1/aggregators.
//
// Lists the routers registry (Soroswap router, DeFindex vaults, …)
// with each entry's routed-via attribution over the trailing 24 h:
// how many trades — and how much USD volume — arrived at the
// underlying venues via that router. The window is a rolling
// observation over the trades hypertable (recomputed per request,
// cheap via the partial routed_via index), NOT a closed-bucket
// series — treat the numbers like /v1/network/stats, not /v1/vwap.
func (s *Server) handleAggregators(w http.ResponseWriter, r *http.Request) {
	if s.aggregators == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/aggregators-unavailable",
			"Aggregators listing unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the aggregators reader yet.")
		return
	}

	rows, err := s.aggregators.AggregatorRollup(r.Context(), time.Now().UTC().Add(-24*time.Hour))
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
			LastRoutedAt:       row.LastRoutedAt,
		}
	}
	writeJSON(w, out, Flags{})
}
