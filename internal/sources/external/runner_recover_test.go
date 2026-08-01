package external

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// panicPoller panics on PollOnce — simulates a decode/parse fault
// surfacing inside a poller's tick.
type panicPoller struct{ interval time.Duration }

func (panicPoller) Name() string                  { return "panic-poller" }
func (panicPoller) Class() Class                  { return ClassExchange }
func (p panicPoller) PollInterval() time.Duration { return p.interval }
func (panicPoller) PollOnce(context.Context, []canonical.Pair) ([]canonical.Trade, []canonical.OracleUpdate, error) {
	panic("simulated poller fault in PollOnce")
}

// TestRun_RecoversPanickingPoller proves the W4-cmd-1 fix: a panic in a
// poller's tick (the per-connector goroutine Run fans out) is CONTAINED.
// The poller's immediate first doPoll panics; the guard recovers it, the
// goroutine unwinds, and Run's wait() returns — instead of the panic
// crashing the whole ingest process.
//
// Proven red: without the `defer worker.Recover(...)` added to the
// poller goroutine, the panic below is unrecovered in a DETACHED
// goroutine and takes the entire test binary down. With the guard,
// wait() returns.
func TestRun_RecoversPanickingPoller(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := make(chan consumer.Event, 1)
	p := panicPoller{interval: time.Hour} // long interval — only the immediate poll fires
	wait, err := Run(ctx, nil, []PollerSpec{{Poller: p, Pairs: []canonical.Pair{newTestPair(t)}}}, sink, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	done := make(chan struct{})
	go func() { wait(); close(done) }()

	select {
	case <-done:
		// wait() returned — the poller goroutine recovered its panic and
		// exited instead of crashing the process.
	case <-time.After(3 * time.Second):
		t.Fatal("external.Run wait() did not return after the poller panicked — the per-connector recover is missing")
	}
}
