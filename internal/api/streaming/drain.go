package streaming

import "sync"

// Drain is the one-shot "server is shutting down" broadcast the SSE writer watches alongside the request context, so attached streams cannot pin http.Server.Shutdown; Begin and Done are safe on a nil receiver and the zero value (never fires), and NewDrain builds one that can.
type Drain struct {
	once sync.Once
	ch   chan struct{}
}

// NewDrain returns a Drain that has not yet begun.
func NewDrain() *Drain { return &Drain{ch: make(chan struct{})} }

// Begin broadcasts "shutting down" to every watcher; idempotent and safe to call concurrently from any goroutine.
func (d *Drain) Begin() {
	if d == nil || d.ch == nil {
		return
	}
	d.once.Do(func() { close(d.ch) })
}

// Done returns a channel closed by the first [Drain.Begin]; a nil *Drain or zero value returns nil, which never becomes ready.
func (d *Drain) Done() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.ch
}
