package chops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// tagTxIndex is the historical half of trades.tx_index (the live sweeper covers a trailing 30-min window), via
// pipeline.TagTxIndexWindow windowed by ts (the partition key). No checkpoint: tagging is first-wins. -write refuses a range touching a
// compressed trades chunk (the UPDATE decompresses it in-transaction and bloats WAL). Fail-closed via opsutil.WriteGate.
func tagTxIndex(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("tag-tx-index")
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	fromStr := fs.String("from", "", "Window start, RFC3339 (inclusive) — required")
	toStr := fs.String("to", "", "Window end, RFC3339 (exclusive) — required")
	window := fs.Duration("window", time.Hour, "trades ts span per walk")
	chAddr := fs.String("ch-addr", "", "ClickHouse native address (default: config clickhouse_addr)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	from, to, err := parseTxIndexRange(*fromStr, *toStr)
	if err != nil {
		return err
	}
	if *window <= 0 {
		return errors.New("-window must be > 0")
	}
	write := gate.Banner()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	addr := *chAddr
	if addr == "" {
		addr = cfg.Storage.ClickHouseAddr
	}
	if addr == "" {
		addr = "127.0.0.1:9300"
	}
	lake, err := clickhouse.NewTxIndexReader(ctx, addr)
	if err != nil {
		return fmt.Errorf("clickhouse open %s: %w", addr, err)
	}
	defer func() { _ = lake.Close() }()

	fmt.Fprintf(os.Stderr, "tag-tx-index: [%s, %s), window %s\n", from.Format(time.RFC3339), to.Format(time.RFC3339), *window)
	total, err := runTagTxIndex(ctx, lake, store, write, from, to, *window)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "tag-tx-index: done. %s: %d\n", opsutil.WriteModeVerb(write, "tagged trades", "WOULD tag txs"), total)
	return nil
}

// txIndexTagStore is the tagger seam plus the chunk listing the -write
// guard reads. *timescale.Store satisfies it.
type txIndexTagStore interface {
	pipeline.TxIndexTradeTagger
	TradesChunksInRange(ctx context.Context, from, to time.Time) ([]timescale.TradeChunk, error)
}

// runTagTxIndex walks [from, to) window by window. Under -write the chunk guard runs over the whole range first (a refused run writes
// nothing) and again before each window, because the compression policy can compress a chunk mid-run.
func runTagTxIndex(ctx context.Context, lake pipeline.TxIndexLakeReader, store txIndexTagStore,
	write bool, from, to time.Time, window time.Duration,
) (int64, error) {
	if err := refuseCompressedTradesChunks(ctx, store, write, from, to); err != nil {
		return 0, err
	}
	var total int64
	for lo := from; lo.Before(to); lo = lo.Add(window) {
		hi := lo.Add(window)
		if hi.After(to) {
			hi = to
		}
		if write {
			if err := refuseCompressedTradesChunks(ctx, store, write, lo, hi); err != nil {
				return total, err
			}
		}
		n, werr := tagTxIndexWindow(ctx, lake, store, write, lo, hi)
		total += n
		if werr != nil {
			return total, fmt.Errorf("window [%s, %s): %w", lo.Format(time.RFC3339), hi.Format(time.RFC3339), werr)
		}
		if n > 0 {
			fmt.Fprintf(os.Stderr, "tag-tx-index: window [%s, %s) %s: %d (total %d)\n",
				lo.Format(time.RFC3339), hi.Format(time.RFC3339), opsutil.WriteModeVerb(write, "tagged trades", "WOULD tag txs"), n, total)
		}
	}
	return total, nil
}

// refuseCompressedTradesChunks lists trades chunks intersecting [from, to) and, under -write, refuses if any is compressed (also true
// when partially); the preview only warns.
func refuseCompressedTradesChunks(ctx context.Context, store txIndexTagStore, write bool, from, to time.Time) error {
	chunks, err := store.TradesChunksInRange(ctx, from, to)
	if err != nil {
		return err
	}
	var compressed []string
	for _, c := range chunks {
		if c.Compressed {
			compressed = append(compressed, fmt.Sprintf("%s [%s, %s)", c, c.RangeStart.Format(time.RFC3339), c.RangeEnd.Format(time.RFC3339)))
		}
	}
	if len(compressed) == 0 {
		return nil
	}
	msg := fmt.Sprintf("[%s, %s) touches %d compressed trades chunk(s): %s",
		from.Format(time.RFC3339), to.Format(time.RFC3339), len(compressed), strings.Join(compressed, ", "))
	if !write {
		fmt.Fprintf(os.Stderr, "tag-tx-index: WARNING: -write would refuse: %s\n", msg)
		return nil
	}
	return fmt.Errorf("%w: %s; narrow -from/-to to uncompressed chunks", errTxIndexCompressedChunk, msg)
}

// errTxIndexCompressedChunk is the -write refusal for a range that touches
// a compressed trades chunk.
var errTxIndexCompressedChunk = errors.New("tag-tx-index: refusing to UPDATE compressed trades chunks")

func parseTxIndexRange(fromStr, toStr string) (from, to time.Time, err error) {
	if fromStr == "" || toStr == "" {
		return from, to, errors.New("-from and -to are required (RFC3339)")
	}
	if from, err = time.Parse(time.RFC3339, fromStr); err != nil {
		return from, to, fmt.Errorf("-from: %w", err)
	}
	if to, err = time.Parse(time.RFC3339, toStr); err != nil {
		return from, to, fmt.Errorf("-to: %w", err)
	}
	if !to.After(from) {
		return from, to, fmt.Errorf("-to (%s) must be after -from (%s)", toStr, fromStr)
	}
	return from.UTC(), to.UTC(), nil
}

// tagTxIndexWindow walks one window. In the preview the tag step only counts resolved txs, so trades is never written.
func tagTxIndexWindow(ctx context.Context, lake pipeline.TxIndexLakeReader, store pipeline.TxIndexTradeTagger,
	write bool, from, to time.Time,
) (int64, error) {
	if !write {
		store = txIndexPreview{store}
	}
	return pipeline.TagTxIndexWindow(ctx, lake, store, from, to, pipeline.TxIndexPageSize)
}

// txIndexPreview reads through to the store and reports, instead of
// applying, each page's tags.
type txIndexPreview struct {
	pipeline.TxIndexTradeTagger
}

func (txIndexPreview) TagTradesTxIndex(_ context.Context, _, _ time.Time, tags []timescale.TxIndexTag) (int64, error) {
	return int64(len(tags)), nil
}
