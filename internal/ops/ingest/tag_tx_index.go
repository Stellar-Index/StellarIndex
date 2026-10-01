package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// tagTxIndex is the historical / recovery half of trades.tx_index
// (migration 0196): the live sweeper (pipeline.RunTxIndexTagger) covers only
// a trailing 30-min window, so rows older than that — a lake outage, a
// projector replay, the SDEX history backfill — are tagged here through the
// same pipeline.TagTxIndexWindow walk.
//
// Windowed by ts, the trades partition key, so each window's UPDATEs prune
// to its chunks and decompress only those (SQLSTATE 53400 means: lower
// -window). No checkpoint: the tag is first-wins, so a re-run re-reads an
// already-tagged window as an empty page and moves on.
//
// Fail-closed (opsutil.WriteGate): the default run reports how many
// transactions it WOULD tag and writes nothing; -write applies.
func tagTxIndex(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("tag-tx-index")
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	fromStr := fs.String("from", "", "Window start, RFC3339 (inclusive) — required")
	toStr := fs.String("to", "", "Window end, RFC3339 (exclusive) — required")
	window := fs.Duration("window", time.Hour, "trades ts span per walk; lower it if an UPDATE trips SQLSTATE 53400")
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
	var total int64
	for lo := from; lo.Before(to); lo = lo.Add(*window) {
		hi := lo.Add(*window)
		if hi.After(to) {
			hi = to
		}
		n, werr := tagTxIndexWindow(ctx, lake, store, write, lo, hi)
		total += n
		if werr != nil {
			return fmt.Errorf("window [%s, %s): %w", lo.Format(time.RFC3339), hi.Format(time.RFC3339), werr)
		}
		if n > 0 {
			fmt.Fprintf(os.Stderr, "tag-tx-index: window [%s, %s) %s: %d (total %d)\n",
				lo.Format(time.RFC3339), hi.Format(time.RFC3339), writeModeVerb(write, "tagged trades", "WOULD tag txs"), n, total)
		}
	}
	fmt.Fprintf(os.Stderr, "tag-tx-index: done. %s: %d\n", writeModeVerb(write, "tagged trades", "WOULD tag txs"), total)
	return nil
}

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

// tagTxIndexWindow walks one window. In the fail-closed preview the tag step
// only counts the resolved txs, so the trades table is never written; the
// untagged-row reads still run.
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
