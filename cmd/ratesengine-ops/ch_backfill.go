package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/RatesEngine/rates-engine/internal/config"
	"github.com/RatesEngine/rates-engine/internal/ledgerstream"
	"github.com/RatesEngine/rates-engine/internal/storage/clickhouse"
)

// runCHBackfill walks a bounded ledger range from galexie and writes the
// Tier-1 structural rows to ClickHouse (ADR-0034 Phase 2). Mirrors the
// census-backfill ledgerstream walk; the per-ledger work is
// clickhouse.ExtractLedger -> Sink. Idempotent (ReplacingMergeTree), so a
// re-run over the same range is safe.
func chBackfill(args []string) error {
	fs := flag.NewFlagSet("ch-backfill", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to ratesengine.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required)")
	bucket := fs.String("bucket", "", "override storage bucket (default cfg.Storage.S3BucketLive)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	flushEvery := fs.Int("flush-every", 500, "flush to ClickHouse every N ledgers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	streamBucket := cfg.Storage.S3BucketLive
	if *bucket != "" {
		streamBucket = *bucket
	}
	lsCfg := newBoundedLedgerStreamConfig(cfg, streamBucket)
	passphrase := cfg.Stellar.Passphrase()

	sink, err := clickhouse.Open(ctx, *chAddr, *flushEvery)
	if err != nil {
		return err
	}
	defer func() { _ = sink.Close(ctx) }()

	fmt.Fprintf(os.Stderr, "ch-backfill: streaming ledgers %d..%d from %q -> ClickHouse %s\n",
		*from, *to, streamBucket, *chAddr)

	var total int
	start := time.Now()
	lastLog := time.Now()

	walkErr := ledgerstream.Stream(ctx, lsCfg, uint32(*from), uint32(*to),
		func(lcm sdkxdr.LedgerCloseMeta) error {
			ext, eerr := clickhouse.ExtractLedger(lcm, passphrase)
			if eerr != nil {
				fmt.Fprintf(os.Stderr, "ch-backfill: extract ledger %d: %v\n", lcm.LedgerSequence(), eerr)
				return nil
			}
			if aerr := sink.Add(ctx, ext); aerr != nil {
				return aerr // a ClickHouse write failure is fatal; retry the range
			}
			total++
			if time.Since(lastLog) >= 15*time.Second {
				rate := float64(total) / time.Since(start).Seconds()
				fmt.Fprintf(os.Stderr, "ch-backfill: %d ledgers (at %d, %.1f ledgers/s)\n",
					total, lcm.LedgerSequence(), rate)
				lastLog = time.Now()
			}
			return nil
		},
	)

	if ferr := sink.Flush(ctx); ferr != nil {
		return fmt.Errorf("ch-backfill: final flush: %w", ferr)
	}
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return fmt.Errorf("ch-backfill: stream (%d done): %w", total, walkErr)
	}
	elapsed := time.Since(start)
	rate := 0.0
	if elapsed.Seconds() > 0 {
		rate = float64(total) / elapsed.Seconds()
	}
	fmt.Fprintf(os.Stderr, "ch-backfill: done — %d ledgers in %s (%.1f ledgers/s)\n",
		total, elapsed.Round(time.Second), rate)
	return nil
}
