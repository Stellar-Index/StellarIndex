package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Intra-ledger apply order — the live half of migration 0196's
// trades.tx_index. Same shape as RunSignerTagger: trades rows are written by
// the projector and the dispatcher, neither of which sees the tx's position
// in its ledger, so a first-wins trailing-window sweep back-tags it from the
// lake. Unlike signer it covers every on-chain source (ledger > 0).

const (
	txIndexSweepInterval = time.Minute
	txIndexSweepLookback = 30 * time.Minute
	// TxIndexPageSize is the distinct txs per page: one lake lookup and one
	// UPDATE. Shared with tag-tx-index so live and historical cannot drift.
	TxIndexPageSize = 1000
)

// TxIndexLakeReader resolves tx hashes to their intra-ledger apply order.
// *clickhouse.TxIndexReader satisfies it; hashes it does not know are absent.
type TxIndexLakeReader interface {
	TxIndexes(ctx context.Context, hashes []string) (map[string]uint32, error)
}

// TxIndexTradeTagger is the Postgres seam the tx-index walk reads and writes
// through. *timescale.Store satisfies it.
type TxIndexTradeTagger interface {
	UntaggedTxIndexTrades(ctx context.Context, from, to time.Time, afterLedger uint32, afterHash string, limit int) ([]timescale.TxIndexKey, error)
	TagTradesTxIndex(ctx context.Context, from, to time.Time, tags []timescale.TxIndexTag) (int64, error)
}

// TagTxIndexWindow tags trades.tx_index for every untagged on-chain tx in
// [from, to), page by page in (ledger, tx_hash) order, and returns the rows
// tagged. It walks to the end of the window rather than retrying the oldest
// page, so a hash the lake cannot resolve never starves the txs behind it.
func TagTxIndexWindow(ctx context.Context, lake TxIndexLakeReader, store TxIndexTradeTagger, from, to time.Time, pageSize int) (int64, error) {
	var (
		total       int64
		afterLedger uint32
		afterHash   string
	)
	for {
		keys, err := store.UntaggedTxIndexTrades(ctx, from, to, afterLedger, afterHash, pageSize)
		if err != nil {
			return total, err
		}
		if len(keys) == 0 {
			return total, nil
		}
		n, err := tagTxIndexPage(ctx, lake, store, keys)
		total += n
		if err != nil {
			return total, err
		}
		last := keys[len(keys)-1]
		afterLedger, afterHash = last.Ledger, last.TxHash
		if len(keys) < pageSize {
			return total, nil
		}
	}
}

func tagTxIndexPage(ctx context.Context, lake TxIndexLakeReader, store TxIndexTradeTagger, keys []timescale.TxIndexKey) (int64, error) {
	hashes := make([]string, len(keys))
	for i, k := range keys {
		hashes[i] = k.TxHash
	}
	order, err := lake.TxIndexes(ctx, hashes)
	if err != nil {
		return 0, fmt.Errorf("lake tx-index read (ledgers %d..%d): %w", keys[0].Ledger, keys[len(keys)-1].Ledger, err)
	}
	tags := make([]timescale.TxIndexTag, 0, len(keys))
	var tsFrom, tsTo time.Time
	for _, k := range keys {
		idx, ok := order[k.TxHash]
		if !ok {
			continue
		}
		if len(tags) == 0 || k.MinTs.Before(tsFrom) {
			tsFrom = k.MinTs
		}
		if len(tags) == 0 || k.MaxTs.After(tsTo) {
			tsTo = k.MaxTs
		}
		tags = append(tags, timescale.TxIndexTag{Ledger: k.Ledger, TxHash: k.TxHash, TxIndex: idx})
	}
	if len(tags) == 0 {
		return 0, nil
	}
	// +1s makes the inclusive max representable in the half-open ts bound.
	return store.TagTradesTxIndex(ctx, tsFrom, tsTo.Add(time.Second), tags)
}

// RunTxIndexTagger sweeps the trailing lookback window every interval,
// back-tagging trades.tx_index (first-wins, every on-chain source) from the
// lake. Blocks until ctx cancels; performs one final sweep on shutdown.
// interval/lookback <= 0 select the defaults.
func RunTxIndexTagger(ctx context.Context, logger *slog.Logger, lake TxIndexLakeReader, store TxIndexTradeTagger, interval, lookback time.Duration) {
	if interval <= 0 {
		interval = txIndexSweepInterval
	}
	if lookback <= 0 {
		lookback = txIndexSweepLookback
	}
	if logger == nil {
		logger = slog.Default()
	}

	sweep := func(sweepCtx context.Context) {
		now := time.Now().UTC()
		tagged, err := TagTxIndexWindow(sweepCtx, lake, store, now.Add(-lookback), now, TxIndexPageSize)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("tx-index sweep failed", "err", err, "tagged_before_failure", tagged)
			return
		}
		if tagged > 0 {
			logger.Info("tx-index sweep tagged trades", "tagged", tagged)
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	sweep(ctx)
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
