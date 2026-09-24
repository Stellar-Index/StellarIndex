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
// A -write run never reads past the contiguous lake tip (the LiveSink
// drops whole ledgers under buffer pressure, so the lake can hold holes
// near the tip): the walk stops at the day holding that tip, which stays
// the newest day present and so is recomputed once ch-live-catchup heals
// the hole. A recompute smaller than the live partition is refused unless
// -shrink-ok.
//
// -backfill walks every day from the lake's first contract event
// (or -from-day) up to today. Heavy on the first run — serialize on
// r1 under run-heavy-job.sh.
//
// Fail-closed DRY RUN by default (opsutil.WriteGate): without -write
// this only logs which days WOULD be recomputed. The census-rollup
// and holders-rollup systemd units pass -write explicitly.
func chCensusRollup(args []string) error {
	fs := flag.NewFlagSet("ch-census-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	backfill := fs.Bool("backfill", false, "walk every day from the lake's first contract event (or -from-day) to today")
	fromDay := fs.String("from-day", "", "backfill floor as YYYY-MM-DD (default: first contract event's day; also the resume point)")
	shrinkOK := fs.Bool("shrink-ok", false, "allow replacing a day's partition with a recompute that has fewer contracts or events")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	write := gate.Banner()

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

	if !write {
		for day := from; !day.After(today); day = day.Add(24 * time.Hour) {
			logf("DRY RUN: would recompute census day %s (pass -write to apply; it stops at the contiguous lake tip)", day.Format("2006-01-02"))
		}
		return nil
	}
	tipDay, ok, err := clickhouse.ContiguousThroughDay(ctx, *chAddr, from)
	if err != nil {
		return fmt.Errorf("clickhouse: resolve contiguous lake tip: %w", err)
	}
	last := censusWalkEnd(from, today, tipDay, ok, logf)
	for day := from; !day.After(last); day = day.Add(24 * time.Hour) {
		if err := clickhouse.RunCensusDay(ctx, *chAddr, day, *shrinkOK, logf); err != nil {
			return fmt.Errorf("%w — resume with -from-day %s", err, day.Format("2006-01-02"))
		}
	}
	return nil
}

// censusWalkEnd is the last day the -write walk may recompute: today, or
// the day holding the contiguous lake tip (tipDay, ok from
// clickhouse.ContiguousThroughDay) when that is earlier. A day past a hole
// is not computed, so the resume point (the newest day present) stays at or
// before the holed day and it is recomputed after the heal, not skipped.
func censusWalkEnd(from, today, tipDay time.Time, ok bool, logf func(string, ...any)) time.Time {
	if !ok {
		logf("lake is not contiguous from the start of %s (a hole at the day boundary, or ingest has not reached it) — "+
			"recomputing nothing; re-run once ch-live-catchup heals it", from.Format("2006-01-02"))
		return from.Add(-24 * time.Hour)
	}
	if tipDay.Before(today) {
		logf("contiguous lake tip is on %s — recomputing through it only; later days wait until the hole heals",
			tipDay.Format("2006-01-02"))
		return tipDay
	}
	return today
}
