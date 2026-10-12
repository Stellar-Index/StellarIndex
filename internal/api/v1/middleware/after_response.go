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

// afterResponseWorkers / afterResponseQueueSize size the pool for usage metering and last-seen touches. Sustained saturation is
// a signal in itself ([obs.AfterResponseTasksDroppedTotal]), not something to size around.
const (
	afterResponseWorkers   = 64
	afterResponseQueueSize = 4096
)

// afterResponsePool is the process-wide pool: one, so TouchUsage and UsageTracker share a single bound on in-flight post-response work.
var afterResponsePool = newAfterResponsePool(afterResponseWorkers, afterResponseQueueSize)

// afterResponsePoolT fans fire-and-forget tasks onto fixed long-lived goroutines. wg tracks accepted-but-unfinished tasks
// so tests can drain it deterministically.
type afterResponsePoolT struct {
	tasks   chan func()
	wg      sync.WaitGroup
	workers int
	started sync.Once
}

// newAfterResponsePool starts no goroutines: workers spawn on the first
// submit, so importing this package leaves goleak-checked suites clean.
func newAfterResponsePool(workers, queueSize int) *afterResponsePoolT {
	return &afterResponsePoolT{tasks: make(chan func(), queueSize), workers: workers}
}

func (p *afterResponsePoolT) start() {
	for i := 0; i < p.workers; i++ {
		go func() {
			// Backstop for a panic in loop outside a task; run recovers per-task panics itself.
			defer worker.Recover(nil, "api-after-response-pool")
			p.loop()
		}()
	}
}

func (p *afterResponsePoolT) loop() {
	for task := range p.tasks {
		p.run(task)
	}
}

// run executes one task with its own recover so a panicking task does not kill its worker and
// shrink the shared pool for every other caller.
func (p *afterResponsePoolT) run(task func()) {
	defer p.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			worker.Report(nil, "api-after-response-pool", r)
		}
	}()
	task()
}

// submit enqueues task and returns false, without blocking, when the queue is full. wg.Add runs
// before the enqueue so [AfterResponseDrainForTest] never sees a task as "not yet submitted".
func (p *afterResponsePoolT) submit(task func()) bool {
	p.started.Do(p.start)
	p.wg.Add(1)
	select {
	case p.tasks <- task:
		return true
	default:
		p.wg.Done()
		return false
	}
}

// AfterResponse flushes w then runs fn on the shared bounded pool; fn must not touch w or the request and needs its own bounded context,
// and a saturated pool drops it (counted by [obs.AfterResponseTasksDroppedTotal]).
func AfterResponse(w http.ResponseWriter, fn func()) {
	submitAfterResponseTask(fn)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// submitAfterResponseTask hands fn to the shared pool without flushing, for callers whose response is incomplete (a panicking handler):
// a premature Flush would lock in 200 before the outer Recoverer writes its 500.
func submitAfterResponseTask(fn func()) {
	if !afterResponsePool.submit(fn) {
		obs.AfterResponseTasksDroppedTotal.Inc()
	}
}

// AfterResponseDrainForTest blocks until submitted tasks finish or timeout elapses and reports whether it drained; tests asserting on
// post-response effects must wait on it.
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
