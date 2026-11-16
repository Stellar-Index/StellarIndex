package pipeline

import (
	"context"
	"log/slog"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ExternalPersistWorkers is the off-chain lane's worker count: two batch writers clear every venue's feed and leave the pool to the on-chain lane.
const ExternalPersistWorkers = 2

// PersistExternalEvents drains the off-chain connectors' own channel on [ExternalPersistWorkers] workers. Sharing the on-chain
// channel let a write block-retrying a Postgres fault stop every venue stream being read, and those trades have no cursor to
// replay from. Nothing in this lane waits on Postgres: a trade goes to the bounded retry buffer, a poll result is skipped.
func PersistExternalEvents(ctx context.Context, logger *slog.Logger, store *timescale.Store, in <-chan consumer.Event, late *LateTradeRefresher) ShutdownLoss {
	ep := offChainEventPersister(logger, storeEventPersister(logger, store))
	return runSinkLane(ctx, logger, ep, late.writer(store), in, SinkModeAll, ExternalPersistWorkers, store != nil)
}

// offChainEventPersister makes an infra fault on an off-chain poll result a counted skip instead of a blocking retry:
// the next poll supersedes it, and a worker held in retry would stop the venue streams being read.
func offChainEventPersister(logger *slog.Logger, ep eventPersister) eventPersister {
	return func(ctx context.Context, ev consumer.Event, countEvent bool) error {
		err := ep(ctx, ev, countEvent)
		if err == nil || classifyFault(err) != faultInfra {
			return err
		}
		obs.SourceInsertErrorsTotal.WithLabelValues(eventSource(ev), "dropped").Inc()
		logger.Warn("off-chain event not written (infrastructure fault) — skipped, the next poll supersedes it",
			"kind", ev.EventKind(), "source", eventSource(ev), "err", err)
		return nil
	}
}

// runSinkLane runs one sink lane: workers drain goroutines over in, plus the external retry buffer when buffered.
// ep and tw are the two write seams (see persistWorker).
func runSinkLane(ctx context.Context, logger *slog.Logger, ep eventPersister, tw tradeWriter, in <-chan consumer.Event, mode SinkMode, workers int, buffered bool) ShutdownLoss {
	lt := &lossTracker{}
	// Bounded async retry buffer for external (CEX/FX) trades on infra faults (ADR-0041): they have no cursor to gate,
	// so they drop-oldest on overflow rather than block. A nil store skips the buffer.
	var extBuf *externalRetryBuffer
	var bufWG sync.WaitGroup
	var bufCancel context.CancelFunc
	if buffered {
		extBuf = newExternalRetryBuffer(tw, logger, externalRetryBufferMaxDepth)
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
