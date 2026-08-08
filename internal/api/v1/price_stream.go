package v1

import (
	"net/http"
	"strconv"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// PriceStreamTopic returns the Hub topic key for closed-bucket events
// on a given (asset, quote, window) triple. Exported so the publishers
// (the redispub subscriber bridging the aggregator, and streampublish)
// compute the same key when calling Hub.Publish — keeps the wire
// string in one place.
//
// Format: `closed:<asset>/<quote>/<window_seconds>` using canonical
// asset strings. The window is part of the key (cold audit 2026-08-03
// finding 1, r1-confirmed): the aggregator publishes one closed bucket
// per (pair, window) — r1 runs [5m, 1h, 24h] — and pre-fix all three
// landed on ONE topic, so a subscriber reading value_decimal saw the
// price flap across three window lengths every tick. One topic per
// window restores "consecutive events on a topic are comparable".
//
// The "closed:" prefix lets the same Hub multiplex tip / observations
// fanout without topic-key collisions (the tip stream uses "tip:").
func PriceStreamTopic(asset, quote canonical.Asset, windowSeconds int) string {
	return "closed:" + asset.String() + "/" + quote.String() + "/" + strconv.Itoa(windowSeconds)
}

// Closed-bucket stream window bounds. The DEFAULT matches the
// aggregator's smallest default window (5m) — the highest-frequency
// closed-bucket surface. The bound mirrors redispub's sanity ceiling
// (positive, under a year) rather than an allowlist: operators can
// reconfigure `[aggregate].windows`, and the API can't see that
// config, so an unknown-but-sane window simply subscribes to a topic
// that may stay quiet.
const (
	defaultClosedWindowSeconds = 300
	maxClosedWindowSeconds     = 366 * 24 * 60 * 60
)

// parseClosedWindowSeconds reads the optional window_seconds query
// param for /v1/price/stream. Distinct from parseTipWindowSeconds:
// tip windows are rolling-compute bounds clamped to [1, 60]; closed
// windows name which aggregator bucket series to follow.
func parseClosedWindowSeconds(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("window_seconds")
	if raw == "" {
		return defaultClosedWindowSeconds, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > maxClosedWindowSeconds {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-window",
			"Invalid window_seconds", http.StatusBadRequest,
			"window_seconds must be a positive integer naming an aggregation window (default 300; the standard windows are 300, 3600, and 86400)")
		return 0, false
	}
	return n, true
}

// handlePriceStream serves GET /v1/price/stream — the SSE endpoint
// carrying the strict ADR-0015 closed-bucket consistency contract
// that /v1/price serves. Unlike the tip + observations streams (per-
// connection tick), this surface is Hub-driven: the aggregator
// publishes one event per closed bucket, and every subscriber on
// the same (asset, quote) topic receives the same byte-identical
// payload — the same cross-region consistency property that
// /v1/price itself exposes.
//
// Wire shape:
//
//   - On connect: SSE headers, optional buffered-replay from
//     `Last-Event-ID` (Hub maintains a per-topic ring buffer).
//   - Per closed bucket: one `price_update` event with the same
//     envelope shape as a `/v1/price` response.
//   - Heartbeats every 15 s as comment lines.
//
// Pre-flight 503: when no Hub is wired (typical pre-launch state
// where the aggregator isn't running yet), the endpoint returns
// 503 — same posture as the other Hub-dependent surfaces.
func (s *Server) handlePriceStream(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/stream-unavailable",
			"Closed-bucket stream not configured", http.StatusServiceUnavailable,
			"this deployment has no streaming Hub wired — typically because the aggregator publish path isn't running yet")
		return
	}

	// Reuse the parameter parser from the request endpoint — same
	// asset+quote contract, same default fiat:USD quote.
	asset, quote, ok := s.parseTipAssetQuote(w, r)
	if !ok {
		return
	}

	// URL discipline: the closed-bucket surface accepts no granularity
	// query param — bucket series are selected by ?window_seconds=
	// (per ADR-0015's per-window contract), not a granularity concept.
	if r.URL.Query().Get("granularity") != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-stream-param",
			"granularity is not valid on /v1/price/stream", http.StatusBadRequest,
			"the closed-bucket stream selects its aggregation window via window_seconds (default 300); use /v1/history/since-inception for chart granularities")
		return
	}

	window, ok := parseClosedWindowSeconds(w, r)
	if !ok {
		return
	}

	// Alias fan-out (cold audit 2026-08-03 finding 2): the aggregator
	// publishes under whichever canonical spelling it computed —
	// `crypto:XLM/fiat:USD` for the CEX-fed XLM VWAP — so a client
	// subscribing as `?asset=native` got a healthy 200 and zero frames
	// forever. Subscribe to every alias spelling of the pair; the same
	// assetAliases loop every REST read path already runs.
	var topics []string
	for _, a := range assetAliases(asset) {
		for _, q := range assetAliases(quote) {
			topics = append(topics, PriceStreamTopic(a, q, window))
		}
	}
	streaming.Stream(w, r, s.hub, topics, streaming.StreamOptions{})
}
