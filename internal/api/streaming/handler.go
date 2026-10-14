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

// streamWriteDeadline bounds a single SSE write, rolled forward before every write; it fails only a write that BLOCKS (a client that stops
// reading is bounded by
// [DefaultMaxStreamLifetime]). Must exceed the heartbeat interval so a slow-but-alive client is not killed.
const streamWriteDeadline = 25 * time.Second

// DefaultMaxStreamLifetime is how long one SSE connection is served before the server ends it and the client reconnects; it bounds a TCP-alive client that never reads, with up to 10% jitter per stream to avoid reconnect stampedes.
const DefaultMaxStreamLifetime = 30 * time.Minute

// drainWriteDeadline bounds the courtesy frame written when the SERVER ends a stream; far shorter
// than streamWriteDeadline (see endStreamByServer).
const drainWriteDeadline = time.Second

// maxConcurrentStreams caps simultaneous SSE connections across all stream endpoints so a flood
// cannot exhaust file descriptors or goroutines. <= 0 disables the cap.
var maxConcurrentStreams int64 = 8192

var activeStreams int64

var rejectedStreams int64

// SetMaxConcurrentStreams overrides the global SSE connection cap (<= 0 disables); call at startup.
func SetMaxConcurrentStreams(n int64) { atomic.StoreInt64(&maxConcurrentStreams, n) }

// StreamsRejected reports the cumulative SSE connections refused by the concurrency caps since process start.
func StreamsRejected() int64 { return atomic.LoadInt64(&rejectedStreams) }

// TryAcquireStreamSlot is [admitStream] exported for callers with expensive pre-flight work; release is idempotent and MUST be called when the stream ends, and pre-admitting callers MUST use [StreamFromChannelPreAdmitted] or a SECOND slot is acquired.
func TryAcquireStreamSlot(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	return admitStream(w, r)
}

// admitStream reserves one slot against the global and per-IP caps, writing the refusal itself (503 on the global cap, 429 on the per-address cap). It runs BEFORE [Hub.Subscribe]
// (client-supplied topic key)
// so the caps bound Hub memory, not just sockets: a refused connection must never mint a topic. The returned release is idempotent.
func admitStream(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	releaseGlobal, ok := acquireGlobalStreamSlot()
	if !ok {
		atomic.AddInt64(&rejectedStreams, 1)
		obs.APISSEStreamsRejectedTotal.WithLabelValues("global_cap").Inc()
		http.Error(w, "too many concurrent streams", http.StatusServiceUnavailable)
		return nil, false
	}
	// Per-IP cap: the global cap alone lets one address hold the whole budget.
	releaseIP, ok := acquireIPStreamSlot(r)
	if !ok {
		releaseGlobal()
		atomic.AddInt64(&rejectedStreams, 1)
		obs.APISSEStreamsRejectedTotal.WithLabelValues("per_ip_cap").Inc()
		http.Error(w, "too many concurrent streams from your address", http.StatusTooManyRequests)
		return nil, false
	}
	return func() {
		releaseIP()
		releaseGlobal()
	}, true
}

// acquireGlobalStreamSlot reserves one slot against [maxConcurrentStreams]. A cap of <= 0
// disables the ceiling but still counts the stream, so activeStreams stays truthful.
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

// DefaultHeartbeatInterval is the cadence of SSE comment heartbeats when no events flow; every Caddy timeout on the stream path must be at least two of these (internal/config/caddy_sse_timeouts_test.go).
const DefaultHeartbeatInterval = 15 * time.Second

// DefaultRetry is the SSE `retry:` reconnection-delay hint sent once in the prelude, so a deploy roll does not cause a reconnect stampede.
const DefaultRetry = 5000 * time.Millisecond

// StreamOptions tunes [Stream] behaviour; zero values use defaults.
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

// Stream wires an http.ResponseWriter into the Hub for the topics: SSE headers, replay from Last-Event-ID (header wins over `?last_event_id=`), live forwarding until the request context cancels, and comment heartbeats.
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

// WriteSubscribeRefused answers a connection whose [Hub.Subscribe] failed with [ErrTopicCapacity]; call it before any SSE header is sent.
func WriteSubscribeRefused(w http.ResponseWriter) {
	atomic.AddInt64(&rejectedStreams, 1)
	obs.APISSEStreamsRejectedTotal.WithLabelValues("topic_cap").Inc()
	http.Error(w, "too many concurrent stream topics", http.StatusTooManyRequests)
}

// StreamFromChannel is the lower-level SSE writer for any receive-only event channel (events computed per connection); it returns when the request context cancels or the caller closes ch.
func StreamFromChannel(w http.ResponseWriter, r *http.Request, ch <-chan Event, opts StreamOptions) {
	release, ok := admitStream(w, r)
	if !ok {
		return
	}
	defer release()
	writeStream(w, r, ch, opts)
}

// StreamFromChannelPreAdmitted is [StreamFromChannel] for a caller that already holds a slot via [TryAcquireStreamSlot]; it neither acquires nor releases one, the caller owns that lifecycle.
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

	// The server's fixed 30s WriteTimeout would reset an SSE stream and clearing it would let a
	// blocked write hang forever, so a per-write deadline is rolled forward before every write.
	// http.ErrNotSupported (httptest, wrappers without Unwrap) is ignored: those enforce none.
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
				// Producer closed the channel; the client resumes via Last-Event-ID.
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

// setSSEHeaders writes the WHATWG SSE headers before WriteHeader. Cache-Control is a fallback only: overwriting the CacheControl
// middleware's policy would turn no-store into storable.
func setSSEHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// endStreamForRevocation ends a stream whose credential, key policy or quota no longer admits it; the reconnect gets the 401/403/429 from
// the middleware, which EventSource treats as a stop.
func endStreamForRevocation(w http.ResponseWriter, flusher http.Flusher, setWriteDeadline func()) {
	setWriteDeadline()
	if _, err := fmt.Fprint(w, ":revoked\n\n"); err != nil {
		return
	}
	flusher.Flush()
}

// streamLifetime returns maxLifetime (default when zero) plus up to 10% random jitter.
func streamLifetime(maxLifetime time.Duration) time.Duration {
	if maxLifetime <= 0 {
		maxLifetime = DefaultMaxStreamLifetime
	}
	if jitter := maxLifetime / 10; jitter > 0 {
		maxLifetime += rand.N(jitter)
	}
	return maxLifetime
}

// endStreamByServer ends a server-closed stream (drain or lifetime expiry) with a final comment frame, so the chunked body terminates
// cleanly and the OpenAPI event vocabulary is unchanged.
// The short [drainWriteDeadline] stops a stalled client holding the drain; a failed write is ignored.
func endStreamByServer(w http.ResponseWriter, flusher http.Flusher, rc *http.ResponseController, frame string) {
	_ = rc.SetWriteDeadline(time.Now().Add(drainWriteDeadline))
	if _, err := fmt.Fprint(w, frame); err != nil {
		return
	}
	flusher.Flush()
}

// LastEventIDFrom returns the resume cursor: the `Last-Event-ID` header, else `?last_event_id=` for clients that cannot set headers; exported for non-Hub endpoints.
func LastEventIDFrom(r *http.Request) string {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		return v
	}
	return r.URL.Query().Get("last_event_id")
}

// WriteFrame emits one SSE frame (retry, id, event, one data line per \n in Data, blank terminator) to w; it does NOT flush, so direct callers (custom event loops) must.
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
