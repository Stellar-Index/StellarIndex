package usage

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

// RollupBackfill re-folds Redis per-endpoint usage counters into usage_daily for a UTC date range (-from/-to, -write; dry run otherwise), bounded by the 35-day TTL; re-runs are safe (GREATEST() merge).
func RollupBackfill(args []string) error {
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
	days, err := usageRollupDays(*fromStr, *toStr, time.Now())
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

// usageRollupDays expands -from/-to into inclusive UTC days. The range must sit inside the Redis retention window
// ending today: an expired day reads nothing and prints "0 row(s)", indistinguishable from no traffic, and a future day has nothing yet; both are refused.
func usageRollupDays(fromStr, toStr string, now time.Time) ([]time.Time, error) {
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
	today := now.UTC().Truncate(24 * time.Hour)
	if to.After(today) {
		return nil, fmt.Errorf("-to %s is after today (%s UTC); there are no counters to fold yet",
			toStr, today.Format(usageRollupDateLayout))
	}
	oldest := today.AddDate(0, 0, -(usage.RetentionDays - 1))
	if from.Before(oldest) {
		return nil, fmt.Errorf(
			"-from %s is before %s: the Redis counters that feed this rollup only live %d days, "+
				"so those days have already expired and would fold as 0 rows — start at %s or later",
			fromStr, oldest.Format(usageRollupDateLayout), usage.RetentionDays, oldest.Format(usageRollupDateLayout))
	}
	days := make([]time.Time, 0, int(to.Sub(from)/(24*time.Hour))+1)
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		days = append(days, d)
	}
	return days, nil
}

// runUsageRollupBackfill folds each day in turn and prints a per-day
// tally to stderr plus a summary line. Split from RollupBackfill
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
