package streaming

import "sync"

// Drain is the one-shot "this server is shutting down" broadcast that the SSE
// writer watches alongside the request context.
//
// http.Server.Shutdown waits for every connection to go idle, and an SSE
// connection never does; r.Context() is cancelled when the CLIENT leaves, not on
// server shutdown. Without Drain one attached stream pins the listener drain for
// the whole shutdown budget (measured on r1: 30.18s with one browser on
// /v1/ledger/stream, 0.21s with none).
//
// Not http.Server.BaseContext: that would cancel every in-flight request the
// instant SIGTERM lands, not just the few streams.
//
// One Drain per server: pass Begin to http.Server.RegisterOnShutdown and hand
// the Drain to the stream writers through [StreamOptions]. Begin and Done are
// safe on a nil receiver and the zero value (a nil channel, which blocks forever
// in a select), so a caller that wires no Drain is never told to drain. Use
// NewDrain for one that can fire.
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
