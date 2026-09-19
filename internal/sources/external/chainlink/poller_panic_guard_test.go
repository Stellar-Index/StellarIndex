package chainlink

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// K012 — PollOnce fans out one detached goroutine per feed plus a join
// goroutine, and an unrecovered panic in ANY goroutine terminates the WHOLE
// process. Without the guard this test does not fail, it CRASHES the test
// binary; that crash is the production harm.
//
// The assertion is not merely "it survived". A panicking feed that is only
// CONTAINED disappears from `results` entirely, so the tick ends with zero
// updates and zero errors — which PollOnce's own G10-02 comment calls a
// healthy skip, bumping ExternalPollerLastSuccessUnix and leaving the
// staleness gauge green over a poller that is actually dead. So the panic
// must be reported as a per-feed FAILURE, and an all-feeds-panicking tick
// must surface an error.
//
// The panic is the real one a struct-literal Poller produces: resolveDecimals
// dereferences p.decimals, which only NewPoller populates.
func TestPollOnce_PanickingFeedIsReportedNotSilentlySkipped(t *testing.T) {
	const workerName = "external-chainlink-feed-poll"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	pair := testPair("BTC", "USD")
	p := &Poller{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		FeedMap:     map[string]FeedSpec{pair.String(): {Address: "0xfeed", Decimals: 8}},
		Cache:       newRoundCache(),
		Concurrency: 1,
		// decimals deliberately left nil — the mis-construction whose
		// nil dereference stands in for any panic inside a feed read.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	var (
		trades  []canonical.Trade
		updates []canonical.OracleUpdate
		err     error
	)
	go func() {
		trades, updates, err = p.PollOnce(ctx, []canonical.Pair{pair})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("PollOnce never returned: the join goroutine contained its panic without " +
			"closing the results channel, so the fan-in ranges forever — close from a defer")
	}

	if err == nil {
		t.Fatalf("a tick in which every feed panicked returned err=nil (trades=%v updates=%v): "+
			"the runner reads that as a healthy skip and keeps the staleness gauge green "+
			"over a poller that reported nothing at all", trades, updates)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("PollOnce err = %v, want the per-feed panic surfaced as that feed's failure", err)
	}
	if len(updates) != 0 {
		t.Errorf("PollOnce returned %d update(s) from a panicking feed, want 0", len(updates))
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v — a recover "+
			"that does not move the counter turns a loud crash into a silent dead feed",
			workerName, after, before+1)
	}
}
