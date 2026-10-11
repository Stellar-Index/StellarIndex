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

// tipStreamTickTimeout bounds a single per-tick computeTip call.
// RequestTimeout excludes `/stream` paths, so without a per-tick bound a slow computation
// could hold the producer goroutine and its DB connection open indefinitely. Mirrors
// observationsScanTimeout's 8s ceiling. It bounds the tip computation only; the divergence
// lookup has the shorter [tipStreamDivergenceBudget].
const tipStreamTickTimeout = 8 * time.Second

// tipStreamDivergenceBudget bounds the per-event divergence lookup, the one
// auxiliary read inside an emission, separately from the tick and pre-flight
// budgets that carry the tip computation.
//
// The cadence is the product; the verdict is an overlay whose absent state,
// `divergence_checked: false`, already means "could not verify". A degraded auxiliary signal
// must degrade the FLAG, never the stream, so it must not share [tipStreamTickTimeout].
//
// One second: the read is a single GET of the pair's precomputed div: record
// (divergence.Service.LookupCachedPairVerdict), low-millisecond when healthy. Past a second the
// event goes out unchecked, and a wedged store shifts an emission by at most a fifth of the
// default 5s window.
const tipStreamDivergenceBudget = time.Second

// tipStreamDivergenceStallInterval bounds how often a divergence lookup
// that exceeded [tipStreamDivergenceBudget] is logged.
//
// The limit is process-wide because what stalls is the one shared verdict store; keying it
// finer reproduces the flood (up to ~6k lines a minute at [defaultMaxTipTicksPerMinute]). One
// line a minute shows a stall begin and end, and each line carries the count it swallowed.
const tipStreamDivergenceStallInterval = time.Minute

// handlePriceTipStream serves GET /v1/price/tip/stream: the SSE counterpart to
// /v1/price/tip (ADR-0018 §"SSE stream wires onto the tip surface").
//
// Wire shape per connection:
//
//   - Headers: Content-Type: text/event-stream + X-Accel-Buffering: no (set by
//     the streaming writer); Cache-Control is the route policy from the
//     CacheControl middleware, which the writer keeps.
//   - Initial event: a tip_update as soon as the first compute completes.
//   - Recurring events: every window_seconds (default 5, clamp 1–60) a fresh tip
//     is computed and a tip_update fires on success. Failures are logged and
//     skipped; heartbeats keep the connection alive until data returns.
//   - Heartbeats: every streaming.DefaultHeartbeatInterval (15 s) when no real
//     event has flowed.
//
// Pre-stream errors (param validation, "no data ever" 404) are returned as
// problem+json: once the stream body starts the status can't be set, so failures
// must be detected pre-flight.
func (s *Server) handlePriceTipStream(w http.ResponseWriter, r *http.Request) {
	// Admit against the concurrency caps FIRST so a client at its cap does not pay for the
	// pre-flight compute. release is idempotent; the stream is handed off via
	// StreamFromChannelPreAdmitted so it does not acquire a second slot.
	release, ok := streaming.TryAcquireStreamSlot(w, r)
	if !ok {
		return
	}
	defer release()

	if s.Prices == nil {
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

	// Refuse before the DB pre-flight when no producer could be minted;
	// the hub-less path has no producer registry to consult.
	if s.Hub != nil {
		if outcome := s.tipProducerPrecheck(r, asset, quote, window); outcome != tipProducerAdmitted {
			s.writeTipProducerRefused(w, r, outcome, asset, quote, window)
			return
		}
	}

	// First synchronous compute — gives us a chance to return 404
	// before switching the response into SSE mode (where it's too
	// late to set a non-200 status code).
	//
	// BOUNDED, like the tick path: /stream paths skip the request-timeout middleware and
	// statement_timeout is 0, so without this a Postgres stall would pin the handler and its
	// pool connection until the client left (WriteTimeout does not cancel a running query).
	// A few IPs at the per-IP cap would exhaust the pool and never self-release.
	preflightCtx, cancelPreflight := context.WithTimeout(r.Context(), tipStreamTickTimeout)
	defer cancelPreflight()
	first, firstSources, err := s.computeTip(preflightCtx, asset, quote, window)
	if err != nil {
		s.writeTipPreflightError(w, r, err, asset, quote)
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
	firstEv, _ := s.tipStreamEvent(preflightCtx, &gen, asset, quote, first, firstSources)

	// Two producer shapes, because a per-connection producer costs ~6 DB queries/s per viewer:
	//
	//   - Hub-wired (production): ONE shared producer per distinct (asset, quote, window)
	//     publishes into the Hub, so DB cost scales with pairs watched, not viewers. The
	//     pre-flight snapshot is still this connection's first frame so the page paints
	//     instantly; a near-duplicate tip_update from the producer is an idempotent update.
	//   - Hub-less (tests, minimal binaries): a per-connection tick loop.
	if s.Hub != nil {
		// The producer's context is DELIBERATELY detached from r.Context()
		// (contextcheck): the shared compute loop outlives any single
		// connection — it stops via the registry's refcount + linger, not
		// via this request's cancellation.
		topic, releaseProducer, outcome := s.acquireTipProducer(r, asset, quote, window) //nolint:contextcheck
		if outcome != tipProducerAdmitted {
			s.writeTipProducerRefused(w, r, outcome, asset, quote, window)
			return
		}
		defer releaseProducer()
		lastEventID := streaming.LastEventIDFrom(r)
		sub, cancelSub, err := s.Hub.Subscribe([]string{topic}, lastEventID)
		if err != nil {
			streaming.WriteSubscribeRefused(w)
			return
		}
		defer cancelSub()

		ch := make(chan streaming.Event, tipStreamProducerQueueDepth)
		go s.forwardTipStream(r.Context(), ch, sub, firstEv, lastEventID != "")
		streaming.StreamFromChannelPreAdmitted(w, r, ch, s.streamOptions())
		return
	}

	ch := make(chan streaming.Event, tipStreamProducerQueueDepth)
	prodCtx, cancelProd := context.WithCancel(r.Context())
	defer cancelProd()

	go s.runTipStreamProducer(prodCtx, ch, &gen, asset, quote, window, firstEv)

	streaming.StreamFromChannelPreAdmitted(w, r, ch, s.streamOptions())
}

// writeTipPreflightError maps a failed pre-flight computeTip to its
// problem response; a client abort writes nothing.
func (s *Server) writeTipPreflightError(w http.ResponseWriter, r *http.Request, err error, asset, quote canonical.Asset) {
	if errors.Is(err, ErrPriceWithheld) {
		// Substance-gated pair: the stream cannot start — same verdict
		// and problem type as the request endpoint.
		writePriceWithheldProblem(w, r, asset, quote, priceWithheldReason(err))
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
}

// writeTipProducerRefused answers a connection whose pair has no shared producer running and
// may not mint one.
//
// Falling through to the per-connection tick loop would reintroduce the unbounded detached
// compute the bounds exist to prevent. Every refusal is 503 + Retry-After, not 429: that is the
// published contract, and a caller's lingering producers expire within [tipProducerLinger]
// anyway. Refusals are told apart in the log and detail, so an operator can see whether one
// address is enumerating the key space or the deployment has outgrown its ceiling.
func (s *Server) writeTipProducerRefused(
	w http.ResponseWriter, r *http.Request,
	outcome tipProducerOutcome, asset, quote canonical.Asset, window int,
) {
	detail := "too many distinct price-tip streams are active; retry shortly"
	if outcome.callerRefusal() {
		detail = "too many distinct price-tip streams are already active from your address; retry shortly"
	}
	s.logger.Warn("tip producer refused — refusing stream",
		"reason", outcome.String(),
		"asset", asset.String(), "quote", quote.String(), "window", window,
		"running", s.tipProducers.running(),
		"refused_total", s.tipProducers.refusedCount(),
		"refused_per_caller_total", s.tipProducers.refusedPerCallerCount())
	w.Header().Set("Retry-After", "30")
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/capacity",
		"Stream capacity reached", http.StatusServiceUnavailable,
		detail)
}

// forwardTipStream bridges a Hub subscription onto the SSE writer channel,
// prepending the connection's own pre-flight snapshot so the first frame never
// waits for the shared producer's next tick. Returns (closing ch so the SSE
// writer ends cleanly) when the request context cancels or the Hub subscription
// closes (hub shutdown / topic evict; the client's EventSource auto-reconnects).
//
// isResume is whether this connection supplied a Last-Event-ID. On a fresh
// connect the Hub has nothing to replay, so the snapshot is the only early
// frame. On a RESUME, `sub` already replays the buffered backlog after the
// client's cursor in ID order, but firstEv was computed fresh under its own
// [streaming.Generator], so its ID is newer than anything buffered; sending it
// ahead of the replay walks the SSE cursor backwards. The backlog already
// carries the pair's current state, so skip firstEv.
func (s *Server) forwardTipStream(
	ctx context.Context,
	ch chan<- streaming.Event,
	sub <-chan streaming.Event,
	firstEv streaming.Event,
	isResume bool,
) {
	defer s.recoverStreamProducer("price_tip")
	defer close(ch)

	if !isResume && len(firstEv.Data) > 0 {
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
// `windowSeconds` recomputing the tip price. A withheld pair emits a
// price_withheld marker (see [Server.tipTickEvent]); other failures are
// skipped (heartbeats keep the connection alive) — the assumption is
// that transient unavailability resolves itself and the next tick will
// succeed.
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
	// This goroutine is outside middleware.Recoverer and the stream is unauthenticated, so an
	// unrecovered panic would kill the whole process; recover so only this connection ends.
	// Registered BEFORE `defer close(ch)` so the close runs first (LIFO) and the SSE writer
	// ends cleanly, then the panic is logged, never swallowed.
	defer s.recoverStreamProducer("price_tip")
	defer close(ch)

	if len(firstEv.Data) > 0 {
		select {
		case <-ctx.Done():
			return
		case ch <- firstEv:
		}
	}

	ticker := time.NewTicker(s.streamCadence(windowSeconds))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ev, ok := s.tipTickEvent(ctx, gen, asset, quote, windowSeconds)
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

// tipTickEvent computes one tick's emission for both tip producers: a
// tip_update, or a price_withheld marker while a withholding gate
// refuses the pair — a pair that crosses the gate after connect must
// read on the wire as withheld, not as a quiet market. ok is false only
// when there is nothing to emit (a failed compute, logged here).
func (s *Server) tipTickEvent(ctx context.Context, gen *streaming.Generator, asset, quote canonical.Asset, window int) (streaming.Event, bool) {
	tickCtx, cancel := context.WithTimeout(ctx, tipStreamTickTimeout)
	defer cancel()
	snap, sources, err := s.computeTipGated(tickCtx, asset, quote, window)
	if errors.Is(err, ErrPriceWithheld) {
		return tipWithheldEvent(gen, asset, quote, priceWithheldReason(err))
	}
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("computeTip failed (stream tick) — skipping emit",
				"err", err, "asset", asset.String(), "quote", quote.String())
		}
		return streaming.Event{}, false
	}
	// The event's divergence lookup runs inside this per-tick budget but
	// on its own shorter one, so a stalled verdict store cannot push the
	// emission past its window.
	return s.tipStreamEvent(tickCtx, gen, asset, quote, snap, sources)
}

// tipWithheldPayload is the data of a price_withheld stream event. The
// reason vocabulary is the one the request endpoint's 404 carries.
type tipWithheldPayload struct {
	AssetID string              `json:"asset_id"`
	Quote   string              `json:"quote"`
	Reason  PriceWithheldReason `json:"reason"`
	AsOf    WireTime            `json:"as_of"`
}

func tipWithheldEvent(gen *streaming.Generator, asset, quote canonical.Asset, reason PriceWithheldReason) (streaming.Event, bool) {
	body, err := json.Marshal(tipWithheldPayload{
		AssetID: asset.String(),
		Quote:   quote.String(),
		Reason:  reason,
		AsOf:    WireTime(time.Now().UTC()),
	})
	if err != nil {
		return streaming.Event{}, false
	}
	return streaming.Event{ID: gen.Next(), Type: "price_withheld", Data: body}, true
}

// tipStreamEvent builds the SSE event payload for one tip emission.
// The flags are built from the request endpoint's own lookup (tipFlags),
// so an event carries the same verdict a GET on the same base would —
// while the store answers inside [tipStreamDivergenceBudget].
//
// The two surfaces deliberately diverge once the store is slow: the GET walks on the request
// budget while the stream uses the shorter sub-budget, so the same base can be checked=true on
// the GET and checked=false on the stream. That is the trade stated on
// [tipStreamDivergenceBudget], and it is the safe direction: the stream never manufactures an
// all-clear the GET would not give.
//
// See [Server.tipStreamFlags]. Returns (_, false) on JSON-marshal failure
// (which would mean a programming error in PriceSnapshot — caller
// skips emit so the stream stays alive).
func (s *Server) tipStreamEvent(ctx context.Context, gen *streaming.Generator, asset, quote canonical.Asset, snap PriceSnapshot, sources []string) (streaming.Event, bool) {
	// Flags first, THEN the timestamp: as_of must be taken after the last step that can delay
	// the emission, or a slow divergence lookup ships an as_of up to a sub-budget stale.
	flags := s.tipStreamFlags(ctx, snap, asset, quote, sources)
	payload := tipStreamPayload{
		Data:    snap,
		AsOf:    WireTime(time.Now().UTC()),
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
// The cadence is the product: on a stall the stream degrades the FLAG and emits on time, with
// `divergence_checked: false` meaning "could not verify", never "prices agree".
//
// A stall is told apart from teardown by comparing the two contexts: the sub-budget expiring
// while the caller's budget has room is a stalled store; both expiring together is the tick or
// the client going away, which is not logged.
func (s *Server) tipStreamFlags(ctx context.Context, snap PriceSnapshot, asset, quote canonical.Asset, sources []string) Flags {
	budget := s.tipDivergenceBudget()
	lookupCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	flags := s.tipFlags(lookupCtx, snap, asset, quote, sources)
	if lookupCtx.Err() == nil || ctx.Err() != nil {
		return flags
	}
	// Force the unchecked pair so a partial verdict from an unfinished read cannot leak out.
	flags.DivergenceWarning, flags.DivergenceChecked = false, false
	if suppressed, ok := s.tipDivergenceStalls.admit(time.Now()); ok {
		s.logger.Warn("divergence lookup exceeded its tip-stream budget — emitting with the verdict unchecked",
			"asset", asset.String(), "budget", budget,
			"suppressed_since_last", suppressed)
	}
	return flags
}

// tipDivergenceBudget is [tipStreamDivergenceBudget] unless a test
// shortened it.
func (s *Server) tipDivergenceBudget() time.Duration {
	if s.tipDivergenceBudgetFor > 0 {
		return s.tipDivergenceBudgetFor
	}
	return tipStreamDivergenceBudget
}

// streamCadence is the tick period of a stream whose cadence parameter
// is n seconds.
func (s *Server) streamCadence(n int) time.Duration {
	second := time.Second
	if s.streamSecondFor > 0 {
		second = s.streamSecondFor
	}
	return time.Duration(n) * second
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
	// A gap longer than the interval means the previous incident ended; drop its residue
	// rather than attribute an old count to a fresh incident, which is worse than a missing one.
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
	AsOf    WireTime      `json:"as_of"`
	Sources []string      `json:"sources,omitempty"`
	Flags   Flags         `json:"flags"`
}
