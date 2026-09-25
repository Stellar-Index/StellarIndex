// Shared SSE stream multiplexer — the transport layer of the live-tick
// program (RT-2, operator directive 2026-08-08: "most pages updating
// in real time … should feel alive").
//
// One EventSource per URL, shared by every subscriber on the page:
// twelve components watching the ledger tick cost ONE connection, and
// the server's per-IP stream cap (20) is never burned by a single tab.
// Subscriptions are refcounted; the underlying connection closes a
// short linger after the last subscriber unmounts (absorbing
// route transitions where the next page re-subscribes immediately).
//
// Reconnect policy (WB-04, proven on the status page): the browser
// auto-retries transient blips itself (readyState CONNECTING). A HARD
// failure (readyState CLOSED — e.g. a 404 from an older API binary, or
// a proxy strip) is NOT retried by the browser, so we schedule a slow
// reopen instead of hammering a dead endpoint.

export const STREAM_REOPEN_MS = 60_000;

// How long a stream object survives with zero subscribers before the
// connection is torn down. Long enough for a route transition.
const STREAM_LINGER_MS = 5_000;

type Listener = (data: string) => void;

// Connection status surfaced to subscribers so a page can render a
// "reconnecting"/stale badge instead of a value that has silently gone
// dark (GH-1038). 'reconnecting' covers both the initial connect and
// the post-hard-failure wait for STREAM_REOPEN_MS.
export type StreamStatus = 'live' | 'reconnecting';
type StatusListener = (status: StreamStatus) => void;

interface SharedStream {
  es: EventSource | null;
  refs: number;
  // eventType → listeners. One EventSource can carry several event
  // types (a subscriber names the one it wants).
  listeners: Map<string, Set<Listener>>;
  // Event types already attached to the current EventSource object.
  attached: Set<string>;
  reopenTimer: ReturnType<typeof setTimeout> | null;
  lingerTimer: ReturnType<typeof setTimeout> | null;
  // Highest `id:` seen on this connection (lexicographically —
  // server IDs are fixed-width hex, sortable = chronological). Sent
  // back as `?last_event_id=` on reopen so a hard failure resumes
  // instead of dropping everything published during the outage.
  lastEventId: string | null;
  status: StreamStatus;
  statusListeners: Set<StatusListener>;
}

const streams = new Map<string, SharedStream>();

// The server accepts the resume cursor as either the `Last-Event-ID`
// header (which EventSource sends automatically on its own transient
// reconnects) or `?last_event_id=` (LastEventIDFrom,
// internal/api/streaming/handler.go) — the fallback we need here
// since we're opening a brand-new EventSource, not resuming one.
function withLastEventId(url: string, lastEventId: string | null): string {
  if (!lastEventId) return url;
  const sep = url.includes('?') ? '&' : '?';
  return `${url}${sep}last_event_id=${encodeURIComponent(lastEventId)}`;
}

function setStatus(s: SharedStream, status: StreamStatus): void {
  if (s.status === status) return;
  s.status = status;
  for (const fn of s.statusListeners) fn(status);
}

function connect(url: string, s: SharedStream): void {
  const es = new EventSource(withLastEventId(url, s.lastEventId));
  s.es = es;
  s.attached = new Set();
  for (const eventType of s.listeners.keys()) attach(s, eventType);
  es.onopen = () => {
    if (s.es === es) setStatus(s, 'live');
  };
  es.onerror = () => {
    if (s.es === es && es.readyState === EventSource.CLOSED) {
      s.es = null;
      setStatus(s, 'reconnecting');
      if (s.refs > 0 && !s.reopenTimer) {
        s.reopenTimer = setTimeout(() => {
          s.reopenTimer = null;
          if (s.refs > 0 && !s.es) connect(url, s);
        }, STREAM_REOPEN_MS);
      }
    }
  };
}

function attach(s: SharedStream, eventType: string): void {
  if (!s.es || s.attached.has(eventType)) return;
  s.attached.add(eventType);
  s.es.addEventListener(eventType, (ev) => {
    const msgEv = ev as MessageEvent;
    const id = msgEv.lastEventId;
    // Out-of-order guard (#720): a transient reconnect the browser
    // handled itself can still redeliver/reorder frames. Drop
    // anything that isn't strictly newer than what we've already
    // forwarded rather than let a stale frame overwrite a fresh one.
    if (id && s.lastEventId !== null && id <= s.lastEventId) return;
    if (id) s.lastEventId = id;
    const set = s.listeners.get(eventType);
    if (!set) return;
    for (const fn of set) fn(msgEv.data as string);
  });
}

function teardown(url: string, s: SharedStream): void {
  if (s.reopenTimer) clearTimeout(s.reopenTimer);
  if (s.lingerTimer) clearTimeout(s.lingerTimer);
  s.es?.close();
  streams.delete(url);
}

/**
 * subscribeStream — receive the raw `data:` payload of every `eventType`
 * frame on the SSE endpoint at `url`. Returns an unsubscribe function
 * (call exactly once, e.g. as a useEffect cleanup). Safe to call from
 * many components with the same url: they share one connection.
 */
export function subscribeStream(
  url: string,
  eventType: string,
  onData: Listener,
  onStatus?: StatusListener,
): () => void {
  // No-op outside a browser with SSE support (jsdom test environments,
  // any server-side render path): callers simply never receive frames
  // and their fallbacks carry the page.
  if (typeof EventSource === 'undefined') return () => {};
  let s = streams.get(url);
  if (!s) {
    s = {
      es: null,
      refs: 0,
      listeners: new Map(),
      attached: new Set(),
      reopenTimer: null,
      lingerTimer: null,
      lastEventId: null,
      status: 'reconnecting',
      statusListeners: new Set(),
    };
    streams.set(url, s);
  }
  if (s.lingerTimer) {
    clearTimeout(s.lingerTimer);
    s.lingerTimer = null;
  }
  s.refs++;
  let set = s.listeners.get(eventType);
  if (!set) {
    set = new Set();
    s.listeners.set(eventType, set);
  }
  set.add(onData);
  if (onStatus) {
    s.statusListeners.add(onStatus);
    onStatus(s.status);
  }
  if (!s.es) {
    if (s.reopenTimer) {
      clearTimeout(s.reopenTimer);
      s.reopenTimer = null;
    }
    connect(url, s);
  } else {
    attach(s, eventType);
  }

  let released = false;
  return () => {
    if (released) return;
    released = true;
    const cur = streams.get(url);
    if (cur !== s) return;
    set.delete(onData);
    if (onStatus) s.statusListeners.delete(onStatus);
    s.refs--;
    if (s.refs > 0) return;
    s.lingerTimer = setTimeout(() => teardown(url, s), STREAM_LINGER_MS);
  };
}

/** Test hook: number of live shared streams (open or lingering). */
export function activeStreamCount(): number {
  return streams.size;
}

/** Test hook: force-close everything immediately. */
export function resetStreamsForTest(): void {
  for (const [url, s] of streams) teardown(url, s);
}
