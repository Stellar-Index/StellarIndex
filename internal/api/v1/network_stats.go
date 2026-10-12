package v1

import (
	"context"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// NetworkStatsReader is the seam the /v1/network/stats handler reads through; timescale.Store satisfies it.
type NetworkStatsReader interface {
	GetNetworkStats(ctx context.Context) (timescale.NetworkStats, error)
}

// networkStatsStaleReader is the optional capability of a reader that reports each served value's
// freshness (the TTL cache does; the raw store does not). handleNetworkStats then stamps the real
// as_of and flags.stale, so the cache's stale-while-revalidate path cannot assert stale:false
// over a value a failing refresh let age past the TTL.
//
// GetNetworkStatsAt returns the stats, observedAt (fetch time; zero means as_of=now, not stale)
// and stale (past the cache TTL). Mirrors marketsStaleReader.
type networkStatsStaleReader interface {
	GetNetworkStatsAt(ctx context.Context) (timescale.NetworkStats, time.Time, bool, error)
}

// NetworkStats is the /v1/network/stats shape with numeric fields as strings (ADR-0003); its source counts are the static registry, unlike the region-enabled counts on /v1/status.freshness.
type NetworkStats struct {
	Volume24hUSD    *string `json:"volume_24h_usd,omitempty"`
	MarketsCount24h int64   `json:"markets_count_24h"`
	AssetsIndexed   int64   `json:"assets_indexed"`
	LatestLedger    int64   `json:"latest_ledger"`
	ExchangeSources int     `json:"exchange_sources"`
	TotalSources    int     `json:"total_sources"`
}

// handleNetworkStats serves GET /v1/network/stats: the home-page aggregates in one call. One SQL
// query (GetNetworkStats) feeds it; the source counts come from the static external.Registry.
func (s *Server) handleNetworkStats(w http.ResponseWriter, r *http.Request) {
	if s.NetworkStats == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/network-stats-unavailable",
			"Network stats unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the network-stats reader yet.")
		return
	}
	// Stamp flags.stale and the real observation time when the reader reports them; a zero
	// observedAt (uncached read) leaves as_of at now with stale=false.
	var (
		stats      timescale.NetworkStats
		observedAt time.Time
		stale      bool
		err        error
	)
	if sr, ok := s.NetworkStats.(networkStatsStaleReader); ok {
		stats, observedAt, stale, err = sr.GetNetworkStatsAt(r.Context())
	} else {
		stats, err = s.NetworkStats.GetNetworkStats(r.Context())
	}
	if err != nil {
		s.logger.Warn("network stats", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/network-stats-error",
			"Network stats failed", http.StatusInternalServerError,
			"Storage layer returned an error.")
		return
	}

	// Source counts come from the in-memory registry — no DB hit.
	totalSources := 0
	exchangeSources := 0
	for _, md := range external.Registry {
		totalSources++
		if md.Class == external.ClassExchange {
			exchangeSources++
		}
	}

	out := NetworkStats{
		Volume24hUSD:    stats.Volume24hUSD,
		MarketsCount24h: stats.MarketsCount24h,
		AssetsIndexed:   stats.AssetsIndexed,
		LatestLedger:    stats.LatestLedger,
		ExchangeSources: exchangeSources,
		TotalSources:    totalSources,
	}
	env := Envelope{Data: out, Flags: Flags{Stale: stale}}
	if !observedAt.IsZero() {
		env.AsOf = WireTime(observedAt.UTC())
	}
	writeEnvelope(w, env)
}
