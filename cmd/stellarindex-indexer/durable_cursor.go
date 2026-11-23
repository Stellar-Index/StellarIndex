package main

import (
	"context"
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
)

// cursorWriter is what [durableCursor] needs from [timescale.Store]; narrow so the checkpoint rules run without a database.
type cursorWriter interface {
	UpsertCursor(ctx context.Context, source, sub string, lastLedger uint32) error
	UpsertAccountObserverWatermark(ctx context.Context, processedLedger uint32) error
	UpsertClassicObserverWatermark(ctx context.Context, processedLedger uint32) error
}

// durableMarks is the part of [pipeline.LedgerMarks] a checkpoint reads.
type durableMarks interface {
	Processed() uint32
	DurableThrough() uint32
	AllLanded()
}

// durableCursor commits the ledgerstream cursor and the observer watermarks at the marks' durable ledger, never at the
// processed one: a crash or forced exit then resumes below every row the sink had not landed, and replay is idempotent
// (served-tier writes are ON CONFLICT). Not safe for concurrent use; the producer owns it until it has exited.
type durableCursor struct {
	store                                        cursorWriter
	marks                                        durableMarks
	logger                                       *slog.Logger
	accountObserverActive, classicObserverActive bool
	written                                      uint32 // last ledger committed by this process, or the resume cursor
}

// finalCommit runs after the sink lane returned. Workers stop acking at cancel, so the durable mark sits below the
// drained tail; only a closed channel (safeToClose) drained with nothing abandoned proves every processed ledger landed.
func (c *durableCursor) finalCommit(ctx context.Context, loss pipeline.ShutdownLoss, safeToClose bool) {
	if safeToClose && loss.Rows == 0 {
		c.marks.AllLanded()
		c.checkpoint(ctx)
	}
}

// checkpoint commits the durable ledger if it moved. Best-effort: a failed write lags the cursor, which the next ledger
// catches up, and the forward-only upserts make a repeat a no-op.
func (c *durableCursor) checkpoint(ctx context.Context) {
	through := c.marks.DurableThrough()
	obs.CursorDurableLagLedgers.Set(float64(c.marks.Processed() - through))
	if through <= c.written {
		return
	}
	if err := c.store.UpsertCursor(ctx, cursorSource, "", through); err != nil {
		// The gauge tracks the durable resume position, so it must not advance past a failed upsert.
		c.logger.Warn("cursor upsert", "ledger", through, "err", err)
	} else {
		c.written = through
		recordCursorMetric(through)
	}
	// The observer watermarks are the supply freshness anchors; they advance per ledger whether or not a watched
	// balance changed, but only for a live observer and only as far as its rows have landed.
	if c.accountObserverActive {
		if err := c.store.UpsertAccountObserverWatermark(ctx, through); err != nil {
			c.logger.Warn("account observer watermark upsert", "ledger", through, "err", err)
		}
	}
	if c.classicObserverActive {
		if err := c.store.UpsertClassicObserverWatermark(ctx, through); err != nil {
			c.logger.Warn("classic observer watermark upsert", "ledger", through, "err", err)
		}
	}
}
