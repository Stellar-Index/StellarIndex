package chops

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// chCensusRollup maintains stellar.contracts_census_daily (the
// day-keyed per-contract event counts behind the /v1/contracts
// directory, deploy/clickhouse/contracts_census_daily.sql).
//
// Default (timer) mode recomputes the CURRENT UTC day plus any days
// missing since the newest one present — one whole-day compute +
// atomic REPLACE PARTITION each, so re-runs are idempotent and a
// 30-min cadence keeps the serving tail at most one cadence stale.
//
// -backfill walks every day from the lake's first contract event
// (or -from-day) up to today. Heavy on the first run — serialize on
// r1 under run-heavy-job.sh.
func chCensusRollup(args []string) error {
	fs := flag.NewFlagSet("ch-census-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	backfill := fs.Bool("backfill", false, "walk every day from the lake's first contract event (or -from-day) to today")
	fromDay := fs.String("from-day", "", "backfill floor as YYYY-MM-DD (default: first contract event's day; also the resume point)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "ch-census-rollup: "+format+"\n", a...)
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	var from time.Time
	switch {
	case *fromDay != "":
		var err error
		from, err = time.ParseInLocation("2006-01-02", *fromDay, time.UTC)
		if err != nil {
			return fmt.Errorf("-from-day: %w", err)
		}
	case *backfill:
		var err error
		from, err = clickhouse.EarliestEventDay(ctx, *chAddr)
		if err != nil {
			return err
		}
		logf("backfill floor = first contract event day %s", from.Format("2006-01-02"))
	default:
		// Incremental: resume from the newest day present (recompute it
		// — it may have been partial), or just today on an empty table
		// (the operator contract says run -backfill first).
		maxDay, hasRows, err := clickhouse.CensusMaxDay(ctx, *chAddr)
		if err != nil {
			return err
		}
		if hasRows {
			from = maxDay.UTC().Truncate(24 * time.Hour)
		} else {
			from = today
			logf("WARNING: census table is empty — serving readers stay on the fallback until -backfill has run")
		}
	}
	if from.After(today) {
		from = today
	}

	for day := from; !day.After(today); day = day.Add(24 * time.Hour) {
		if err := clickhouse.RunCensusDay(ctx, *chAddr, day, logf); err != nil {
			return fmt.Errorf("%w — resume with -from-day %s", err, day.Format("2006-01-02"))
		}
	}
	return nil
}
