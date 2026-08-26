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
func RunSignerTagger(ctx context.Context, logger *slog.Logger, lake SignerLakeReader, store SignerRangeTagger, interval, lookback time.Duration) { //nolint:gocognit // linear trailing-window sweep: range → clamp → scoped lake read → first-wins tag, each step guarded

	if interval <= 0 {
		interval = signerSweepInterval
	}
	if lookback <= 0 {
		lookback = signerSweepLookback
	}
	if logger == nil {
		logger = slog.Default()
	}

	sweep := func(sweepCtx context.Context) {
		now := time.Now().UTC()
		minL, maxL, ok, err := store.UntaggedAMMSignerLedgerRange(sweepCtx, now.Add(-lookback), now)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			logger.Warn("signer sweep: range read failed", "err", err)
			return
		}
		if !ok {
			return // nothing untagged in the window — skip the lake read
		}
		// Clamp a wide (cold-start / lag) span to the oldest slice; the rest
		// is caught on the next tick as min-ledger advances.
		if maxL-minL+1 > signerSweepMaxLedgerSpan {
			maxL = minL + signerSweepMaxLedgerSpan - 1
		}
		sigs, err := lake.TxSignersForLedgerRange(sweepCtx, minL, maxL)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			logger.Warn("signer sweep: lake read failed", "err", err, "min_ledger", minL, "max_ledger", maxL)
			return
		}
		if len(sigs) == 0 {
			return
		}
		tags := make([]timescale.SignerTag, len(sigs))
		tsFrom, tsTo := sigs[0].CloseTime, sigs[0].CloseTime
		for i, s := range sigs {
			tags[i] = timescale.SignerTag{Ledger: s.Ledger, TxHash: s.TxHash, Signer: s.Signer}
			if s.CloseTime.Before(tsFrom) {
				tsFrom = s.CloseTime
			}
			if s.CloseTime.After(tsTo) {
				tsTo = s.CloseTime
			}
		}
		// Half-open [tsFrom, tsTo+1s) bounds the UPDATE to the tagged txs'
		// chunk span (+1s makes the inclusive max representable), so the
		// hypertable prunes instead of scanning/decompressing every chunk.
		tagged, err := store.TagTradesSigner(sweepCtx, tsFrom, tsTo.Add(time.Second), tags)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			logger.Warn("signer sweep: tag failed", "err", err)
			return
		}
		if tagged > 0 {
			logger.Info("signer sweep tagged trades", "tagged", tagged, "ledger_span", maxL-minL+1)
		}
	}

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
