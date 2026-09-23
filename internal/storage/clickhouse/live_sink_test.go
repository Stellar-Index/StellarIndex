package clickhouse

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// stalledConn models a wedged ClickHouse: every batch prepare blocks until
// its ctx ends. Methods the LiveSink never reaches stay nil and would panic.
type stalledConn struct{ driver.Conn }

func (stalledConn) PrepareBatch(ctx context.Context, _ string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (stalledConn) Close() error { return nil }

// TestLiveSink_StopBoundedByStopTimeout — #1018. Stop used to run the final
// flush and then Close (which flushes again), each on a fresh WriteTimeout,
// after any flush already in flight: up to 3 × WriteTimeout (90s in
// production) against a wedged ClickHouse, past systemd's stop timeout.
func TestLiveSink_StopBoundedByStopTimeout(t *testing.T) {
	l := newLiveSink(&Sink{conn: stalledConn{}, flushEvery: 1000},
		slog.New(slog.DiscardHandler), LiveSinkOptions{
			BufferSize:    8,
			FlushInterval: time.Hour,
			WriteTimeout:  2 * time.Second,
			StopTimeout:   200 * time.Millisecond,
		})
	l.Start()
	l.PushLedger(LedgerExtract{Ledger: LedgerRow{LedgerSeq: 7}})

	start := time.Now()
	l.Stop()
	elapsed := time.Since(start)

	// Unbounded, the final flush plus Close's flush take 2 × WriteTimeout = 4s.
	if elapsed > time.Second {
		t.Fatalf("Stop took %v; want it bounded by StopTimeout (200ms), not a WriteTimeout per phase", elapsed)
	}
	if got := l.ErroredCount(); got == 0 {
		t.Errorf("ErroredCount = 0; the unflushed shutdown ledger must be counted, not discarded silently")
	}
}
