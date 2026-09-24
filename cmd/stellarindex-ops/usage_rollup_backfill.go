package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/redisclient"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// usageRollupDateLayout is the wire format of every date the usage
// package speaks (Redis key suffix, DetailRow.Date, RollupRow.Day).
const usageRollupDateLayout = "2006-01-02"

// usageRollupMaxRangeDays bounds -from/-to. The Redis detail hashes
// that feed the rollup carry a 35-day TTL (internal/usage's
// `retentionDays`, unexported), so a wider range can only scan days
// whose source data has already expired — while still paying a full
// keyspace SCAN per day. Refusing the range is better than a silent
// hour-long no-op walk.
const usageRollupMaxRangeDays = 35

// usageRollupBackfill re-folds the Redis per-endpoint usage counters
// into the `usage_daily` Timescale hypertable for an operator-chosen
// UTC date range.
//
// The in-process rollup worker ([usage.Rollup.Run], API binary)
// re-folds on its own every retained day an outage skipped, a bounded
// batch per sweep. This is the manual path for when that is not
// enough or not soon enough: no API process running a rollup, an
// operator who wants a range folded now, or a re-fold to check.
// It cannot reach past the Redis counters' 35-day TTL either.
//
// Usage:
//
//	stellarindex-ops usage-rollup-backfill \
//	  -config /etc/stellarindex.toml \
//	  -from 2026-07-19 -to 2026-07-21 -write
//
// Omitting -write runs a dry pass: it scans Redis and reports what it
// would upsert without touching usage_daily (see [opsutil.RegisterWriteGate]).
//
// Safe to re-run: [timescale.Store.UpsertUsageDaily] is a GREATEST()
// merge over the cumulative per-day counters, so a repeat pass is a
// no-op and can never regress a row a live sweep already wrote.
//
// Each day is folded by [usage.Rollup.SweepDays], the same fold the
// live worker's Sweep runs, so the rows this writes are byte-identical
// to the rows the worker would have written rather than a second
// implementation of the grouping that could drift.
func usageRollupBackfill(args []string) error {
	fs := flag.NewFlagSet("usage-rollup-backfill", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	fromStr := fs.String("from", "", "First UTC day to re-fold, YYYY-MM-DD (required)")
	toStr := fs.String("to", "", "Last UTC day to re-fold, YYYY-MM-DD (defaults to -from)")
	gate := opsutil.RegisterWriteGate(fs)
	timeout := fs.Duration("timeout", 15*time.Minute, "Overall deadline for the whole run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	days, err := usageRollupDays(*fromStr, *toStr)
	if err != nil {
		return err
	}
	gate.Banner()
	dryRun := gate.DryRun()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	rdb := redisclient.Build(cfg.Storage)
	if rdb == nil {
		return errors.New("redis is not configured (storage.redis_addr / redis_sentinel_addrs both empty) — " +
			"usage-rollup-backfill reads the per-endpoint counters from Redis")
	}
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer func() { _ = store.Close() }()

	return runUsageRollupBackfill(ctx, rdb, store, days, dryRun)
}

// usageRollupDays expands the -from/-to flags into the inclusive list
// of UTC days to fold, rejecting the shapes that would otherwise walk
// the whole Redis keyspace for nothing.
func usageRollupDays(fromStr, toStr string) ([]time.Time, error) {
	if fromStr == "" {
		return nil, errors.New("-from is required (YYYY-MM-DD, UTC)")
	}
	from, err := time.ParseInLocation(usageRollupDateLayout, fromStr, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("-from %q: want YYYY-MM-DD: %w", fromStr, err)
	}
	if toStr == "" {
		toStr = fromStr
	}
	to, err := time.ParseInLocation(usageRollupDateLayout, toStr, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("-to %q: want YYYY-MM-DD: %w", toStr, err)
	}
	if to.Before(from) {
		return nil, fmt.Errorf("-to %s is before -from %s", toStr, fromStr)
	}
	span := int(to.Sub(from)/(24*time.Hour)) + 1
	if span > usageRollupMaxRangeDays {
		return nil, fmt.Errorf(
			"-from %s -to %s spans %d days; the Redis counters that feed this rollup only live %d days, "+
				"so anything older has already expired — narrow the range (or split it) instead",
			fromStr, toStr, span, usageRollupMaxRangeDays)
	}
	days := make([]time.Time, 0, span)
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		days = append(days, d)
	}
	return days, nil
}

// runUsageRollupBackfill folds each day in turn and prints a per-day
// tally to stderr plus a summary line. Split from usageRollupBackfill
// so the flag/dependency wiring above stays readable.
func runUsageRollupBackfill(
	ctx context.Context,
	rdb redis.Cmdable,
	sink usage.RollupSink,
	days []time.Time,
	dryRun bool,
) error {
	logger := opsutil.MkBackfillLogger()
	if dryRun {
		// Swapped here, not by the caller, so the function that owns
		// -dry-run is the one that guarantees the real sink is never written.
		sink = &countingUsageSink{}
		fmt.Fprintln(os.Stderr, "DRY RUN — scanning Redis, no usage_daily writes")
	}
	fmt.Fprintf(os.Stderr, "Re-folding %d day(s): %s .. %s\n",
		len(days),
		days[0].Format(usageRollupDateLayout),
		days[len(days)-1].Format(usageRollupDateLayout),
	)

	rollup := usage.NewRollup(usage.New(rdb), sink, usage.DefaultRollupInterval,
		logger.With("component", "usage-rollup-backfill"))
	if rollup == nil {
		return errors.New("usage.NewRollup returned nil — counter or sink missing")
	}
	var total int
	for _, day := range days {
		n, err := rollup.SweepDays(ctx, []string{day.Format(usageRollupDateLayout)})
		if err != nil {
			return fmt.Errorf("re-fold %s: %w", day.Format(usageRollupDateLayout), err)
		}
		total += n
		fmt.Fprintf(os.Stderr, "  %s  %d row(s)\n", day.Format(usageRollupDateLayout), n)
	}

	verb := "upserted"
	if dryRun {
		verb = "would upsert"
	}
	fmt.Fprintf(os.Stderr, "usage-rollup-backfill: %s %d row(s) across %d day(s)\n", verb, total, len(days))
	return nil
}

// countingUsageSink is the -dry-run stand-in for the Timescale sink:
// it accepts (and discards) the batch the real sink would have merged
// so an operator can size the recovery before writing anything.
type countingUsageSink struct{ rows int }

func (s *countingUsageSink) UpsertUsageDaily(_ context.Context, rows []usage.RollupRow) error {
	s.rows += len(rows)
	return nil
}
