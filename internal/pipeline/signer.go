package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// AMM actor attribution — the live half of migration 0150's trades.signer.
//
// Same shape + rationale as RunRoutedViaTagger: the AMM `trades` rows are
// written by the PROJECTOR (ADR-0032) from the lake events, which carry no
// tx source account, so the signer cannot be set on the decode path and an
// inline tag would race the projector. A trailing-window sweep is immune to
// that ordering, is first-wins/idempotent (timescale.TagTradesSigner), and
// self-heals projector lag inside the lookback.
//
// Where it differs from routed_via: the source (the tx signer) lives in the
// ClickHouse lake (stellar.transactions), not a pre-filtered Postgres table.
// A naive "read every recent tx" sweep would pull tens of thousands of rows
// per tick at pubnet volume, so this scopes the lake read to the SMALL ledger
// span of AMM trades that still need a signer (UntaggedAMMSignerLedgerRange);
// at steady state that span is a minute or two of ledgers, and it skips the
// lake entirely when nothing is untagged.

const (
	signerSweepInterval = time.Minute
	signerSweepLookback = 30 * time.Minute
	// signerSweepMaxLedgerSpan caps how many ledgers one sweep reads from the
	// lake + tags in a single UPDATE. At steady state the untagged span is a
	// minute or two of ledgers, well under this; but a cold start or a
	// multi-minute projector lag can widen it toward the full lookback, so
	// this clamps each tick to the OLDEST slice of the span — the rest is
	// picked up on the next tick(s) (first-wins, so the just-tagged head drops
	// out and min-ledger advances). Bounds the per-tick CH read + UPDATE.
	signerSweepMaxLedgerSpan = 120 // ~10 min of pubnet ledgers
)

// SignerLakeReader reads tx source accounts from the lake for a ledger range.
// *clickhouse.ExplorerReader satisfies it via TxSignersForLedgerRange.
type SignerLakeReader interface {
	TxSignersForLedgerRange(ctx context.Context, minLedger, maxLedger uint32) ([]clickhouse.TxSigner, error)
}

// SignerRangeTagger is the Postgres seam the sweeper reads + writes through.
// *timescale.Store satisfies it.
type SignerRangeTagger interface {
	UntaggedAMMSignerLedgerRange(ctx context.Context, from, to time.Time) (minLedger, maxLedger uint32, ok bool, err error)
	TagTradesSigner(ctx context.Context, from, to time.Time, tags []timescale.SignerTag) (int64, error)
}

// RunSignerTagger sweeps the trailing lookback window every interval,
// back-tagging trades.signer (first-wins, AMM sources) from the lake's
// stellar.transactions. Blocks until ctx cancels (run it in its own
// goroutine); performs one final sweep on shutdown so the last partial window
// isn't left to the next boot. interval/lookback <= 0 select the defaults.
func RunSignerTagger(ctx context.Context, logger *slog.Logger, lake SignerLakeReader, store SignerRangeTagger, interval, lookback time.Duration) {
	if interval <= 0 {
		interval = signerSweepInterval
	}
	if lookback <= 0 {
		lookback = signerSweepLookback
	}
	if logger == nil {
		logger = slog.Default()
	}

	sw := &signerSweeper{logger: logger, lake: lake, store: store, lookback: lookback}
	sweep := sw.sweep

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	sweep(ctx) // immediate first sweep so a restart resumes attribution promptly
	for {
		select {
		case <-ticker.C:
			sweep(ctx)
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second) //nolint:contextcheck // parent ctx is cancelled by definition here
			sweep(flushCtx)                                                               //nolint:contextcheck // see above
			cancel()
			return
		}
	}
}

// signerSweeper holds one tagger's sweep state.
type signerSweeper struct {
	logger   *slog.Logger
	lake     SignerLakeReader
	store    SignerRangeTagger
	lookback time.Duration
	// floor skips past a clamped slice that made no progress: a trade whose tx
	// is absent from the lake keeps the untagged minimum pinned, which would
	// otherwise re-read the same slice until the lookback expires.
	floor uint32
}

// warn logs a sweep failure unless it is just shutdown.
func (w *signerSweeper) warn(err error, msg string, args ...any) {
	if errors.Is(err, context.Canceled) {
		return
	}
	w.logger.Warn(msg, append([]any{"err", err}, args...)...)
}

func (w *signerSweeper) sweep(ctx context.Context) {
	now := time.Now().UTC()
	minL, maxL, ok, err := w.store.UntaggedAMMSignerLedgerRange(ctx, now.Add(-w.lookback), now)
	if err != nil {
		w.warn(err, "signer sweep: range read failed")
		return
	}
	if !ok {
		return // nothing untagged in the window — skip the lake read
	}
	if minL < w.floor {
		minL = w.floor
		if minL > maxL {
			return
		}
	}
	// Clamp a wide (cold-start / lag) span to the oldest slice; the rest
	// is caught on the next tick as min-ledger advances.
	clamped := maxL-minL+1 > signerSweepMaxLedgerSpan
	if clamped {
		maxL = minL + signerSweepMaxLedgerSpan - 1
	}
	sigs, err := w.lake.TxSignersForLedgerRange(ctx, minL, maxL)
	if err != nil {
		w.warn(err, "signer sweep: lake read failed", "min_ledger", minL, "max_ledger", maxL)
		return
	}
	if len(sigs) == 0 {
		if clamped {
			w.floor = maxL + 1
		}
		return
	}
	tags, tsFrom, tsTo := signerTags(sigs)
	// Half-open [tsFrom, tsTo+1s) bounds the UPDATE to the tagged txs'
	// chunk span (+1s makes the inclusive max representable), so the
	// hypertable prunes instead of scanning/decompressing every chunk.
	tagged, err := w.store.TagTradesSigner(ctx, tsFrom, tsTo.Add(time.Second), tags)
	if err != nil {
		w.warn(err, "signer sweep: tag failed")
		return
	}
	if tagged == 0 && clamped {
		w.floor = maxL + 1
	}
	if tagged > 0 {
		w.logger.Info("signer sweep tagged trades", "tagged", tagged, "ledger_span", maxL-minL+1)
	}
}

// signerTags converts lake rows to tags plus their inclusive close-time span.
func signerTags(sigs []clickhouse.TxSigner) (tags []timescale.SignerTag, from, to time.Time) {
	tags = make([]timescale.SignerTag, len(sigs))
	from, to = sigs[0].CloseTime, sigs[0].CloseTime
	for i, s := range sigs {
		tags[i] = timescale.SignerTag{Ledger: s.Ledger, TxHash: s.TxHash, Signer: s.Signer}
		if s.CloseTime.Before(from) {
			from = s.CloseTime
		}
		if s.CloseTime.After(to) {
			to = s.CloseTime
		}
	}
	return tags, from, to
}
