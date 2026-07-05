package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/config"
	"github.com/StellarIndex/stellar-index/internal/sources/external"
	externalbinance "github.com/StellarIndex/stellar-index/internal/sources/external/binance"
	externalbitstamp "github.com/StellarIndex/stellar-index/internal/sources/external/bitstamp"
	externalcoinbase "github.com/StellarIndex/stellar-index/internal/sources/external/coinbase"
	externalkraken "github.com/StellarIndex/stellar-index/internal/sources/external/kraken"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// backfillExternal drives the Backfiller interface for one external
// venue. Operator passes the venue-native symbol (the same shape
// each venue's Streamer would subscribe with): "XLMUSDT" for
// Binance, "XLM/USD" for Kraken, "xlmusd" for Bitstamp, "XLM-USD"
// for Coinbase. Keeps the CLI surface honest to venue conventions
// rather than inventing our own cross-venue normalisation.
//
//nolint:gocognit,gocyclo,funlen // ops-CLI subcommand: flag parsing + venue dispatch + insert-loop in one function is the most readable shape
func backfillExternal(args []string) error {
	fs := flag.NewFlagSet("backfill-external", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	source := fs.String("source", "", "Venue: binance | kraken | bitstamp | coinbase (required)")
	pairSym := fs.String("pair", "", "Venue-native symbol, e.g. XLMUSDT / XLM/USD / xlmusd / XLM-USD (required)")
	fromStr := fs.String("from", "", "Start time, RFC 3339 (required, e.g. 2024-01-01T00:00:00Z)")
	toStr := fs.String("to", "", "End time, RFC 3339 (required, e.g. 2024-12-31T00:00:00Z)")
	granStr := fs.String("granularity", "1h", "Candle granularity as a Go duration (1m / 15m / 1h / 4h / 1d / 1w)")
	rawTrades := fs.Bool("raw-trades", false, "kraken only: walk the /Trades fills endpoint instead of /OHLC — the deep-history path (OHLC serves only the most recent 720 candles; board #44). Slower (rate-limited pagination) but reaches the pair's full history with exact per-fill prices.")
	dryRun := fs.Bool("dry-run", false, "Fetch + synthesise trades but don't write to Timescale")
	progressEvery := fs.Int("progress-every", 1000, "Print a progress line every N trades inserted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *source == "" || *pairSym == "" || *fromStr == "" || *toStr == "" {
		fs.Usage()
		return fmt.Errorf("-config, -source, -pair, -from, -to all required")
	}

	from, err := time.Parse(time.RFC3339, *fromStr)
	if err != nil {
		return fmt.Errorf("parse -from %q: %w", *fromStr, err)
	}
	to, err := time.Parse(time.RFC3339, *toStr)
	if err != nil {
		return fmt.Errorf("parse -to %q: %w", *toStr, err)
	}
	if !from.Before(to) {
		return fmt.Errorf("-from %v must be before -to %v", from, to)
	}
	granularity, err := time.ParseDuration(*granStr)
	if err != nil {
		return fmt.Errorf("parse -granularity %q: %w", *granStr, err)
	}

	backfiller, pair, err := buildBackfiller(*source, *pairSym)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	fmt.Fprintf(os.Stderr, "backfill-external: source=%s pair=%s granularity=%v from=%s to=%s dry-run=%v\n",
		*source, pair.String(), granularity,
		from.Format(time.RFC3339), to.Format(time.RFC3339), *dryRun)

	t0 := time.Now()
	var trades []canonical.Trade
	if *rawTrades {
		kr, ok := backfiller.(*externalkraken.Streamer)
		if !ok {
			return fmt.Errorf("-raw-trades is kraken-only (venue %q has no fills-pagination path)", *source)
		}
		// Deep pagination is slow by design (venue rate limit) —
		// replace the 30-minute candle budget with a day.
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		trades, err = kr.BackfillTrades(ctx, pair, from, to)
	} else {
		trades, err = backfiller.Backfill(ctx, pair, from, to, granularity)
	}
	if err != nil {
		return fmt.Errorf("backfill: %w", err)
	}
	fmt.Fprintf(os.Stderr, "backfill-external: fetched %d trades in %v\n",
		len(trades), time.Since(t0).Round(time.Millisecond))

	if *dryRun {
		summariseDryRun(trades)
		return nil
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Mirror the indexer's USD-volume wiring (L2.2 phase 1) so an
	// ops-driven backfill of on-chain trades populates usd_volume
	// the same way live ingest does.
	if len(cfg.Trades.USDPeggedClassicAssets) > 0 {
		spec, err := timescale.NewUSDVolumeQuoteSpec(
			cfg.Trades.USDPeggedClassicAssets,
			cfg.Supply.SACWrappers,
		)
		if err != nil {
			return fmt.Errorf("usd-volume quote spec: %w", err)
		}
		store.SetUSDVolumeQuoteSpec(spec)
	}

	inserted, skipped := 0, 0
	for i, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			skipped++
			fmt.Fprintf(os.Stderr, "insert trade %d (%s): %v\n", i, tr.TxHash, err)
			continue
		}
		inserted++
		if *progressEvery > 0 && inserted%*progressEvery == 0 {
			fmt.Fprintf(os.Stderr, "  ... %d inserted, %d skipped\n", inserted, skipped)
		}
	}
	fmt.Fprintf(os.Stderr, "backfill-external: done — %d inserted, %d skipped in %v\n",
		inserted, skipped, time.Since(t0).Round(time.Millisecond))
	return nil
}

// buildBackfiller maps the -source flag to the venue's Backfiller
// implementation. Each venue's DefaultPairs is consulted to resolve
// the venue-native symbol into a canonical.Pair. Unknown sources or
// unconfigured pairs return a clear error rather than a generic
// "not in map".
func buildBackfiller(source, symbol string) (external.Backfiller, canonical.Pair, error) {
	switch source {
	case externalbinance.SourceName:
		pm, err := externalbinance.DefaultPairs()
		if err != nil {
			return nil, canonical.Pair{}, fmt.Errorf("binance pairs: %w", err)
		}
		pair, ok := pm[symbol]
		if !ok {
			return nil, canonical.Pair{}, unknownPairError(source, symbol, pm)
		}
		return externalbinance.NewStreamer(pm), pair, nil
	case externalkraken.SourceName:
		pm, err := externalkraken.DefaultPairs()
		if err != nil {
			return nil, canonical.Pair{}, fmt.Errorf("kraken pairs: %w", err)
		}
		pair, ok := pm[symbol]
		if !ok {
			return nil, canonical.Pair{}, unknownPairError(source, symbol, pm)
		}
		return externalkraken.NewStreamer(pm), pair, nil
	case externalbitstamp.SourceName:
		pm, err := externalbitstamp.DefaultPairs()
		if err != nil {
			return nil, canonical.Pair{}, fmt.Errorf("bitstamp pairs: %w", err)
		}
		pair, ok := pm[symbol]
		if !ok {
			return nil, canonical.Pair{}, unknownPairError(source, symbol, pm)
		}
		return externalbitstamp.NewStreamer(pm), pair, nil
	case externalcoinbase.SourceName:
		pm, err := externalcoinbase.DefaultPairs()
		if err != nil {
			return nil, canonical.Pair{}, fmt.Errorf("coinbase pairs: %w", err)
		}
		pair, ok := pm[symbol]
		if !ok {
			return nil, canonical.Pair{}, unknownPairError(source, symbol, pm)
		}
		return externalcoinbase.NewStreamer(pm), pair, nil
	}
	return nil, canonical.Pair{}, fmt.Errorf("unknown -source %q (supported: binance, kraken, bitstamp, coinbase)", source)
}

// unknownPairError prints the configured set so the operator can
// see the venue-specific symbol format without consulting docs.
func unknownPairError(source, want string, pm map[string]canonical.Pair) error {
	keys := make([]string, 0, len(pm))
	for k := range pm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Errorf("pair %q not in %s DefaultPairs — known symbols: %v", want, source, keys)
}

// summariseDryRun prints a compact stats view for --dry-run mode.
// Shows first/last trade timestamps, trade count, and pair-level
// volume totals so the operator can sanity-check a range before
// committing a large insert.
func summariseDryRun(trades []canonical.Trade) {
	if len(trades) == 0 {
		fmt.Println("(no trades in range)")
		return
	}
	totalBase, totalQuote := 0.0, 0.0
	for _, t := range trades {
		// Convert 10^8-scaled Amount to float for display. Precision
		// loss here is fine — it's a dry-run summary, not a computed
		// price.
		bf := amountToFloat(t.BaseAmount, 8)
		qf := amountToFloat(t.QuoteAmount, 8)
		totalBase += bf
		totalQuote += qf
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "FIELD\tVALUE")
	_, _ = fmt.Fprintf(w, "trade count\t%d\n", len(trades))
	_, _ = fmt.Fprintf(w, "first ts\t%s\n", trades[0].Timestamp.Format(time.RFC3339))
	_, _ = fmt.Fprintf(w, "last  ts\t%s\n", trades[len(trades)-1].Timestamp.Format(time.RFC3339))
	_, _ = fmt.Fprintf(w, "pair\t%s\n", trades[0].Pair.String())
	_, _ = fmt.Fprintf(w, "total base volume\t%.8f\n", totalBase)
	_, _ = fmt.Fprintf(w, "total quote volume\t%.8f\n", totalQuote)
	if totalBase > 0 {
		_, _ = fmt.Fprintf(w, "vwap (quote/base)\t%.8f\n", totalQuote/totalBase)
	}
	_ = w.Flush()
}

// amountToFloat converts a canonical.Amount at the given decimal
// scale to a float64 for display. Precision-lossy; never use this
// path for anything that writes back to storage.
func amountToFloat(a canonical.Amount, decimals int) float64 {
	bi := a.BigInt()
	if bi == nil {
		return 0
	}
	// Build "INT.FRAC" then parse via strconv.
	s := bi.String()
	neg := false
	if len(s) > 0 && s[0] == '-' {
		neg = true
		s = s[1:]
	}
	if len(s) <= decimals {
		s = strings.Repeat("0", decimals-len(s)+1) + s
	}
	cut := len(s) - decimals
	formatted := s[:cut] + "." + s[cut:]
	if neg {
		formatted = "-" + formatted
	}
	f, _ := strconv.ParseFloat(formatted, 64)
	return f
}

// ─── verify-decoders ─────────────────────────────────────────────
