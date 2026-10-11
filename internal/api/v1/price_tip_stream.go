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

// tipStreamProducerQueueDepth is the per-connection channel capacity between producer and SSE
// writer: enough for the writer to lag a tick or two, small enough that a wedged writer blocks the
// producer's next send and ctx cancel then signals teardown.
const tipStreamProducerQueueDepth = 4

// tipStreamTickTimeout bounds one per-tick computeTip call. RequestTimeout excludes `/stream`
// paths, so a slow computation would otherwise hold the producer and its DB connection open
// indefinitely.
const tipStreamTickTimeout = 8 * time.Second

// tipStreamDivergenceBudget bounds the per-event divergence lookup, the one auxiliary read in an
// emission, separately from the tick and pre-flight budgets.
//
// The cadence is the product; `divergence_checked: false` already means "could not verify". A
// degraded auxiliary signal must degrade the flag, never the stream, so it cannot share
// [tipStreamTickTimeout]. The read is a single GET of the precomputed div: record; past the
// budget the event goes out unchecked, shifting an emission by at most a fifth of the default 5s
// window.
const tipStreamDivergenceBudget = time.Second

// tipStreamDivergenceStallInterval bounds how often a divergence lookup that exceeded
// [tipStreamDivergenceBudget] is logged. It is process-wide because the one shared verdict store
// stalls; per-key limits would flood the log. Each line carries the count it swallowed.
const tipStreamDivergenceStallInterval = time.Minute

// handlePriceTipStream serves GET /v1/price/tip/stream: the SSE counterpart to /v1/price/tip
// (ADR-0018 §"SSE stream wires onto the tip surface").
//
// A tip_update fires as soon as the first compute completes, then every window_seconds (default 5,
// clamp 1–60). Failed ticks are logged and skipped; heartbeats every
// streaming.DefaultHeartbeatInterval keep the connection alive. Cache-Control is the route policy
// from the CacheControl middleware, which the writer keeps.
//
// Pre-stream errors (param validation, "no data ever" 404) are problem+json: once the body starts
// the status can't be set, so failures must be detected pre-flight.
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

	// Refuse before the DB pre-flight when no producer could be minted (hub-less has no registry).
	if s.Hub != nil {
		if outcome := s.tipProducerPrecheck(r, asset, quote, window); outcome != tipProducerAdmitted {
			s.writeTipProducerRefused(w, r, outcome, asset, quote, window)
			return
		}
	}

	// First synchronous compute, so a 404 can still be returned before the response switches to SSE.
	// Bounded like the tick path: /stream skips the request-timeout middleware and
	// statement_timeout is 0, so a Postgres stall would pin the handler and its pool connection
	// (WriteTimeout does not cancel a running query).
	preflightCtx, cancelPreflight := context.WithTimeout(r.Context(), tipStreamTickTimeout)
	defer cancelPreflight()
	first, firstSources, err := s.computeTip(preflightCtx, asset, quote, window)
	if err != nil {
		s.writeTipPreflightError(w, r, err, asset, quote)
		return
	}

	// Build the first frame here, under the pre-flight budget, so both producer shapes emit the
	// same event. Its divergence lookup gets its own [tipStreamDivergenceBudget] because this call
	// sits before the response headers and any auxiliary delay is paid in time-to-first-byte.
	var gen streaming.Generator
	firstEv, _ := s.tipStreamEvent(preflightCtx, &gen, asset, quote, first, firstSources)

	// Production runs ONE shared producer per distinct (asset, quote, window), so DB cost scales
	// with pairs watched, not viewers. The pre-flight snapshot is still this connection's first
	// frame; a near-duplicate tip_update from the producer is idempotent. Hub-less (tests, minimal
	// binaries) uses a per-connection tick loop.
	if s.Hub != nil {
		// The producer's context is DELIBERATELY detached from r.Context(): the shared compute loop
		// outlives any single connection and stops via the registry's refcount + linger.
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
		// Substance-gated pair: same verdict and problem type as the request endpoint.
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
// Falling through to the per-connection tick loop would reintroduce the unbounded compute the
// bounds exist to prevent. Every refusal is 503 + Retry-After, not 429 (the published contract;
// lingering producers expire within [tipProducerLinger]).
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

// forwardTipStream bridges a Hub subscription onto the SSE writer channel, prepending the
// connection's pre-flight snapshot so the first frame never waits for the producer's next tick.
// It closes ch when the request context cancels or the subscription closes (hub shutdown or topic
// evict; the client's EventSource auto-reconnects).
//
// isResume is whether the connection supplied a Last-Event-ID. On a resume `sub` replays the
// buffered backlog in ID order, but firstEv came from its own [streaming.Generator] and its ID is
// newer than anything buffered, so sending it ahead of the replay walks the SSE cursor backwards.
// The backlog already carries the pair's current state, so firstEv is skipped.
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

// runTipStreamProducer is the per-connection compute and push loop. It emits the pre-built initial
// event (skipped when its Data is empty), then recomputes every `windowSeconds`. A withheld pair
// emits a price_withheld marker ([Server.tipTickEvent]); other failures are skipped on the
// assumption that transient unavailability clears by the next tick. It returns when ctx cancels and
// closes ch so [streaming.StreamFromChannel] returns cleanly.
func (s *Server) runTipStreamProducer(
	ctx context.Context,
	ch chan<- streaming.Event,
	gen *streaming.Generator,
	asset, quote canonical.Asset,
	windowSeconds int,
	firstEv streaming.Event,
) {
	// Outside middleware.Recoverer and unauthenticated: an unrecovered panic would kill the
	// process. Registered BEFORE `defer close(ch)` so the close runs first (LIFO) and the SSE
	// writer ends cleanly, then the panic is logged.
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

// tipTickEvent computes one tick's emission for both producers: a tip_update, or a price_withheld
// marker while a withholding gate refuses the pair, so a pair crossing the gate after connect reads
// as withheld, not as a quiet market. ok is false only when there is nothing to emit.
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
	// The divergence lookup has its own shorter budget so a stalled verdict store cannot push the
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

// tipStreamEvent builds the SSE payload for one tip emission, with flags from the request
// endpoint's own lookup (tipFlags) so it carries the verdict a GET would, while the store answers
// inside [tipStreamDivergenceBudget].
//
// Once the store is slow the surfaces deliberately diverge (GET uses the request budget, the
// stream the shorter sub-budget), so a base can be checked=true on GET and checked=false on the
// stream. That is the safe direction: the stream never manufactures an all-clear the GET would
// not give. Returns (_, false) on JSON-marshal failure; the caller skips the emit.
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

// tipStreamFlags builds one emission's envelope flags, running the divergence lookup under
// [tipStreamDivergenceBudget] rather than the caller's whole budget. On a stall the stream degrades
// the flag and emits on time, with `divergence_checked: false` meaning "could not verify", never
// "prices agree". Comparing the two contexts tells a stall (sub-budget expired, caller's has room)
// from teardown (both expired), which is not logged.
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

// tipDivergenceStallLog rate-limits the stalled-lookup warning to one line per
// [tipStreamDivergenceStallInterval] for the whole process.
type tipDivergenceStallLog struct {
	mu sync.Mutex
	// lastLine and lastStall are separate so a residue ages out against the incident that
	// produced it, not against the last time anything was printed.
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

// tipStreamPayload is the SSE data: the request endpoint's flattened envelope, so SDK consumers
// use one type for polling and streaming.
type tipStreamPayload struct {
	Data    PriceSnapshot `json:"data"`
	AsOf    WireTime      `json:"as_of"`
	Sources []string      `json:"sources,omitempty"`
	Flags   Flags         `json:"flags"`
}
