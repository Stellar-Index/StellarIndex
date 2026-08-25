package v1

import (
	"context"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// NetworkStatsReader is the seam the /v1/network/stats handler
// reads through. timescale.Store satisfies via GetNetworkStats.
type NetworkStatsReader interface {
	GetNetworkStats(ctx context.Context) (timescale.NetworkStats, error)
}

// networkStatsStaleReader is the optional capability a NetworkStatsReader
// exposes when it can report each served value's freshness — the TTL cache
// (CachedNetworkStatsReader) implements it; the raw store does not. When a
// reader satisfies it, handleNetworkStats stamps an honest as_of (the
// served data's real observation time) and flags.stale, instead of the
// cache's SWR stale-serve path silently asserting stale:false / as_of=now
// over a value a failing refresh has let age past the TTL (REC-05).
//
// GetNetworkStatsAt returns the stats, observedAt (the served value's fetch
// time; zero → as_of=now, not stale), and stale (the served value is past
// the cache TTL). Mirrors marketsStaleReader.
type networkStatsStaleReader interface {
	GetNetworkStatsAt(ctx context.Context) (timescale.NetworkStats, time.Time, bool, error)
}

// NetworkStats is the wire shape for /v1/network/stats. Numeric
// fields are stringified per ADR-0003 (volume can exceed int64
// in raw cents).
//
// Source-count semantics — important and easy to confuse with
// /v1/status.freshness.{active,total}_sources:
//
//   - ExchangeSources / TotalSources here count entries in the
//     static `internal/sources/external.Registry` map — i.e.,
//     "sources the binary knows how to read." Independent of
//     operator config; constant across regions running the same
//     build.
//   - /v1/status.freshness.total_sources counts sources the
//     operator has ENABLED in their region config (Prometheus
//     `count(stellarindex_source_enabled == 1)`); a strict subset.
//   - /v1/status.freshness.active_sources further narrows to
//     sources that have emitted an event in the last 10 minutes.
//
// On r1 today: Registry=21, enabled=17, active=15. The gap
// between the two `total_sources` fields is by design (different
// metrics) — kept in separate envelopes so the names don't collide
// in any single response.
type NetworkStats struct {
	Volume24hUSD    *string `json:"volume_24h_usd,omitempty"`
	MarketsCount24h int64   `json:"markets_count_24h"`
	AssetsIndexed   int64   `json:"assets_indexed"`
	LatestLedger    int64   `json:"latest_ledger"`
	ExchangeSources int     `json:"exchange_sources"`
	TotalSources    int     `json:"total_sources"`
}

// handleNetworkStats serves GET /v1/network/stats.
//
// Returns the home-page aggregate stats — total 24h USD volume,
// active markets count, classic-assets-indexed count, latest live
// ledger, exchange-class source count. Single SQL query under the
// hood (`GetNetworkStats`); the source counts are derived from
// the static `external.Registry` map, not the DB.
//
// Designed to back the explorer's home network strip in one HTTP
// call instead of fanning out to /v1/coins, /v1/markets,
// /v1/sources, /v1/diagnostics/cursors separately.
func (s *Server) handleNetworkStats(w http.ResponseWriter, r *http.Request) {
	if s.networkStats == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/network-stats-unavailable",
			"Network stats unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the network-stats reader yet.")
		return
	}
	// Honest freshness: when the reader can report it (the TTL cache does;
	// the raw store doesn't), stamp flags.stale + the served value's real
	// observation time. A zero observedAt (live/uncached read) leaves
	// as_of to default to now with stale=false — truthful and unchanged.
	var (
		stats      timescale.NetworkStats
		observedAt time.Time
		stale      bool
		err        error
	)
	if sr, ok := s.networkStats.(networkStatsStaleReader); ok {
		stats, observedAt, stale, err = sr.GetNetworkStatsAt(r.Context())
	} else {
		stats, err = s.networkStats.GetNetworkStats(r.Context())
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
		env.AsOf = observedAt.UTC()
	}
	writeEnvelope(w, env)
}
