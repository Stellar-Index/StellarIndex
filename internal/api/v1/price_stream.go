package v1

import (
	"context"
	"net/http"
	"strconv"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
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

// closedStreamGateSurface is this surface's low-cardinality label on
// obs.PriceServe{Scam,Substance}WithheldTotal. A distinct constant, not
// a reuse of "price_read" or "tip": an operator seeing a step-change
// needs to know WHICH surface withheld, and this one withholds on a
// fan-out cadence (one consultation per closed bucket per connection)
// that has nothing to do with request traffic on the reader seam.
const closedStreamGateSurface = "price_stream"

// closedStreamGateBudget bounds ONE withholding consultation on this
// surface — both the connect-time pre-flight and each per-event
// re-check below.
//
// A budget is mandatory, not tidiness: /stream paths are deliberately
// excluded from the request-timeout middleware and r1 runs
// statement_timeout = 0, so without one a Postgres stall pins the
// handler goroutine AND its pool connection until the client
// disconnects — the cold-audit-2026-08-04 shape that took the tip
// stream's pre-flights out.
//
// Deliberately GENEROUS (it matches the tip stream's pre-flight budget,
// [tipStreamTickTimeout]) rather than tuned down. Both gates fail OPEN
// on a timed-out consultation — pricingguard's documented posture, so a
// DB blip cannot blank every price at once — which means a budget set
// too tight would silently re-open the very hole this gate closes.
// Verdicts are TTL-cached per pair inside the gates and closed buckets
// arrive at most once per window (300 s minimum), so the steady-state
// cost of the generous budget is nil; only a genuinely stalled DB pays
// it, and it pays in latency rather than in a re-exposed price.
const closedStreamGateBudget = tipStreamTickTimeout

// closedStreamWithheld reports whether an aggregated closed-bucket
// price claim for (asset, quote) may be published on this surface.
//
// Both gates, never one. /v1/price consults substance AND scam through
// cmd/stellarindex-api's priceWithheld chokepoint, and a hand-written
// site that consults one and forgets the other is exactly the MSP-07
// drift that chokepoint exists to prevent (see
// TestWithholdingGatesAreSpelledOnlyAtTheChokepoint): an operator
// setting disable_substance_gate=true to diagnose a coverage complaint
// would otherwise start fanning out a directory-flagged issuer's VWAP.
//
// Keyed on the requested (asset, quote): the substance gate measures
// the pair's ALIAS UNION and the scam gate resolves each leg to its
// canonical family form internally, so one consultation covers every
// alias spelling this connection subscribes to.
//
// BOTH LEGS on the scam side too, via [scamWithheld]. This line used to
// spell `s.scam.Withheld(ctx, asset, ...)` — the base-only question —
// so a flagged issuer named as the QUOTE opened the stream at 200 and
// was fanned its own market's price, inverted, once per closed bucket
// for the hours an SSE connection lives, while the same issuer named as
// the asset was refused (F002/K001). The fold over legs belongs inside
// pricingguard, never hand-written at a call site.
//
// The scam gate is asked first so a pair both gates refuse is reported
// under the flag, not as a thin market (see [writePriceWithheldProblem]).
//
// Nil gates (operator disabled [pricing_guard]) withhold nothing.
func (s *Server) closedStreamWithheld(ctx context.Context, asset, quote canonical.Asset) pricingguard.Withholding {
	if s.substance == nil && s.scam == nil {
		return pricingguard.NotWithheld
	}
	ctx, cancel := context.WithTimeout(ctx, closedStreamGateBudget)
	defer cancel()
	return withheldBy(ctx, s.substance, s.scam, asset, quote, closedStreamGateSurface)
}

// forwardClosedStream bridges the Hub subscription onto the SSE writer
// channel, re-consulting the withholding gates for every event before
// it reaches the wire. Returns (closing ch so the SSE writer ends
// cleanly) when the request context cancels or the Hub subscription
// closes.
//
// The per-event re-check is the half of this gate that matters. The
// producer is the AGGREGATOR: it publishes a closed bucket for every
// pair it computes, with no pricingguard consultation anywhere on that
// path, so this handler is the only place the verdict can be applied.
// A connect-time check alone would gate new subscribers while every
// connection opened BEFORE an issuer was flagged — including the
// attacker's own — kept receiving the attacker-authored rate for the
// life of an SSE connection, which is hours. That is the 2026-08-04
// valuation-incident class surviving the fix meant to close it.
//
// A withheld bucket is DROPPED, not turned into a stream error: the tip
// stream's shared producer behaves identically (a withheld tick emits
// nothing and the connection heartbeats on), and closing the stream
// would send every EventSource into a reconnect storm against a pair
// whose verdict may well flip back at the next TTL expiry.
func (s *Server) forwardClosedStream(
	ctx context.Context,
	ch chan<- streaming.Event,
	sub <-chan streaming.Event,
	asset, quote canonical.Asset,
) {
	defer s.recoverStreamProducer("price_stream")
	defer close(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-sub:
			if !open {
				return
			}
			if s.closedStreamWithheld(ctx, asset, quote) != pricingguard.NotWithheld {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case ch <- ev:
			}
		}
	}
}

// closedStreamQueueDepth is the forwarder→writer hand-off buffer, the
// same shallow depth the tip stream's producer hand-off uses: closed
// buckets arrive at most once per window, so anything deeper would only
// stage staler prices behind a slow client.
const closedStreamQueueDepth = 4

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
//
// Pre-flight 404 `errors/price-withheld`: the same consistency
// contract carries the same WITHHOLDING contract. Until 2026-09-18
// this surface fanned out, unfiltered, prices that /v1/price answers
// 404 for — the aggregator publishes a closed bucket for every pair it
// computes, the Redis→Hub bridge sanitises the envelope but asks no
// gate, and this handler asked none either, so a thin dust market's
// attacker-authored VWAP and a directory-scam-flagged issuer's price
// were both available in real time to anyone who spelled the pair into
// a query string. Both gates are applied here, at connect AND on every
// forwarded bucket (see [Server.forwardClosedStream]).
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

	// Admission BEFORE the gate consultation, and before Hub.Subscribe.
	// The gate reads the DB, so an unauthenticated flood must be bounded
	// by the stream caps before it can spend a pool connection (the
	// REL-05 reason the tip stream pre-admits too); and the topic key is
	// client-supplied, so a refused connection must never mint one.
	// Idempotent and deferred here so every return path releases exactly
	// once — the stream itself is handed off via
	// StreamFromChannelPreAdmitted below rather than taking a second
	// slot.
	release, ok := streaming.TryAcquireStreamSlot(w, r)
	if !ok {
		return
	}
	defer release()

	// Withholding pre-flight, while a non-200 status can still be set:
	// once the SSE headers go out it is too late to say 404. Same verdict
	// and same problem type as /v1/price and /v1/price/tip/stream.
	if withheld := s.closedStreamWithheld(r.Context(), asset, quote); withheld != pricingguard.NotWithheld {
		writePriceWithheldProblem(w, r, asset, quote, withheldReasonFor(withheld))
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
	sub, cancelSub := s.hub.Subscribe(topics, streaming.LastEventIDFrom(r))
	defer cancelSub()

	ch := make(chan streaming.Event, closedStreamQueueDepth)
	go s.forwardClosedStream(r.Context(), ch, sub, asset, quote)

	streaming.StreamFromChannelPreAdmitted(w, r, ch, s.streamOptions())
}
