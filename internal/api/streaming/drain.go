package streaming

import "sync"

// Drain is the one-shot "this server is shutting down" broadcast that
// the SSE writer watches ALONGSIDE the request context.
//
// Why it exists. http.Server.Shutdown waits for every active
// connection to become IDLE, and an SSE connection is never idle: it
// holds an open response for as long as the client keeps reading.
// r.Context() is cancelled when the CLIENT goes away, not when the
// server shuts down, so nothing in the stream writer's event loop ever
// learns that a drain started. One attached stream therefore pins the
// listener drain for the entire shutdown budget, after which Shutdown
// returns "context deadline exceeded" and the process exits on top of
// connections that were still open — the client gets a truncated
// response, every ordinary request pays the whole budget as deploy
// downtime, and the background-worker wait that shares the deadline
// gets nothing at all.
//
// Measured on r1 (2026-09-15), same binary, two restarts: 30.18s with
// one browser on /v1/ledger/stream, 0.21s with none.
//
// Why NOT http.Server.BaseContext. Deriving every request context from
// the process root context would end the stream problem by cancelling
// every in-flight request the instant SIGTERM lands — an abrupt
// teardown for the overwhelming majority of traffic that is not a
// stream, traded for a tidy one on the few connections that are. Drain
// is scoped to the writers that actually need it and is invisible to
// every other handler.
//
// Wiring. One Drain per server; pass Begin to
// http.Server.RegisterOnShutdown so it fires exactly when Shutdown
// starts, and hand the Drain to the stream writers through
// [StreamOptions]. Begin and Done are safe on a nil receiver and on
// the zero value: both report a nil channel, which in a select blocks
// forever, so a caller that wires no Drain behaves exactly as it did
// before this type existed. Use NewDrain for one that can actually
// fire.
type Drain struct {
	once sync.Once
	ch   chan struct{}
}

// NewDrain returns a Drain that has not yet begun.
func NewDrain() *Drain { return &Drain{ch: make(chan struct{})} }

// Begin broadcasts "shutting down" to every watcher. It is idempotent
// and safe to call concurrently, from any goroutine — which matters
// because http.Server.Shutdown invokes each registered hook on a
// goroutine of its own.
func (d *Drain) Begin() {
	if d == nil || d.ch == nil {
		return
	}
	d.once.Do(func() { close(d.ch) })
}

// Done returns a channel closed by the first [Drain.Begin]. A nil
// *Drain (and the zero value) returns nil, which never becomes ready —
// the "no drain wired" case.
func (d *Drain) Done() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.ch
}
