package streaming

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// streamWriteDeadline bounds a single SSE write. It's rolled forward
// before every write, so a healthy stream (which writes a heartbeat every
// HeartbeatInterval < this) never trips it. It only fails a write that
// BLOCKS — a wedged socket whose send buffer is full. A client that stops
// reading but keeps the socket open absorbs heartbeats into the kernel
// buffer for days without blocking; [DefaultMaxStreamLifetime] bounds that
// one. Must be > the heartbeat interval so a slow-but-alive client isn't
// killed.
const streamWriteDeadline = 25 * time.Second

// DefaultMaxStreamLifetime is how long one SSE connection is served before
// the server ends it and the client reconnects (resuming via Last-Event-ID
// where the endpoint supports it). It is the bound on a TCP-alive client
// that never reads, which neither the write deadline nor TCP keepalive
// reclaims. Each stream adds up to 10% jitter so a cohort that connected
// together (after a deploy) does not reconnect together.
const DefaultMaxStreamLifetime = 30 * time.Minute

// drainWriteDeadline bounds the single courtesy frame written when the
// SERVER ends a stream at shutdown. Deliberately far shorter than
// streamWriteDeadline: see endStreamByServer.
const drainWriteDeadline = time.Second

// maxConcurrentStreams caps simultaneous SSE connections across all stream
// endpoints, so a flood of connections can't exhaust file descriptors /
// goroutines. Generous by default (legit fan-out is small on
// a single host); tune via SetMaxConcurrentStreams. <= 0 disables the cap.
var maxConcurrentStreams int64 = 8192

var activeStreams int64

var rejectedStreams int64

// SetMaxConcurrentStreams overrides the global concurrent-SSE-connection
// cap. Pass <= 0 to disable. Call once at startup.
func SetMaxConcurrentStreams(n int64) { atomic.StoreInt64(&maxConcurrentStreams, n) }

// StreamsRejected reports the cumulative number of SSE connections
// refused by the concurrency caps (global or per-IP) since process
// start. Operators read it, split by cap, as
// stellarindex_api_sse_streams_rejected_total.
func StreamsRejected() int64 { return atomic.LoadInt64(&rejectedStreams) }

// TryAcquireStreamSlot reserves one connection slot against the global and
// per-IP concurrency caps, writing the 503 itself when either refuses. It is
// [admitStream] exported for callers outside this package whose pre-flight work
// (before switching into SSE mode) is itself expensive, so admission happens
// before that compute.
//
// release MUST be called exactly once (typically `defer release()` right after a
// successful acquire); it is idempotent. Callers that pre-admit MUST switch
// their stream call from [StreamFromChannel] to [StreamFromChannelPreAdmitted],
// or a SECOND slot is acquired for the same connection.
func TryAcquireStreamSlot(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	return admitStream(w, r)
}

// admitStream reserves one connection slot against the global and
// per-IP concurrency caps, writing the 503 itself when either refuses.
//
// It runs BEFORE any per-connection allocation — in particular before
// [Hub.Subscribe], whose topic key is client-supplied on
// /v1/price/stream. Admitting first is what makes the caps bound Hub
// memory and not just socket count: a refused connection must never
// mint a topic.
//
// The returned release MUST be called exactly once when the stream
// ends; it is idempotent.
func admitStream(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	releaseGlobal, ok := acquireGlobalStreamSlot()
	if !ok {
		atomic.AddInt64(&rejectedStreams, 1)
		obs.APISSEStreamsRejectedTotal.WithLabelValues("global_cap").Inc()
		http.Error(w, "too many concurrent streams", http.StatusServiceUnavailable)
		return nil, false
	}
	// Per-IP cap: the global cap alone lets one client hold the
	// entire budget, so a single stalled/hostile address can starve the
	// streams for everyone. Give each client its own small ceiling.
	releaseIP, ok := acquireIPStreamSlot(r)
	if !ok {
		releaseGlobal()
		atomic.AddInt64(&rejectedStreams, 1)
		obs.APISSEStreamsRejectedTotal.WithLabelValues("per_ip_cap").Inc()
		http.Error(w, "too many concurrent streams from your address", http.StatusServiceUnavailable)
		return nil, false
	}
	return func() {
		releaseIP()
		releaseGlobal()
	}, true
}

// acquireGlobalStreamSlot reserves one slot against
// [maxConcurrentStreams], so a connection flood can't exhaust
// FDs/goroutines. A cap of <= 0 disables the ceiling but still counts
// the stream, so activeStreams stays truthful.
func acquireGlobalStreamSlot() (release func(), ok bool) {
	if limit := atomic.LoadInt64(&maxConcurrentStreams); limit > 0 {
		if atomic.AddInt64(&activeStreams, 1) > limit {
			atomic.AddInt64(&activeStreams, -1)
			return nil, false
		}
	} else {
		atomic.AddInt64(&activeStreams, 1)
	}
	obs.APISSEStreamsActive.Inc()
	var once sync.Once
	return func() {
		once.Do(func() {
			atomic.AddInt64(&activeStreams, -1)
			obs.APISSEStreamsActive.Dec()
		})
	}, true
}

// DefaultHeartbeatInterval is the cadence at which Stream emits
// SSE comment heartbeats (`:keepalive\n\n`) when no real events are
// flowing, so no idle bound on the proxy path cuts a quiet stream.
// internal/config/caddy_sse_timeouts_test.go holds every Caddy timeout
// on the stream path to at least two of these.
const DefaultHeartbeatInterval = 15 * time.Second

// DefaultRetry is the SSE reconnection-delay hint (the `retry:`
// field) writeStream sends once in the connection prelude. A client
// that loses the connection waits this long before its own
// automatic reconnect, giving the server headroom to recover
// (deploy roll, transient LB blip) without a reconnect stampede.
const DefaultRetry = 5000 * time.Millisecond

// StreamOptions tunes [Stream] behaviour. Zero values use sensible
// defaults so most callers can pass `StreamOptions{}`.
type StreamOptions struct {
	// HeartbeatInterval is the no-event cadence for SSE comment
	// heartbeats. Zero = DefaultHeartbeatInterval. Tests may want a
	// faster value to keep wall-clock test time short.
	HeartbeatInterval time.Duration

	// Drain, when wired, is the server-shutdown broadcast the writer
	// watches alongside r.Context(). Nil (the default) means "no
	// shutdown signal" and the writer runs until the client leaves —
	// the behaviour that made one attached stream cost a 30s deploy
	// stall. See [Drain] for the mechanism and for why this is not
	// http.Server.BaseContext.
	Drain *Drain

	// Revalidate, when wired, re-runs the request's credential, key
	// policy and quota checks at every heartbeat; false ends the stream.
	// Those checks are per-request middleware, so without this a key
	// revoked (or a quota exhausted, a tier downgraded) after admission
	// keeps streaming until the client leaves. Nil means no re-check;
	// v1's Server.streamOptions supplies it for every stream endpoint.
	Revalidate func(*http.Request) bool

	// MaxLifetime ends the stream after this long (plus jitter). Zero =
	// DefaultMaxStreamLifetime.
	MaxLifetime time.Duration
}

// Stream wires an http.ResponseWriter into the Hub for the supplied
// topics. It:
//
//  1. Sets the SSE-mandated response headers and disables proxy buffering.
//  2. Reads `Last-Event-ID` from the request (header takes
//     precedence over the `?last_event_id=` query param fallback)
//     and replays buffered events with greater IDs.
//  3. Forwards live events from the Hub as SSE frames until the
//     request context cancels.
//  4. Emits comment-only heartbeat frames at HeartbeatInterval to
//     keep proxies from idling out the connection.
//
// Stream is the convenience constructor for a Hub-driven endpoint.
// The v1 endpoints subscribe (or run a per-connection producer)
// themselves and feed [StreamFromChannelPreAdmitted]; see doc.go for
// which of them replay from a Hub.
func Stream(w http.ResponseWriter, r *http.Request, hub *Hub, topics []string, opts StreamOptions) {
	// Admission FIRST. hub.Subscribe allocates a Hub topic keyed by
	// `topics` — client-controlled on /v1/price/stream — so a
	// connection the caps are going to refuse must never get that far;
	// otherwise the caps bound sockets while the topic map grows
	// unchecked.
	release, ok := admitStream(w, r)
	if !ok {
		return
	}
	defer release()

	ch, cancel, err := hub.Subscribe(topics, LastEventIDFrom(r))
	if err != nil {
		WriteSubscribeRefused(w)
		return
	}
	defer cancel()
	writeStream(w, r, ch, opts)
}

// WriteSubscribeRefused answers a connection whose [Hub.Subscribe]
// failed with [ErrTopicCapacity]. Call it before any SSE header is sent.
func WriteSubscribeRefused(w http.ResponseWriter) {
	atomic.AddInt64(&rejectedStreams, 1)
	obs.APISSEStreamsRejectedTotal.WithLabelValues("topic_cap").Inc()
	http.Error(w, "too many concurrent stream topics", http.StatusServiceUnavailable)
}

// StreamFromChannel is the lower-level SSE writer: given any
// receive-only event channel, write headers, run the heartbeat-aware
// event loop, and return when the request context cancels or `ch`
// closes. Pair this with a per-connection producer goroutine to
// build endpoints whose events are computed on a tick rather than
// fanned out from a Hub.
//
// The caller is responsible for closing `ch` to signal "no more
// events"; closing terminates the stream cleanly.
func StreamFromChannel(w http.ResponseWriter, r *http.Request, ch <-chan Event, opts StreamOptions) {
	release, ok := admitStream(w, r)
	if !ok {
		return
	}
	defer release()
	writeStream(w, r, ch, opts)
}

// StreamFromChannelPreAdmitted is [StreamFromChannel] for a caller
// that already reserved its concurrency-cap slot via
// [TryAcquireStreamSlot] — e.g. because it has its own expensive
// pre-flight compute that must run AFTER admission, not before.
// Unlike StreamFromChannel, this does NOT acquire (or
// release) a slot itself: the caller's own TryAcquireStreamSlot +
// deferred release own that lifecycle end to end. Calling this
// instead of StreamFromChannel after a manual TryAcquireStreamSlot
// avoids reserving a SECOND slot for the same connection.
func StreamFromChannelPreAdmitted(w http.ResponseWriter, r *http.Request, ch <-chan Event, opts StreamOptions) {
	writeStream(w, r, ch, opts)
}

// writeStream is the post-admission SSE writer: headers, the rolling
// write deadline, and the heartbeat-aware event loop. Split out of
// [StreamFromChannel] so [Stream] can take its connection slot before
// subscribing (and therefore before allocating a Hub topic) while both
// entry points share one event loop.
func writeStream(w http.ResponseWriter, r *http.Request, ch <-chan Event, opts StreamOptions) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// The API's http.Server sets `WriteTimeout: 30s` to keep short
	// handlers honest, but that fixed deadline would reset an SSE stream
	// at 30s, and clearing it would let a blocked write hang forever. So
	// we ROLL a per-write deadline forward before every write (see
	// setWriteDeadline): a blocked write fails after streamWriteDeadline
	// and the handler returns + cleans up. A non-reading client whose
	// writes never block is bounded by the stream lifetime instead.
	//
	// On transports that don't expose SetWriteDeadline (httptest writers,
	// wrappers without Unwrap) the call returns http.ErrNotSupported,
	// which we ignore — those transports don't enforce write deadlines
	// anyway. Production wrappers all expose Unwrap().
	rc := http.NewResponseController(w)
	setWriteDeadline := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteDeadline))
	}

	setSSEHeaders(w.Header())
	w.WriteHeader(http.StatusOK)

	heartbeat := opts.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = DefaultHeartbeatInterval
	}

	ctx := r.Context()
	// r.Context() alone is only half the teardown story: it cancels when
	// the CLIENT goes away. draining is the other half — the server
	// going away — and an SSE connection would otherwise never learn
	// about it, pinning http.Server.Shutdown for the whole drain budget
	// (see [Drain]). Nil when no Drain is wired, and a nil channel never
	// becomes ready, so the select below keeps its pre-drain behaviour.
	draining := opts.Drain.Done()
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	expiry := time.NewTimer(streamLifetime(opts.MaxLifetime))
	defer expiry.Stop()

	// Initial flush so the client sees the response start
	// immediately rather than waiting for the first event. Some
	// clients deadlock if the server hasn't written headers + flushed
	// before they time out.
	//
	// retry: precedes :connected so a client that reconnects mid-way
	// through the prelude (or one that only reads the first frame)
	// still picks up the reconnection-delay hint.
	setWriteDeadline()
	if _, err := fmt.Fprintf(w, "retry: %d\n\n:connected\n\n", DefaultRetry.Milliseconds()); err != nil {
		return
	}
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case <-draining:
			endStreamByServer(w, flusher, rc, ":draining\n\n")
			return
		case <-expiry.C:
			endStreamByServer(w, flusher, rc, ":reconnect\n\n")
			return
		case ev, ok := <-ch:
			if !ok {
				// Channel closed → producer signalled done. Return
				// cleanly; client reconnects with Last-Event-ID for
				// resume.
				return
			}
			setWriteDeadline()
			if err := WriteFrame(w, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if opts.Revalidate != nil && !opts.Revalidate(r) {
				endStreamForRevocation(w, flusher, setWriteDeadline)
				return
			}
			setWriteDeadline()
			if _, err := fmt.Fprint(w, ":keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// setSSEHeaders writes the SSE response headers per WHATWG, before
// WriteHeader so the first frame goes out cleanly. X-Accel-Buffering
// disables nginx response buffering; Connection: keep-alive is implicit
// in HTTP/1.1 and harmless on HTTP/2.
//
// Cache-Control is a fallback only: the v1 CacheControl middleware has
// already set the per-route policy (`no-store` on /v1/price/stream,
// `private, …` on tip/observations), and overwriting it would turn
// no-store into a storable response and drop `private`.
func setSSEHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// endStreamForRevocation ends a stream whose credential, key policy or
// quota no longer admits it. The last frame is an SSE comment, as in
// [endStreamByServer]; the client's reconnect then gets the 401 / 403 /
// 429 problem response from the middleware, which is the in-band
// signal, and a conforming EventSource stops on that non-200.
func endStreamForRevocation(w http.ResponseWriter, flusher http.Flusher, setWriteDeadline func()) {
	setWriteDeadline()
	if _, err := fmt.Fprint(w, ":revoked\n\n"); err != nil {
		return
	}
	flusher.Flush()
}

// streamLifetime returns maxLifetime (DefaultMaxStreamLifetime when zero) plus up
// to 10% random jitter.
func streamLifetime(maxLifetime time.Duration) time.Duration {
	if maxLifetime <= 0 {
		maxLifetime = DefaultMaxStreamLifetime
	}
	if jitter := maxLifetime / 10; jitter > 0 {
		maxLifetime += rand.N(jitter)
	}
	return maxLifetime
}

// endStreamByServer closes a stream the SERVER is ending (shutdown drain or
// lifetime expiry) after writing the given comment frame, so net/http terminates
// the chunked body and EventSource clients reconnect normally instead of the
// reverse proxy logging a truncated response.
//
// The final frame is an SSE comment, not a named event: spec-legal, ignored by
// every conforming client, and it leaves the event vocabulary in
// openapi/stellar-index.v1.yaml unchanged.
//
// The write gets [drainWriteDeadline], not the rolling [streamWriteDeadline]: a
// stalled client with a full socket buffer must not hold the drain for the full
// stream deadline. Each connection blocks only its own goroutine, so the cost
// across stalled streams is the maximum, not the sum.
//
// A failed write is ignored: the connection is going away either way.
func endStreamByServer(w http.ResponseWriter, flusher http.Flusher, rc *http.ResponseController, frame string) {
	_ = rc.SetWriteDeadline(time.Now().Add(drainWriteDeadline))
	if _, err := fmt.Fprint(w, frame); err != nil {
		return
	}
	flusher.Flush()
}

// LastEventIDFrom returns the resume cursor from the request:
// header `Last-Event-ID` per the WHATWG SSE spec, or
// `?last_event_id=` as a fallback for clients that can't set custom
// headers (notably the EventSource API in browsers — it auto-sends
// the header on reconnect, but the *initial* connection may need
// the query-param form for resume across page reloads).
//
// Exported so non-Hub endpoints can consult it themselves (e.g. to
// log resumption events or skip stale state on reconnect).
func LastEventIDFrom(r *http.Request) string {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		return v
	}
	return r.URL.Query().Get("last_event_id")
}

// WriteFrame emits one SSE frame to w:
//
//	retry: <ms>        (omitted when Retry == 0)
//	id: <ID>
//	event: <Type>      (omitted when Type == "")
//	data: <line 1>
//	data: <line 2>     (one per \n in Data)
//	\n
//
// Each `data:` line ends with \n per the SSE spec; the trailing \n
// separates the frame from the next.
//
// WriteFrame does NOT flush the underlying writer; Stream and
// StreamFromChannel call Flush() after each successful WriteFrame.
// Direct callers (custom event loops) are responsible for flushing.
func WriteFrame(w http.ResponseWriter, ev Event) error {
	var b strings.Builder
	b.Grow(len(ev.Data) + 64)
	if ev.Retry > 0 {
		b.WriteString("retry: ")
		b.WriteString(strconv.FormatInt(ev.Retry.Milliseconds(), 10))
		b.WriteByte('\n')
	}
	if ev.ID != "" {
		b.WriteString("id: ")
		b.WriteString(ev.ID)
		b.WriteByte('\n')
	}
	if ev.Type != "" {
		b.WriteString("event: ")
		b.WriteString(ev.Type)
		b.WriteByte('\n')
	}
	if len(ev.Data) == 0 {
		b.WriteString("data:\n")
	} else {
		for _, line := range strings.Split(string(ev.Data), "\n") {
			b.WriteString("data: ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	_, err := w.Write([]byte(b.String()))
	return err
}
