// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// afterResponseWorkers / afterResponseQueueSize size the shared pool
// [AfterResponse] hands post-response bookkeeping to (GH-627): usage
// metering and TouchUsage's last-seen update. Generous enough that a
// healthy Redis/Postgres never saturates it under ordinary request
// volume; a sustained saturation is a signal in its own right (see
// [obs.AfterResponseTasksDroppedTotal]), not something to size around.
const (
	afterResponseWorkers   = 64
	afterResponseQueueSize = 4096
)

// afterResponsePool is the process-wide shared pool. One pool, not one
// per middleware, so TouchUsage and UsageTracker share a single bound on
// how much post-response work can be in flight at once.
var afterResponsePool = newAfterResponsePool(afterResponseWorkers, afterResponseQueueSize)

// afterResponsePoolT is a bounded fan-out of fire-and-forget tasks onto a
// fixed set of long-lived goroutines. wg tracks tasks that have been
// accepted (queued or running) but not yet finished, so tests can drain
// it deterministically instead of racing the async completion.
type afterResponsePoolT struct {
	tasks chan func()
	wg    sync.WaitGroup
}

func newAfterResponsePool(workers, queueSize int) *afterResponsePoolT {
	p := &afterResponsePoolT{tasks: make(chan func(), queueSize)}
	for i := 0; i < workers; i++ {
		go func() {
			// Belt-and-braces alongside run's own per-task recover: run
			// already keeps one bad task from taking a worker down, but a
			// panic anywhere else in loop (not inside a task) would still
			// need this to avoid taking the whole process down with it.
			defer worker.Recover(nil, "api-after-response-pool")
			p.loop()
		}()
	}
	return p
}

func (p *afterResponsePoolT) loop() {
	for task := range p.tasks {
		p.run(task)
	}
}

// run executes one task with its own recover, so a panicking task logs
// and dies without taking its worker goroutine (and therefore a slot of
// shared pool capacity) down with it — the pool must keep serving every
// OTHER caller's usage/touch bookkeeping after one bad task.
func (p *afterResponsePoolT) run(task func()) {
	defer p.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			worker.Report(nil, "api-after-response-pool", r)
		}
	}()
	task()
}

// submit enqueues task. wg.Add happens before the enqueue attempt (and
// therefore before the caller's subsequent Flush) so a test waiting on
// [AfterResponseDrainForTest] always observes the task as outstanding,
// never as "not yet submitted". Returns false — without ever blocking —
// when the queue is full; the caller counts that as a drop.
func (p *afterResponsePoolT) submit(task func()) bool {
	p.wg.Add(1)
	select {
	case p.tasks <- task:
		return true
	default:
		p.wg.Done()
		return false
	}
}

// AfterResponse flushes w — so the client sees the already-written
// response immediately instead of waiting for bookkeeping the client has
// no interest in — then hands fn to the shared bounded worker pool
// instead of running it inline on the request goroutine (GH-627: TouchUsage
// and UsageTracker used to run synchronously post-handler under
// context.WithoutCancel(r.Context()) + a 5 s bound each, so a wedged
// Redis added up to 10 s to a request net/http had already buffered but
// never flushed, entirely outside api.request_timeout).
//
// fn must not read or write w or the request — by the time fn runs, the
// response may already be on the wire and the request's context may be
// reused or gone. fn gets its own context (typically
// context.WithTimeout(context.Background(), postResponseWriteTimeout)) —
// never context.WithoutCancel(r.Context()), which is exactly the
// pattern this replaces.
//
// If the pool is saturated, fn is dropped (never blocks the request
// goroutine) and [obs.AfterResponseTasksDroppedTotal] is incremented —
// silently losing a usage row or a last-seen touch must leave an
// operator-visible signal.
func AfterResponse(w http.ResponseWriter, fn func()) {
	submitAfterResponseTask(fn)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// submitAfterResponseTask hands fn to the shared pool without touching
// the response writer. Used by [AfterResponse] on the ordinary path, and
// directly by callers whose response is NOT yet known-complete — a
// panicking handler, where the outer Recoverer still has to write the
// 500 and a premature Flush here would lock the status in at 200 first
// (GH-627).
func submitAfterResponseTask(fn func()) {
	if !afterResponsePool.submit(fn) {
		obs.AfterResponseTasksDroppedTotal.Inc()
	}
}

// AfterResponseDrainForTest blocks until every task submitted to the
// shared after-response pool so far has completed, or timeout elapses.
// Reports whether it drained in time. Test-only synchronization point:
// [AfterResponse] hands work to background goroutines, so a test that
// asserts on the effect of that work (a usage counter, a touch) must
// wait for it rather than reading immediately after ServeHTTP returns.
func AfterResponseDrainForTest(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer worker.Recover(nil, "api-after-response-drain-test")
		afterResponsePool.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
