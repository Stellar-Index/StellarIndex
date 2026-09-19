package chainlink

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// NS10 — the per-feed fan-out's panic guard must RELEASE what the feed
// holds, not merely contain the panic.
//
// PollOnce acquires the concurrency semaphore in the CALLER's frame
// (`sem <- struct{}{}` before the `go`) and a WaitGroup slot with it, so a
// recover that swallows the panic without running the `<-sem` and wg.Done
// defers first converts a loud whole-process crash into a silent permanent
// hang: the loop blocks forever handing out the next slot, the join
// goroutine never closes `results`, and the runner's tick never returns.
// That is strictly worse than the crash it replaces, and it is invisible to
// a single-feed test — with one feed the slot is never re-acquired and the
// WaitGroup is never waited on under contention.
//
// So: more feeds than slots, and every one of them panics. The panic is the
// real one a struct-literal Poller produces (resolveDecimals dereferences
// p.decimals, which only NewPoller populates).
func TestPollOnce_PanickingFeedReleasesItsSlotAndWaiter(t *testing.T) {
	const (
		workerName = "external-chainlink-feed-poll"
		feeds      = 4
	)
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	pairs := make([]canonical.Pair, 0, feeds)
	feedMap := make(map[string]FeedSpec, feeds)
	for i := 0; i < feeds; i++ {
		pr := testPair(fmt.Sprintf("TK%d", i), "USD")
		pairs = append(pairs, pr)
		feedMap[pr.String()] = FeedSpec{Address: fmt.Sprintf("0xfeed%d", i), Decimals: 8}
	}

	p := &Poller{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		FeedMap: feedMap,
		Cache:   newRoundCache(),
		// One slot, four feeds: feed 2 cannot start until feed 1's
		// guard has given the slot back.
		Concurrency: 1,
		// decimals deliberately left nil — the mis-construction whose
		// nil dereference stands in for any panic inside a feed read.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	var (
		trades  []canonical.Trade
		updates []canonical.OracleUpdate
		err     error
	)
	go func() {
		trades, updates, err = p.PollOnce(ctx, pairs)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PollOnce never returned with a panicking feed and fewer slots than feeds: " +
			"the guard contained the panic but did not release the concurrency slot / " +
			"WaitGroup slot, so the fan-out wedges the poll tick forever")
	}

	if err == nil {
		t.Fatalf("a tick in which every feed panicked returned err=nil (trades=%v updates=%v): "+
			"the runner reads that as a healthy skip", trades, updates)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("PollOnce err = %v, want the per-feed panic surfaced as that feed's failure", err)
	}
	if len(updates) != 0 || len(trades) != 0 {
		t.Errorf("PollOnce returned %d update(s) / %d trade(s) from panicking feeds, want 0/0",
			len(updates), len(trades))
	}

	// Every feed must be accounted for. A guard that releases the slot but
	// drops the later feeds (or one that lets a feed vanish from `results`)
	// still leaves dead price feeds invisible.
	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+feeds {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v — every one of "+
			"the %d panicking feeds must be reported, not just the first",
			workerName, after, before+feeds, feeds)
	}
}
