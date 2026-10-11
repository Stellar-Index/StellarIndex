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

// streamWriteDeadline bounds a single SSE write, rolled forward before every write. It only fails a
// write that BLOCKS (wedged socket, full send buffer); a client that stops reading but keeps the
// socket open is bounded by [DefaultMaxStreamLifetime]. Must exceed the heartbeat interval so a
// slow-but-alive client is not killed.
const streamWriteDeadline = 25 * time.Second

// DefaultMaxStreamLifetime is how long one SSE connection is served before the server ends it and
// the client reconnects (resuming via Last-Event-ID where supported). It bounds a TCP-alive client
// that never reads, which neither the write deadline nor TCP keepalive reclaims. Up to 10% jitter
// per stream keeps a cohort connected together (after a deploy) from reconnecting together.
const DefaultMaxStreamLifetime = 30 * time.Minute

// drainWriteDeadline bounds the single courtesy frame written when the
// SERVER ends a stream at shutdown. Deliberately far shorter than
// streamWriteDeadline: see endStreamByServer.
const drainWriteDeadline = time.Second

// maxConcurrentStreams caps simultaneous SSE connections across all stream endpoints so a flood
// cannot exhaust file descriptors or goroutines. <= 0 disables the cap.
var maxConcurrentStreams int64 = 8192

var activeStreams int64

var rejectedStreams int64

// SetMaxConcurrentStreams overrides the global concurrent-SSE-connection
// cap. Pass <= 0 to disable. Call once at startup.
func SetMaxConcurrentStreams(n int64) { atomic.StoreInt64(&maxConcurrentStreams, n) }

// StreamsRejected reports the cumulative SSE connections refused by the concurrency caps since
// process start (exported per cap as stellarindex_api_sse_streams_rejected_total).
func StreamsRejected() int64 { return atomic.LoadInt64(&rejectedStreams) }

// TryAcquireStreamSlot is [admitStream] exported for callers whose pre-flight work is expensive, so
// admission happens before that compute. release is idempotent and MUST be called when the stream
// ends. Callers that pre-admit MUST use [StreamFromChannelPreAdmitted] instead of
// [StreamFromChannel], or a SECOND slot is acquired for the same connection.
func TryAcquireStreamSlot(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	return admitStream(w, r)
}

// admitStream reserves one connection slot against the global and per-IP caps, writing the 503
// itself when either refuses. It runs BEFORE [Hub.Subscribe] (whose topic key is client-supplied
// on /v1/price/stream) so the caps bound Hub memory, not just sockets: a refused connection must
// never mint a topic. The returned release is idempotent.
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

// DefaultHeartbeatInterval is the cadence of SSE comment heartbeats (`:keepalive\n\n`) when no
// events flow, so no idle bound on the proxy path cuts a quiet stream.
// internal/config/caddy_sse_timeouts_test.go holds every Caddy timeout on the stream path to at
// least two of these.
const DefaultHeartbeatInterval = 15 * time.Second

// DefaultRetry is the SSE `retry:` reconnection-delay hint sent once in the prelude, giving the
// server headroom to recover (deploy roll, LB blip) without a reconnect stampede.
const DefaultRetry = 5000 * time.Millisecond

// StreamOptions tunes [Stream] behaviour. Zero values use sensible
// defaults so most callers can pass `StreamOptions{}`.
type StreamOptions struct {
	// HeartbeatInterval is the no-event cadence for SSE comment heartbeats. Zero =
	// DefaultHeartbeatInterval.
	HeartbeatInterval time.Duration

	// Drain, when wired, is the server-shutdown broadcast the writer watches alongside
	// r.Context(). Nil means the writer runs until the client leaves. See [Drain] for why this is
	// not http.Server.BaseContext.
	Drain *Drain

	// Revalidate, when wired, re-runs the credential, key policy and quota checks at every
	// heartbeat; false ends the stream. Those checks are per-request middleware, so without this a
	// key revoked (or quota exhausted, tier downgraded) after admission keeps streaming.
	Revalidate func(*http.Request) bool

	// MaxLifetime ends the stream after this long (plus jitter). Zero =
	// DefaultMaxStreamLifetime.
	MaxLifetime time.Duration
}

// Stream wires an http.ResponseWriter into the Hub for the supplied topics: SSE headers, replay
// from Last-Event-ID (header wins over `?last_event_id=`), live forwarding until the request
// context cancels, and comment heartbeats. The v1 endpoints mostly subscribe themselves and feed
// [StreamFromChannelPreAdmitted]; see doc.go for which replay from a Hub.
func Stream(w http.ResponseWriter, r *http.Request, hub *Hub, topics []string, opts StreamOptions) {
	// Admission FIRST: hub.Subscribe allocates a topic keyed by client-controlled `topics`, so a
	// connection the caps will refuse must never get that far.
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

// StreamFromChannel is the lower-level SSE writer for any receive-only event channel, for endpoints
// whose events are computed per connection rather than fanned out from a Hub. It returns when the
// request context cancels or the caller closes `ch`, which ends the stream cleanly.
func StreamFromChannel(w http.ResponseWriter, r *http.Request, ch <-chan Event, opts StreamOptions) {
	release, ok := admitStream(w, r)
	if !ok {
		return
	}
	defer release()
	writeStream(w, r, ch, opts)
}

// StreamFromChannelPreAdmitted is [StreamFromChannel] for a caller that already reserved its slot
// via [TryAcquireStreamSlot]. It neither acquires nor releases a slot; the caller owns that
// lifecycle, which avoids reserving a SECOND slot for the same connection.
func StreamFromChannelPreAdmitted(w http.ResponseWriter, r *http.Request, ch <-chan Event, opts StreamOptions) {
	writeStream(w, r, ch, opts)
}

// writeStream is the post-admission SSE writer, split out so [Stream] can take its slot before
// subscribing (and so before allocating a Hub topic) while all entry points share one event loop.
func writeStream(w http.ResponseWriter, r *http.Request, ch <-chan Event, opts StreamOptions) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// The server's fixed 30s WriteTimeout would reset an SSE stream, and clearing it would let a
	// blocked write hang forever, so a per-write deadline is rolled forward before every write
	// (setWriteDeadline). A non-reading client whose writes never block is bounded by the stream
	// lifetime instead. http.ErrNotSupported (httptest writers, wrappers without Unwrap) is
	// ignored because those transports enforce no write deadline anyway.
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
	// r.Context() cancels only when the CLIENT leaves; draining covers the server leaving, which
	// would otherwise pin http.Server.Shutdown for the whole drain budget (see [Drain]). A nil
	// channel (no Drain wired) never becomes ready.
	draining := opts.Drain.Done()
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	expiry := time.NewTimer(streamLifetime(opts.MaxLifetime))
	defer expiry.Stop()

	// Initial flush so the client sees the response start immediately; some clients time out
	// otherwise. retry: precedes :connected so a client that only reads the first frame still
	// gets the reconnection-delay hint.
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

// setSSEHeaders writes the WHATWG SSE headers before WriteHeader. Cache-Control is a fallback only:
// the v1 CacheControl middleware already set the per-route policy, and overwriting it would turn
// no-store into a storable response and drop `private`.
func setSSEHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// endStreamForRevocation ends a stream whose credential, key policy or quota no longer admits it.
// The reconnect gets the 401/403/429 from the middleware, which a conforming EventSource treats as
// a stop.
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

// endStreamByServer ends a stream the SERVER is closing (shutdown drain or lifetime expiry) with a
// final comment frame, so the chunked body terminates and clients reconnect normally instead of
// the proxy logging a truncated response. A comment keeps the event vocabulary in
// openapi/stellar-index.v1.yaml unchanged. The write gets [drainWriteDeadline], not
// [streamWriteDeadline], so a stalled client cannot hold the drain (cost is the maximum across
// streams, not the sum). A failed write is ignored: the connection is going away anyway.
func endStreamByServer(w http.ResponseWriter, flusher http.Flusher, rc *http.ResponseController, frame string) {
	_ = rc.SetWriteDeadline(time.Now().Add(drainWriteDeadline))
	if _, err := fmt.Fprint(w, frame); err != nil {
		return
	}
	flusher.Flush()
}

// LastEventIDFrom returns the resume cursor: the `Last-Event-ID` header, or `?last_event_id=` for
// clients that cannot set headers (browser EventSource sends the header only on reconnect, not on
// the initial connection after a page reload). Exported for non-Hub endpoints.
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
// Each `data:` line ends with \n per the SSE spec; the trailing \n separates frames. WriteFrame does
// NOT flush; direct callers (custom event loops) must.
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
