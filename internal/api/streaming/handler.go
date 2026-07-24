package streaming

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// streamWriteDeadline bounds a single SSE write. It's rolled forward
// before every write, so a healthy stream (which writes a heartbeat every
// HeartbeatInterval < this) never trips it, but a STALLED write — a
// non-reading or zero-window client — fails after this long, letting the
// handler return and free its goroutine/conn/FD (CS-013). Must be > the
// heartbeat interval so a slow-but-alive client isn't killed.
const streamWriteDeadline = 25 * time.Second

// maxConcurrentStreams caps simultaneous SSE connections across all stream
// endpoints, so a flood of connections can't exhaust file descriptors /
// goroutines (CS-013 / F4). Generous by default (legit fan-out is small on
// a single host); tune via SetMaxConcurrentStreams. <= 0 disables the cap.
var maxConcurrentStreams int64 = 8192

var activeStreams int64

var rejectedStreams int64

// SetMaxConcurrentStreams overrides the global concurrent-SSE-connection
// cap. Pass <= 0 to disable. Call once at startup.
func SetMaxConcurrentStreams(n int64) { atomic.StoreInt64(&maxConcurrentStreams, n) }

// ActiveStreams reports the current number of open SSE connections (for
// diagnostics / a gauge).
func ActiveStreams() int64 { return atomic.LoadInt64(&activeStreams) }

// StreamsRejected reports the cumulative number of SSE connections
// refused by the concurrency caps (global or per-IP) since process
// start — the counter that makes a connection flood visible, next to
// the [ActiveStreams] gauge.
func StreamsRejected() int64 { return atomic.LoadInt64(&rejectedStreams) }

// TryAcquireStreamSlot reserves one connection slot against the
// global and per-IP concurrency caps, writing the 503 itself when
// either refuses. It is [admitStream] exported for callers OUTSIDE
// this package whose own pre-flight work (before switching into SSE
// mode) is itself expensive.
//
// REL-05 (pre-flight-compute ordering): [StreamFromChannel] already
// admits before doing anything else, but a caller like
// handleObservationsStream runs its OWN synchronous compute (the
// initial event) BEFORE ever calling StreamFromChannel — so a client
// already at its concurrency cap still paid for that full compute
// before being rejected. Calling TryAcquireStreamSlot at the very top
// of the handler, before that compute, closes the gap: admission is
// now the very first thing that happens, full stop.
//
// The returned release MUST be called exactly once (typically via
// `defer release()` immediately after a successful acquire, covering
// every return path — validation errors, a failed pre-flight compute,
// and the eventual stream teardown alike); it is idempotent, so it is
// safe to also flow it into [StreamFromChannelPreAdmitted] or let a
// deferred call and an explicit one both fire. Callers that pre-admit
// this way MUST switch their eventual stream call from
// [StreamFromChannel] to [StreamFromChannelPreAdmitted] — the plain
// [StreamFromChannel] would acquire a SECOND slot for the same
// connection.
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
// mint a topic (REL-05).
//
// The returned release MUST be called exactly once when the stream
// ends; it is idempotent.
func admitStream(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	releaseGlobal, ok := acquireGlobalStreamSlot()
	if !ok {
		atomic.AddInt64(&rejectedStreams, 1)
		http.Error(w, "too many concurrent streams", http.StatusServiceUnavailable)
		return nil, false
	}
	// Per-IP cap (C3-8): the global cap alone lets one client hold the
	// entire budget, so a single stalled/hostile address can starve the
	// streams for everyone. Give each client its own small ceiling.
	releaseIP, ok := acquireIPStreamSlot(r)
	if !ok {
		releaseGlobal()
		atomic.AddInt64(&rejectedStreams, 1)
		http.Error(w, "too many concurrent streams from your address", http.StatusServiceUnavailable)
		return nil, false
	}
	return func() {
		releaseIP()
		releaseGlobal()
	}, true
}

// acquireGlobalStreamSlot reserves one slot against
// [maxConcurrentStreams] (CS-013), so a connection flood can't exhaust
// FDs/goroutines. A cap of <= 0 disables the ceiling but still counts
// the stream, so [ActiveStreams] stays truthful.
func acquireGlobalStreamSlot() (release func(), ok bool) {
	if limit := atomic.LoadInt64(&maxConcurrentStreams); limit > 0 {
		if atomic.AddInt64(&activeStreams, 1) > limit {
			atomic.AddInt64(&activeStreams, -1)
			return nil, false
		}
	} else {
		atomic.AddInt64(&activeStreams, 1)
	}
	var once sync.Once
	return func() {
		once.Do(func() { atomic.AddInt64(&activeStreams, -1) })
	}, true
}

// DefaultHeartbeatInterval is the cadence at which Stream emits
// SSE comment heartbeats (`:keepalive\n\n`) when no real events are
// flowing. 15 s matches the api-design.md note and is well under
// the typical 60 s reverse-proxy idle timeout — which is what we're
// trying to dodge by sending these.
const DefaultHeartbeatInterval = 15 * time.Second

// StreamOptions tunes [Stream] behaviour. Zero values use sensible
// defaults so most callers can pass `StreamOptions{}`.
type StreamOptions struct {
	// HeartbeatInterval is the no-event cadence for SSE comment
	// heartbeats. Zero = DefaultHeartbeatInterval. Tests may want a
	// faster value to keep wall-clock test time short.
	HeartbeatInterval time.Duration
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
// Stream is the convenience constructor for Hub-driven endpoints
// (the closed-bucket /v1/price/stream). Per-connection-tick
// endpoints (/v1/price/tip/stream, /v1/observations/stream) bypass
// the Hub and feed events through [StreamFromChannel] directly.
func Stream(w http.ResponseWriter, r *http.Request, hub *Hub, topics []string, opts StreamOptions) {
	// Admission FIRST. hub.Subscribe allocates a Hub topic keyed by
	// `topics` — client-controlled on /v1/price/stream — so a
	// connection the caps are going to refuse must never get that far;
	// otherwise the caps bound sockets while the topic map grows
	// unchecked (REL-05).
	release, ok := admitStream(w, r)
	if !ok {
		return
	}
	defer release()

	ch, cancel := hub.Subscribe(topics, LastEventIDFrom(r))
	defer cancel()
	writeStream(w, r, ch, opts)
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
// pre-flight compute that must run AFTER admission, not before
// (REL-05). Unlike StreamFromChannel, this does NOT acquire (or
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

	// F-1228 + CS-013: the API's http.Server sets `WriteTimeout: 30s` to
	// keep short handlers honest, but that fixed deadline would reset an
	// SSE stream at 30s. The old fix cleared the deadline entirely
	// (`SetWriteDeadline(zero)`), which let a stalled write block FOREVER
	// — a non-reading client leaked its goroutine/conn/FD indefinitely.
	// Instead we ROLL a per-write deadline forward before every write
	// (see setWriteDeadline). A healthy stream heartbeats within the
	// window and never trips it; a stalled write fails after
	// streamWriteDeadline and the handler returns + cleans up.
	//
	// On transports that don't expose SetWriteDeadline (httptest writers,
	// wrappers without Unwrap) the call returns http.ErrNotSupported,
	// which we ignore — those transports don't enforce write deadlines
	// anyway. Production wrappers all expose Unwrap().
	rc := http.NewResponseController(w)
	setWriteDeadline := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteDeadline))
	}

	// SSE headers per WHATWG. Setting these BEFORE WriteHeader so
	// the first frame goes out cleanly. X-Accel-Buffering disables
	// nginx response buffering; Connection: keep-alive is implicit
	// in HTTP/1.1 and harmless on HTTP/2.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	heartbeat := opts.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = DefaultHeartbeatInterval
	}

	ctx := r.Context()
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	// Initial flush so the client sees the response start
	// immediately rather than waiting for the first event. Some
	// clients deadlock if the server hasn't written headers + flushed
	// before they time out.
	setWriteDeadline()
	if _, err := fmt.Fprint(w, ":connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
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
			setWriteDeadline()
			if _, err := fmt.Fprint(w, ":keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
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
