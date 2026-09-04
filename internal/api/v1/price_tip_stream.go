package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
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
//
// It bounds the tip COMPUTATION. The per-event divergence lookup that
// rides beside it has its own, much shorter [tipStreamDivergenceBudget]
// — see there for why an auxiliary read must not inherit this one.
const tipStreamTickTimeout = 8 * time.Second

// tipStreamDivergenceBudget bounds the per-event divergence lookup —
// the one auxiliary read inside an emission — separately from the tick
// and pre-flight budgets that carry the tip computation itself.
//
// Without it the lookup inherited [tipStreamTickTimeout], so a stalled
// verdict store held the whole 8s: measured at 8.0036s, during which the
// SSE response headers were not written and no further event was
// emitted. That is backwards. The cadence is the product; the verdict is
// an overlay whose documented absent state, `divergence_checked: false`,
// already means "could not verify" (CS-087) — so a degraded auxiliary
// signal must degrade the FLAG, never the stream.
//
// One second is chosen against two references rather than picked, and
// what it has to cover is a FAN-OUT, not one record read. Per spelling,
// divergence.LookupCached issues 1 SMEMBERS over the base index plus one
// SEQUENTIAL GET per indexed quote — 7 commands for a base with 6
// indexed quotes — and [Server.lookupDivergenceFlag] wraps that in an
// alias walk of up to three canonical spellings, so an XLM-family base
// can cost up to 21 sequential round-trips inside this one budget.
// XLM's family is three spellings today, so the effective per-spelling
// tolerance is about 333ms; a base outside an alias family has one
// spelling and gets the full second.
//
// That is still ample for a healthy store: these are precomputed records
// the cross-reference worker refreshes out of band, so a healthy read is
// low-millisecond and a whole 21-trip walk lands far inside 333ms per
// spelling. It is deliberately NOT ample for a degraded one — a store
// answering every read in a uniform 400ms exhausts the walk and the
// event goes out unchecked, which is the trade this constant exists to
// make: at 400ms per read the stream would otherwise be spending over a
// second per emission on an optional flag.
//
// The second reference is the cadence: a second is an eighth of the tick
// budget and at most a fifth of the DEFAULT 5s window, so a wedged store
// shifts an emission by a fraction of its period instead of consuming
// the period whole. The package's sibling best-effort reads sit at 2s
// ([tokenMetadataReadTimeout] and the coverage-floor probe); those run
// under the 15s REQUEST budget, and the tighter cadence being protected
// here is why this one is tighter.
const tipStreamDivergenceBudget = time.Second

// tipStreamDivergenceStallInterval bounds how often a divergence lookup
// that exceeded [tipStreamDivergenceBudget] is logged.
//
// The warning is process-wide rather than per stream or per pair on
// purpose: what stalls is the one shared verdict store, so keying the
// limit any finer just reproduces the flood. The producer ceiling is 512
// and a window may be as short as a second, so an unbounded warning is
// up to ~30k lines a minute at precisely the moment an operator is
// trying to read the log. One line a minute is enough to see a stall
// begin, persist and end, and each line carries the count it swallowed
// so the volume stays visible instead of hidden.
const tipStreamDivergenceStallInterval = time.Minute

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
		writeProblemErr(w, r, err,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// A valid first snapshot + an open response. Build the
	// connection's first frame here, still under the pre-flight budget,
	// so both producer shapes below emit the same event. The frame's
	// divergence lookup runs under its OWN [tipStreamDivergenceBudget]
	// inside that budget: this call sits between the client's request and
	// the response headers, so letting an auxiliary read spend the whole
	// pre-flight here is paid directly in time-to-first-byte. Then switch
	// to SSE.
	var gen streaming.Generator
	firstEv, _ := s.tipStreamEvent(preflightCtx, &gen, asset, first, firstSources)

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
		topic, releaseProducer, ok := s.acquireTipProducer(asset, quote, window) //nolint:contextcheck
		if !ok {
			// At the producer ceiling and this pair has none running
			// (wave-D UNAUTH-DOS-1). Refuse rather than fall through to
			// the per-connection loop below: that path is the unbounded
			// compute the ceiling exists to prevent, so falling back
			// would make the cap decorative.
			//
			// 503 + Retry-After, not 429: the client is not at fault and
			// nothing about retrying the same request is invalid — the
			// server is at capacity for NEW pairs, and a viewer of an
			// already-watched pair is still served normally.
			s.logger.Warn("tip producer ceiling reached — refusing stream",
				"asset", asset.String(), "quote", quote.String(), "window", window,
				"running", s.tipProducers.running(),
				"refused_total", s.tipProducers.refusedCount())
			w.Header().Set("Retry-After", "30")
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/capacity",
				"Stream capacity reached", http.StatusServiceUnavailable,
				"too many distinct price-tip streams are active; retry shortly")
			return
		}
		defer releaseProducer()
		sub, cancelSub := s.hub.Subscribe([]string{topic}, streaming.LastEventIDFrom(r))
		defer cancelSub()

		ch := make(chan streaming.Event, tipStreamProducerQueueDepth)
		go s.forwardTipStream(r.Context(), ch, sub, firstEv)
		streaming.StreamFromChannelPreAdmitted(w, r, ch, streaming.StreamOptions{})
		return
	}

	ch := make(chan streaming.Event, tipStreamProducerQueueDepth)
	prodCtx, cancelProd := context.WithCancel(r.Context())
	defer cancelProd()

	go s.runTipStreamProducer(prodCtx, ch, &gen, asset, quote, window, firstEv)

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
// Emits the pre-built initial event (skipped when the handler could not
// marshal one — its Data is then empty), then ticks every
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
	firstEv streaming.Event,
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

	if len(firstEv.Data) > 0 {
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
			if err != nil {
				cancel()
				if ctx.Err() == nil {
					s.logger.Warn("computeTip failed (stream tick) — skipping emit",
						"err", err, "asset", asset.String(), "quote", quote.String())
				}
				continue
			}
			// The event's divergence lookup runs inside this per-tick
			// budget but on its own shorter one, so a stalled verdict
			// store cannot push the emission past its window.
			ev, ok := s.tipStreamEvent(tickCtx, gen, asset, snap, sources)
			cancel()
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
// The flags are built from the request endpoint's own lookup (tipFlags),
// so an event carries the same verdict a GET on the same base would —
// while the store answers inside [tipStreamDivergenceBudget].
//
// The two surfaces DO diverge once the store is slow, and deliberately.
// The GET runs the walk on the request budget; this runs it on the
// shorter sub-budget, so with every spelling answering a uniform 400ms
// the GET serves checked=true and the stream serves checked=false for
// the same base at the same instant. That is the trade stated on
// [tipStreamDivergenceBudget]: the stream is a cadence product and a
// slow flag is dropped rather than waited for. `divergence_checked:
// false` is the honest report of it — "could not verify" (CS-087), which
// is exactly what happened — and it is the safe direction: the stream
// never manufactures an all-clear the GET would not give.
//
// See [Server.tipStreamFlags]. Returns (_, false) on JSON-marshal failure
// (which would mean a programming error in PriceSnapshot — caller
// skips emit so the stream stays alive).
func (s *Server) tipStreamEvent(ctx context.Context, gen *streaming.Generator, asset canonical.Asset, snap PriceSnapshot, sources []string) (streaming.Event, bool) {
	// Flags first, THEN the timestamp. as_of describes the event being
	// emitted, so it is taken after the last step that can delay the
	// emission. Stamped inline in the literal below it was evaluated
	// before the divergence lookup (Go orders calls in a composite
	// literal left to right), so a slow lookup published an as_of that
	// was already up to a whole sub-budget stale when it went on the wire.
	flags := s.tipStreamFlags(ctx, asset, sources)
	payload := tipStreamPayload{
		Data:    snap,
		AsOf:    time.Now().UTC(),
		Sources: sources,
		Flags:   flags,
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

// tipStreamFlags builds one emission's envelope flags, running the
// auxiliary divergence lookup under [tipStreamDivergenceBudget] rather
// than the caller's whole tick or pre-flight budget.
//
// The lookup is a nice-to-have; the cadence is the product. So on a
// stall the stream degrades the FLAG and emits on time: the verdict goes
// out unchecked, which is exactly what `divergence_checked: false` means
// on this surface (CS-087 — "could not verify", never "prices agree").
// An emission is never dropped or delayed because this read was slow.
//
// The stall is distinguished from ordinary teardown by comparing the two
// contexts: the sub-budget expiring while the CALLER's budget still has
// room is a stalled store, whereas both expiring together is the tick or
// the client going away, which is not news about the verdict store and
// is not logged.
func (s *Server) tipStreamFlags(ctx context.Context, asset canonical.Asset, sources []string) Flags {
	lookupCtx, cancel := context.WithTimeout(ctx, tipStreamDivergenceBudget)
	defer cancel()
	flags := s.tipFlags(lookupCtx, asset, sources)
	if lookupCtx.Err() == nil || ctx.Err() != nil {
		return flags
	}
	// Belt and braces: lookupDivergenceFlag already returns the unchecked
	// pair on a failed walk, and stating it here means a future lookup
	// that returns a partial verdict cannot leak one out of a read that
	// did not finish.
	flags.DivergenceWarning, flags.DivergenceChecked = false, false
	if suppressed, ok := s.tipDivergenceStalls.admit(time.Now()); ok {
		s.logger.Warn("divergence lookup exceeded its tip-stream budget — emitting with the verdict unchecked",
			"asset", asset.String(), "budget", tipStreamDivergenceBudget,
			"suppressed_since_last", suppressed)
	}
	return flags
}

// tipDivergenceStallLog rate-limits the stalled-lookup warning to one
// line per [tipStreamDivergenceStallInterval] for the whole process,
// carrying the number of stalls swallowed since the previous line.
type tipDivergenceStallLog struct {
	mu sync.Mutex
	// lastLine is when a warning was last emitted; lastStall is when a
	// stall was last seen. They are separate so a residue can be aged
	// out against the incident that produced it rather than against the
	// last time anything was printed.
	lastLine   time.Time
	lastStall  time.Time
	suppressed uint64
}

// admit reports whether this stall should be logged now and, when it
// should, how many went unlogged since the last one that was.
func (l *tipDivergenceStallLog) admit(now time.Time) (suppressed uint64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A gap longer than the interval means the previous incident ended.
	// Its unreported tail belongs to it, so drop the residue rather than
	// stamp a fresh incident an hour later with an hour-old count — a
	// number attributed to the wrong incident is worse than a missing
	// one, and the tail is bounded by a single interval of stalls that
	// the incident's own earlier lines already characterised.
	if !l.lastStall.IsZero() && now.Sub(l.lastStall) > tipStreamDivergenceStallInterval {
		l.suppressed = 0
	}
	l.lastStall = now
	if !l.lastLine.IsZero() && now.Sub(l.lastLine) < tipStreamDivergenceStallInterval {
		l.suppressed++
		return 0, false
	}
	suppressed, l.suppressed = l.suppressed, 0
	l.lastLine = now
	return suppressed, true
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
