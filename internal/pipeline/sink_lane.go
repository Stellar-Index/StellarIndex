package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ExternalPersistWorkers is the venue-stream lane's worker count: two batch writers clear every venue's feed and leave the pool to the on-chain lane.
const ExternalPersistWorkers = 2

// externalWriteTimeout bounds one write on the venue-stream lane, whose ctx carries no deadline: a Postgres that hangs
// instead of refusing would otherwise hold a worker for the TCP timeout while the venue streams go unread.
const externalWriteTimeout = 5 * time.Second

// PersistExternalEvents drains the venue streamers' own channel on [ExternalPersistWorkers] workers. On the on-chain
// channel, writes block-retrying a Postgres fault stopped every venue stream being read, and those trades have no cursor
// to replay from. A trade this lane cannot write goes to the bounded retry buffer; no worker waits on Postgres past
// [externalWriteTimeout].
func PersistExternalEvents(ctx context.Context, logger *slog.Logger, store *timescale.Store, in <-chan consumer.Event, late *LateTradeRefresher) ShutdownLoss {
	tw := late.writer(store)
	var extBuf *externalRetryBuffer
	if store != nil {
		// The buffer writes unbounded: its batch after an outage is the whole ring, and it holds no worker.
		extBuf = newExternalRetryBuffer(tw, logger, externalRetryBufferMaxDepth)
	}
	return runSinkLane(ctx, logger, storeEventPersister(logger, store), boundedTradeWriter{tw, externalWriteTimeout}, in, SinkModeAll, ExternalPersistWorkers, extBuf)
}

// boundedTradeWriter gives each write its own deadline.
type boundedTradeWriter struct {
	tradeWriter
	timeout time.Duration
}

func (w boundedTradeWriter) BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error {
	return w.bounded(ctx, func(c context.Context) error { return w.tradeWriter.BatchInsertTrades(c, trades) })
}

func (w boundedTradeWriter) InsertTrade(ctx context.Context, t canonical.Trade) error {
	return w.bounded(ctx, func(c context.Context) error { return w.tradeWriter.InsertTrade(c, t) })
}

// bounded reports the write's own deadline as [writeTimeoutError] while the caller's ctx is still live: left as a ctx
// error the sink would read it as shutdown and carry the batch instead of buffering it.
func (w boundedTradeWriter) bounded(ctx context.Context, write func(context.Context) error) error {
	wctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	err := write(wctx)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return writeTimeoutError{}
	}
	return err
}

// writeTimeoutError is a net.Error so [timescale.IsInfraError] classes it with the other unreachable-database faults.
type writeTimeoutError struct{}

func (writeTimeoutError) Error() string   { return "venue-stream write timed out" }
func (writeTimeoutError) Timeout() bool   { return true }
func (writeTimeoutError) Temporary() bool { return true }

// runSinkLane runs one sink lane: workers drain goroutines over in, plus extBuf's retrier when it is non-nil.
// ep and tw are the two write seams (see persistWorker).
func runSinkLane(ctx context.Context, logger *slog.Logger, ep eventPersister, tw tradeWriter, in <-chan consumer.Event, mode SinkMode, workers int, extBuf *externalRetryBuffer) ShutdownLoss {
	lt := &lossTracker{}
	var bufWG sync.WaitGroup
	var bufCancel context.CancelFunc
	if extBuf != nil {
		// Derived ctx tied to worker lifetime, not ctx: a backfill's ctx is never canceled, so run() would spin and bufWG.Wait() hang.
		// Cancelling it after the workers drain lets run() exit via its fresh-context finalDrain without losing buffered trades.
		var bufCtx context.Context
		bufCtx, bufCancel = context.WithCancel(ctx)
		bufWG.Add(1)
		go func() {
			defer bufWG.Done()
			extBuf.run(bufCtx)
		}()
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			persistWorker(ctx, logger, ep, tw, in, mode, workerID, extBuf, lt)
		}(i)
	}
	wg.Wait()
	// Workers have drained + block-retried their buffers; signal the external
	// buffer to finish its final drain and exit (see bufCtx above), then wait.
	if bufCancel != nil {
		bufCancel()
	}
	bufWG.Wait()
	return lt.snapshot()
}
