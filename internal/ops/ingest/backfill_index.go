// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/coingecko"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// BackfillIndex fills a historical price window from an INDEX source
// (currently CoinGecko) as oracle updates.
//
// This is the cascade's last resort, and it exists because the preferred
// venues cannot reach certain windows even in principle. Probed 2026-09-08
// for 2017-11-15: kraken, binance, coinbase and bitstamp each return zero
// trades, because Binance listed XLM in 2018 and Coinbase in 2019. Two
// windows are unreachable from venues for that reason — 2017-08-23..
// 2018-02-15 (177 days) and everything before Kraken's floor of 2017-01-17
// (475 days back to chain genesis).
//
// It writes ORACLE UPDATES, never trades. `trades` rows are venue fills
// carrying source + ledger + tx_hash + op_index, and ADR-0033's completeness
// claims are provable because that provenance exists per row; an index price
// is a volume-weighted composite across venues we do not observe and has
// none of it. Keeping the two apart is what stops a convenience backfill
// quietly degrading a claim the project currently earns.
// backfillIndexPlan is the validated shape of a backfill-index invocation:
// every flag resolved, parsed and checked, so the run loop below deals only
// with the walk itself. Splitting it out is what keeps either half readable.
type backfillIndexPlan struct {
	cfgPath       string
	source        string
	pair          canonical.Pair
	from, to      time.Time
	chunkDays     int
	sleep         time.Duration
	progressEvery int
	write         bool
}

// parseBackfillIndexArgs validates the invocation and fails on anything the
// walk would otherwise discover halfway through a long run.
func parseBackfillIndexArgs(args []string) (backfillIndexPlan, error) {
	var plan backfillIndexPlan
	fs := flag.NewFlagSet("backfill-index", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	source := fs.String("source", "coingecko", "index source: coingecko (coinmarketcap and cryptocompare have no historical adapter yet)")
	pairArg := fs.String("pair", "crypto:XLM/fiat:USD", "canonical pair, e.g. crypto:XLM/fiat:USD")
	fromArg := fs.String("from", "", "window start, RFC 3339 (required)")
	toArg := fs.String("to", "", "window end, RFC 3339, exclusive (required)")
	chunkDays := fs.Int("chunk-days", 80, "walk the range in chunks of this many days. CoinGecko picks its own granularity from the window width — under ~90 days it serves hourly, wider it serves daily — so a smaller chunk buys resolution at the cost of more requests")
	sleepMs := fs.Int("sleep-ms", 1500, "pause between chunk requests; the demo tier throttles aggressively")
	progressEvery := fs.Int("progress-every", 500, "print a progress line every N updates")
	write := fs.Bool("write", false, "actually insert; default is a dry run")
	if err := fs.Parse(args); err != nil {
		return plan, err
	}
	if *cfgPath == "" || *fromArg == "" || *toArg == "" {
		fs.Usage()
		return plan, errors.New("backfill-index: -config, -from and -to are required")
	}
	if *source != "coingecko" {
		return plan, fmt.Errorf("backfill-index: source %q has no historical adapter; only coingecko does today", *source)
	}
	from, err := time.Parse(time.RFC3339, *fromArg)
	if err != nil {
		return plan, fmt.Errorf("-from: %w", err)
	}
	to, err := time.Parse(time.RFC3339, *toArg)
	if err != nil {
		return plan, fmt.Errorf("-to: %w", err)
	}
	if !from.Before(to) {
		return plan, errors.New("backfill-index: -from must precede -to")
	}
	if *chunkDays < 1 {
		return plan, errors.New("backfill-index: -chunk-days must be at least 1")
	}
	pair, err := canonical.ParsePair(*pairArg)
	if err != nil {
		return plan, fmt.Errorf("-pair: %w", err)
	}
	plan = backfillIndexPlan{
		cfgPath: *cfgPath, source: *source, pair: pair, from: from, to: to,
		chunkDays: *chunkDays, sleep: time.Duration(*sleepMs) * time.Millisecond,
		progressEvery: *progressEvery, write: *write,
	}
	return plan, nil
}

func BackfillIndex(args []string) error {
	plan, err := parseBackfillIndexArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(plan.cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	poller := coingecko.NewPoller()
	// Keys come from the ENVIRONMENT, not the toml — same as the live
	// poller in cmd/stellarindex-indexer, so an operator sets one variable
	// and both paths pick it up. Pro wins when both are set, and only Pro
	// reaches past the demo tier's 365-day window.
	if k := strings.TrimSpace(os.Getenv("COINGECKO_API_KEY")); k != "" {
		poller.APIKey = k
	}
	if k := strings.TrimSpace(os.Getenv("COINGECKO_DEMO_API_KEY")); k != "" {
		poller.DemoAPIKey = k
	}
	authMode := "anonymous"
	if poller.APIKey != "" {
		authMode = "pro"
	} else if poller.DemoAPIKey != "" {
		authMode = "demo"
	}
	// No catalogue wiring here: the poller's built-in ticker map already
	// covers the assets a historical index backfill is for, and reaching
	// into the indexer's catalogue would couple an ops one-shot to the
	// live service's startup state.

	dryRun := !plan.write
	if dryRun {
		fmt.Fprintln(os.Stderr, "═══ DRY RUN — no writes; pass -write to apply ═══")
	} else {
		fmt.Fprintln(os.Stderr, "═══ WRITING — applying changes ═══")
	}
	fmt.Fprintf(os.Stderr, "backfill-index: source=%s pair=%s from=%s to=%s chunk=%dd dry-run=%v\n",
		plan.source, plan.pair.String(), plan.from.Format(time.RFC3339), plan.to.Format(time.RFC3339), plan.chunkDays, dryRun)
	fmt.Fprintf(os.Stderr, "backfill-index: auth=%s (only a pro key reaches past %d days)\n", authMode, coingecko.FreeTierHistoryDays)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()

	var store *timescale.Store
	if !dryRun {
		store, err = timescale.Open(ctx, cfg.Storage.PostgresDSN)
		if err != nil {
			return fmt.Errorf("storage: %w", err)
		}
		defer func() { _ = store.Close() }()
		// Same re-derive stamp the chainlink backfill uses (INV-3 /
		// migration 0109): a positive derive_generation lets a corrected
		// re-run UPDATE in place and win over a live gen-0 value.
		store.SetDeriveGeneration(time.Now().Unix())
	}

	t0 := time.Now()
	inserted, skipped, chunks, err := walkIndexRange(ctx, plan, poller, store, dryRun)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "backfill-index: done — %d observation(s) over %d chunk(s), %d skipped, in %v\n",
		inserted, chunks, skipped, time.Since(t0).Round(time.Second))
	if inserted == 0 {
		// A zero-row "success" is the shape that hides a broken backfill,
		// so it exits non-zero rather than reading as done.
		return fmt.Errorf("no observations returned for %s..%s — the window may predate the source's coverage, or the key's tier",
			plan.from.Format("2006-01-02"), plan.to.Format("2006-01-02"))
	}
	return nil
}

// walkIndexRange steps the window in chunk-sized pieces and writes what each
// returns. Split out of BackfillIndex so the command's setup (flags, config,
// auth, store) and its work loop can each be read on their own; together they
// were past the complexity budget, and the loop is the half that matters.
func walkIndexRange(
	ctx context.Context,
	plan backfillIndexPlan,
	poller *coingecko.Poller,
	store *timescale.Store,
	dryRun bool,
) (inserted, skipped, chunks int, err error) {
	for start := plan.from; start.Before(plan.to); start = start.AddDate(0, 0, plan.chunkDays) {
		end := start.AddDate(0, 0, plan.chunkDays)
		if end.After(plan.to) {
			end = plan.to
		}
		updates, err := poller.BackfillRange(ctx, plan.pair, start, end)
		if err != nil {
			// The tier refusal is not an outage and must not read like
			// one: it is the signal that this window needs a Pro key,
			// which is a purchasing decision rather than an incident.
			if errors.Is(err, coingecko.ErrOutsideFreeTier) {
				return inserted, skipped, chunks, fmt.Errorf("%s..%s is outside this key's history window — %w",
					start.Format("2006-01-02"), end.Format("2006-01-02"), err)
			}
			return inserted, skipped, chunks, fmt.Errorf("chunk %s..%s: %w",
				start.Format("2006-01-02"), end.Format("2006-01-02"), err)
		}
		chunks++
		added, failed := insertIndexChunk(ctx, store, updates, dryRun, plan.progressEvery, inserted, skipped)
		inserted += added
		skipped += failed
		if end.Before(plan.to) {
			select {
			case <-ctx.Done():
				return inserted, skipped, chunks, ctx.Err()
			case <-time.After(plan.sleep):
			}
		}
	}
	return inserted, skipped, chunks, nil
}

// insertIndexChunk writes one chunk's observations, counting rather than
// aborting on a per-row failure: a single bad observation must not discard a
// window that is otherwise good, and the caller's non-zero-exit-on-zero-rows
// check is what catches a run where everything failed.
//
// inserted/skipped are passed in only so the progress line reports a running
// total across chunks rather than restarting at each one.
func insertIndexChunk(
	ctx context.Context,
	store *timescale.Store,
	updates []canonical.OracleUpdate,
	dryRun bool,
	progressEvery, inserted, skipped int,
) (added, failed int) {
	for _, u := range updates {
		if dryRun {
			added++
			continue
		}
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "insert oracle_update (%s %s ts=%s): %v\n",
				u.Source, u.Price.String(), u.Timestamp.Format(time.RFC3339), err)
			continue
		}
		added++
		if progressEvery > 0 && (inserted+added)%progressEvery == 0 {
			fmt.Fprintf(os.Stderr, "  ... %d inserted, %d skipped\n", inserted+added, skipped+failed)
		}
	}
	return added, failed
}
