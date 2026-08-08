package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// tipStreamProducerQueueDepth is the capacity of the per-connection
// channel between the producer goroutine and the SSE writer. 4 is
// enough for the writer to fall a tick or two behind without the
// producer blocking, while small enough that a wedged writer is
// detected by the producer (next channel send blocks → ctx cancel
// signals teardown).
const tipStreamProducerQueueDepth = 4

// tipStreamTickTimeout bounds a single per-tick computeTip call
// (REL-01, partial fix of G2-04). RequestTimeout deliberately excludes
// `/stream` paths (the connection is long-lived by design), so without
// a per-tick bound a slow tip computation could hold this producer's
// goroutine — and whatever DB connection it's using — open
// indefinitely, once per open connection. Mirrors observationsScanTimeout's
// 8s ceiling, the same fix already applied to the observations-stream
// producer.
const tipStreamTickTimeout = 8 * time.Second

// handlePriceTipStream serves GET /v1/price/tip/stream — the SSE
// counterpart to /v1/price/tip per ADR-0018 §"SSE stream wires onto
// the tip surface" and the Wk-7 plan row L3.7.
//
// Wire shape per connection:
//
//   - Headers: Content-Type: text/event-stream + Cache-Control:
//     no-cache + X-Accel-Buffering: no (set by the streaming.Stream
//     scaffolding).
//   - Initial event: a tip_update emitted as soon as the first
//     compute completes (so the client doesn't sit on a heartbeat-only
//     stream when data is already available).
//   - Recurring events: every window_seconds (default 5, clamp 1–60),
//     a fresh tip computation runs and a tip_update event fires when
//     it succeeds. Failures (transient hypertable error, no data) are
//     logged and silently skipped — the client sees heartbeats keep
//     the connection alive until data returns.
//   - Heartbeats: every streaming.DefaultHeartbeatInterval (15 s) when
//     no real event has flowed.
//
// Pre-stream errors (param validation, "no data ever" 404) are
// returned as standard problem+json with the right HTTP status —
// after the stream body starts there's no way to set status, so
// failures must be detected pre-flight.
func (s *Server) handlePriceTipStream(w http.ResponseWriter, r *http.Request) {
	// REL-05: admit against the concurrency caps FIRST, before the
	// synchronous pre-flight compute below (computeTip) runs. Without
	// this, a client already at its stream cap still paid for the full
	// pre-flight tip computation before being rejected. release is
	// idempotent and deferred here so every return path releases
	// exactly once; the stream is handed off via
	// StreamFromChannelPreAdmitted below so it doesn't also acquire a
	// second slot.
	release, ok := streaming.TryAcquireStreamSlot(w, r)
	if !ok {
		return
	}
	defer release()

	if s.prices == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price serving not configured", http.StatusServiceUnavailable,
			"this deployment has no PriceReader wired — check binary configuration")
		return
	}

	// URL-discipline rule: tip URL never accepts closed-bucket params.
	if r.URL.Query().Get("granularity") != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-tip-param",
			"granularity is not valid on /v1/price/tip/stream", http.StatusBadRequest,
			"granularity is a closed-bucket concept (ADR-0018); /v1/price/tip and /v1/price/tip/stream do not have granularities")
		return
	}

	asset, quote, ok := s.parseTipAssetQuote(w, r)
	if !ok {
		return
	}
	window, ok := parseTipWindowSeconds(w, r)
	if !ok {
		return
	}

	// First synchronous compute — gives us a chance to return 404
	// before switching the response into SSE mode (where it's too
	// late to set a non-200 status code).
	//
	// BOUNDED, like the tick path. This pre-flight runs on the handler
	// goroutine under the raw request context, and /stream paths are
	// deliberately excluded from the request-timeout middleware — so
	// before this it had no deadline ANYWHERE: no middleware, no
	// per-call timeout, and r1 runs statement_timeout = 0. A slow query
	// during a Postgres stall pinned the handler goroutine AND its pool
	// connection until the client disconnected, and WriteTimeout does
	// not cancel an in-flight query. 20 such connections from one IP
	// (the shipped per-IP cap) plus a second IP exhausts the 25-conn
	// pool, and the held connections never self-release — an outage
	// that outlives the hiccup that caused it. These two pre-flights
	// were the only handler paths in the API with no deadline at all
	// (cold audit 2026-08-04).
	preflightCtx, cancelPreflight := context.WithTimeout(r.Context(), tipStreamTickTimeout)
	defer cancelPreflight()
	first, firstSources, err := s.computeTip(preflightCtx, asset, quote, window)
	if errors.Is(err, ErrPriceWithheld) {
		// Substance-gated pair: the stream cannot start — same verdict
		// and problem type as the request endpoint.
		writePriceWithheldProblem(w, r, asset, quote)
		return
	}
	if errors.Is(err, ErrPriceNotFound) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-not-found",
			"No price data for pair", http.StatusNotFound,
			"no trades or oracle observations for "+asset.String()+" / "+quote.String())
		return
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if IsCacheUnavailable(err) {
			s.logger.Warn("computeTip cache unavailable (stream prelude)",
				"err", err, "asset", asset.String(), "quote", quote.String())
			writeCacheUnavailableProblem(w, r)
			return
		}
		s.logger.Error("computeTip failed (stream prelude)",
			"err", err, "asset", asset.String(), "quote", quote.String())
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// We have a valid first event + an open response. Switch to SSE.
	//
	// Two producer shapes (RT-1, audit 2026-08-04 "tip stream = 6 DB
	// queries/s PER CONNECTION"):
	//
	//   - Hub-wired deployments (production): ONE shared producer per
	//     distinct (asset, quote, window) publishes into the Hub; this
	//     connection is a plain subscriber. Steady-state DB cost scales
	//     with distinct pairs being watched, not with viewers. The
	//     pre-flight snapshot computed above is still emitted as this
	//     connection's first frame so the page paints instantly even
	//     when it joins mid-tick; a near-duplicate tip_update from the
	//     shared producer is harmless (idempotent state update).
	//   - Hub-less deployments (tests, minimal binaries): the legacy
	//     per-connection tick loop.
	if s.hub != nil {
		// The producer's context is DELIBERATELY detached from r.Context()
		// (contextcheck): the shared compute loop outlives any single
		// connection — it stops via the registry's refcount + linger, not
		// via this request's cancellation.
		topic, releaseProducer := s.acquireTipProducer(asset, quote, window) //nolint:contextcheck
		defer releaseProducer()
		sub, cancelSub := s.hub.Subscribe([]string{topic}, streaming.LastEventIDFrom(r))
		defer cancelSub()

		var gen streaming.Generator
		firstEv, _ := tipStreamEvent(&gen, first, firstSources)
		ch := make(chan streaming.Event, tipStreamProducerQueueDepth)
		go s.forwardTipStream(r.Context(), ch, sub, firstEv)
		streaming.StreamFromChannelPreAdmitted(w, r, ch, streaming.StreamOptions{})
		return
	}

	var gen streaming.Generator
	ch := make(chan streaming.Event, tipStreamProducerQueueDepth)
	prodCtx, cancelProd := context.WithCancel(r.Context())
	defer cancelProd()

	go s.runTipStreamProducer(prodCtx, ch, &gen, asset, quote, window, first, firstSources)

	streaming.StreamFromChannelPreAdmitted(w, r, ch, streaming.StreamOptions{})
}

// forwardTipStream bridges a Hub subscription onto the SSE writer
// channel, prepending the connection's own pre-flight snapshot so the
// first frame never waits for the shared producer's next tick. Returns
// (closing ch so the SSE writer ends cleanly) when the request context
// cancels or the Hub subscription closes (hub shutdown / topic evict —
// the client's EventSource auto-reconnects and lands on a fresh
// subscription).
func (s *Server) forwardTipStream(
	ctx context.Context,
	ch chan<- streaming.Event,
	sub <-chan streaming.Event,
	firstEv streaming.Event,
) {
	defer s.recoverStreamProducer("price_tip")
	defer close(ch)

	if len(firstEv.Data) > 0 {
		select {
		case <-ctx.Done():
			return
		case ch <- firstEv:
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-sub:
			if !open {
				return
			}
			select {
			case <-ctx.Done():
				return
			case ch <- ev:
			}
		}
	}
}

// runTipStreamProducer is the per-connection compute + push loop.
// Emits the pre-computed initial event, then ticks every
// `windowSeconds` recomputing the tip price. Failures are silently
// skipped (heartbeats keep the connection alive) — the assumption
// is that transient unavailability resolves itself and the next
// tick will succeed.
//
// The function returns when ctx cancels (client disconnect, request
// teardown) and closes ch on the way out so [streaming.StreamFromChannel]
// returns cleanly.
func (s *Server) runTipStreamProducer(
	ctx context.Context,
	ch chan<- streaming.Event,
	gen *streaming.Generator,
	asset, quote canonical.Asset,
	windowSeconds int,
	first PriceSnapshot,
	firstSources []string,
) {
	// AGT-12 (audit-2026-07-24): this producer runs in its OWN goroutine, so an
	// unrecovered panic in the compute path below terminates the WHOLE process —
	// middleware.Recoverer only wraps the handler goroutine, not this one, and the
	// stream is reachable unauthenticated. Recover here so a panic tears down only
	// this connection. Registered BEFORE `defer close(ch)` so that close runs FIRST
	// (defers are LIFO): the SSE writer sees the channel close and ends the response
	// cleanly, then this logs. Never swallow it silently — a crash turning into an
	// invisible dropped connection is its own bug.
	defer s.recoverStreamProducer("price_tip")
	defer close(ch)

	if firstEv, ok := tipStreamEvent(gen, first, firstSources); ok {
		select {
		case <-ctx.Done():
			return
		case ch <- firstEv:
		}
	}

	ticker := time.NewTicker(time.Duration(windowSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tickCtx, cancel := context.WithTimeout(ctx, tipStreamTickTimeout)
			snap, sources, err := s.computeTip(tickCtx, asset, quote, windowSeconds)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("computeTip failed (stream tick) — skipping emit",
						"err", err, "asset", asset.String(), "quote", quote.String())
				}
				continue
			}
			ev, ok := tipStreamEvent(gen, snap, sources)
			if !ok {
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

// tipStreamEvent builds the SSE event payload for one tip emission.
// Returns (_, false) on JSON-marshal failure (which would mean a
// programming error in PriceSnapshot — caller skips emit so the
// stream stays alive).
func tipStreamEvent(gen *streaming.Generator, snap PriceSnapshot, sources []string) (streaming.Event, bool) {
	payload := tipStreamPayload{
		Data:    snap,
		AsOf:    time.Now().UTC(),
		Sources: sources,
		Flags:   Flags{SingleSource: len(sources) == 1},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return streaming.Event{}, false
	}
	return streaming.Event{
		ID:   gen.Next(),
		Type: "tip_update",
		Data: body,
	}, true
}

// tipStreamPayload is the SSE-data shape — a flattened envelope
// matching the request endpoint's wire response. Keeping the shape
// identical means SDK consumers can use one type for both polling
// and streaming.
type tipStreamPayload struct {
	Data    PriceSnapshot `json:"data"`
	AsOf    time.Time     `json:"as_of"`
	Sources []string      `json:"sources,omitempty"`
	Flags   Flags         `json:"flags"`
}
