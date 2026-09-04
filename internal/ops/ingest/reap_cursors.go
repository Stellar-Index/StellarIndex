package ingest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// reap-cursors — delete the ingestion_cursors rows left behind by
// finished or abandoned one-shot jobs.
//
// Why this exists. ingestion_cursors has one permanent row per
// (source, sub_source) and no retention policy, and every sharded ops
// job mints one row per shard. Nothing removes them when the job ends,
// successfully or otherwise: on r1 (2026-09-03) the table held 4,815
// rows, of which 4,703 had not been written to in over a week — 4,523
// projected-rebuild shards, and 91 SDEX backfill shards from an attempt
// abandoned on 2026-05-06 and 2026-05-14 whose lag had reached ~9.7M
// seconds. `list-cursors`, the /diagnostics page and the public
// `/v1/diagnostics/cursors` endpoint all list that table, so the dead
// rows were the bulk of what every consumer saw. The API side now
// defaults to the non-abandoned set; this is the other half — the way
// to actually remove the records once an operator has decided the work
// they describe is over.
//
// Posture:
//
//   - Preview by default. Writes only under -write, via the shared
//     [opsutil.WriteGate], and the preview output IS the review step —
//     it prints the per-source counts, the oldest row, and a sample of
//     the exact rows a -write run would delete.
//   - -older-than has a hard floor of [reapMinAge]. The failure mode
//     worth designing against is a mistyped small threshold sweeping
//     live positions; a day is far past any legitimate checkpoint
//     interval, so nothing is lost by refusing below it.
//   - The live namespaces in [timescale.LiveCursorSources] are never
//     reaped, with or without -write. A stuck live cursor is an
//     incident, and deleting it turns "the indexer is behind" into
//     "the indexer restarts from its configured start ledger" — a
//     recovery loss rather than a cleanup. The same list keeps those
//     rows out of the API's `abandoned` state, so the two halves of
//     this posture cannot drift apart.
//
// Deleting a row deletes a RECORD, never data: the ledgers a shard
// walked stay in the lake and the served tier. What is lost is the
// resume point, so reap only shards whose remaining range is either
// finished or being abandoned deliberately — `resume-stalled -dry-run`
// (or `/v1/diagnostics/cursors?status=abandoned`) is the check to run
// first.
func reapCursors(args []string) error {
	opts, err := parseReapCursorsFlags(args)
	if err != nil {
		return err
	}
	// Announce the mode before any slow or mutating work, the same way
	// every other write-gated subcommand does. The gate decision is
	// carried in opts, so this is the plain-bool form of
	// [opsutil.WriteGate.Banner].
	opsutil.PrintWriteBanner(opts.write)

	cfg, err := config.LoadWithEnv(opts.cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	rows, err := store.ListCursors(ctx)
	if err != nil {
		return err
	}

	cutoff := time.Now().UTC().Add(-opts.olderThan)
	plan := planReapCursors(rows, cutoff, opts.source)
	printReapPlan(os.Stdout, plan, cutoff, opts)

	if len(plan.candidates) == 0 || !opts.write {
		return nil
	}

	deleted, err := store.ReapCursors(ctx, cutoff, opts.source, timescale.LiveCursorSources())
	if err != nil {
		return err
	}
	fmt.Printf("\n✅ reap-cursors: %d row(s) deleted (%d previewed).\n", deleted, len(plan.candidates))
	return nil
}

// reapMinAge is the floor on -older-than. Every writer in this system
// checkpoints far more often than daily (the live indexer every ~5s,
// the projector once per cycle, a walking backfill shard continuously),
// so a threshold below a day cannot separate "finished" from "running"
// — it can only sweep rows that are merely between writes.
const reapMinAge = 24 * time.Hour

// reapDefaultAge matches the API's abandoned boundary
// (v1.cursorAbandonedAge), so `?status=abandoned` and a default
// `reap-cursors` preview describe the same set of rows.
const reapDefaultAge = 7 * 24 * time.Hour

type reapCursorsOpts struct {
	cfgPath   string
	olderThan time.Duration
	source    string
	write     bool
}

// reapPlan is what a -write run would delete, derived from the same
// listing the preview prints.
type reapPlan struct {
	candidates []timescale.Cursor
	// protected counts rows old enough to reap that were spared
	// because their source is a live cursor namespace — surfaced so a
	// stuck live cursor is reported rather than silently skipped.
	protected []timescale.Cursor
	bySource  map[string]int
}

func parseReapCursorsFlags(args []string) (reapCursorsOpts, error) {
	var opts reapCursorsOpts

	fs := flag.NewFlagSet("reap-cursors", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	olderThan := fs.Duration("older-than", reapDefaultAge,
		"delete cursors whose last_updated is older than this Go duration "+
			"(minimum 24h; default 168h = the API's abandoned boundary)")
	source := fs.String("source", "",
		"narrow to one exact `source` value (e.g. projected-rebuild, backfill); "+
			"empty = every reapable source")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return opts, err
	}

	if *cfgPath == "" {
		return opts, errors.New("-config is required")
	}
	if *olderThan < reapMinAge {
		return opts, fmt.Errorf("-older-than %s is below the %s floor — nothing in this system checkpoints that slowly, so a smaller threshold can only reap rows that are still being written",
			*olderThan, reapMinAge)
	}
	if isReapProtected(*source) {
		return opts, fmt.Errorf("-source %q is a live cursor namespace and is never reaped: an old row there means ingest is stuck, which is an incident to investigate rather than a row to delete", *source)
	}

	opts = reapCursorsOpts{
		cfgPath:   *cfgPath,
		olderThan: *olderThan,
		source:    *source,
		write:     gate.Enabled(),
	}
	return opts, nil
}

// planReapCursors splits a cursor listing into the rows a reap would
// delete and the old-enough rows it refuses to touch. Pure so the
// selection rules are testable without a database, and so the preview
// and the DELETE are driven by one definition of "reapable".
func planReapCursors(rows []timescale.Cursor, cutoff time.Time, source string) reapPlan {
	plan := reapPlan{bySource: map[string]int{}}
	for _, c := range rows {
		if !c.UpdatedAt.Before(cutoff) {
			continue
		}
		if source != "" && c.Source != source {
			continue
		}
		if isReapProtected(c.Source) {
			plan.protected = append(plan.protected, c)
			continue
		}
		plan.candidates = append(plan.candidates, c)
		plan.bySource[c.Source]++
	}
	sort.Slice(plan.candidates, func(i, j int) bool {
		if plan.candidates[i].Source != plan.candidates[j].Source {
			return plan.candidates[i].Source < plan.candidates[j].Source
		}
		return plan.candidates[i].UpdatedAt.Before(plan.candidates[j].UpdatedAt)
	})
	return plan
}

func isReapProtected(source string) bool {
	return timescale.IsLiveCursorSource(source)
}

// reapSampleRows caps the per-run row sample in the preview. The
// counts are exact; the sample is there so an operator can recognise
// what the rows ARE before deleting thousands of them.
const reapSampleRows = 10

func printReapPlan(w io.Writer, plan reapPlan, cutoff time.Time, opts reapCursorsOpts) {
	_, _ = fmt.Fprintf(w, "reap-cursors: last_updated < %s (older than %s)%s\n",
		cutoff.Format(time.RFC3339), opts.olderThan, sourceScope(opts.source))

	// A protected row past the cutoff is the one finding here an
	// operator must not miss: live ingest has not written in that long.
	// Printed BEFORE the empty-plan return, because a table with
	// nothing else old enough to reap is exactly the case where a stuck
	// live cursor is the only thing the run has to report.
	for _, c := range plan.protected {
		_, _ = fmt.Fprintf(w, "\n⚠️  %s/%s is older than the cutoff (last write %s) and is NOT reaped — a live cursor that stale means ingest is stuck.\n",
			c.Source, c.Sub, c.UpdatedAt.UTC().Format(time.RFC3339))
	}

	if len(plan.candidates) == 0 {
		if len(plan.protected) > 0 {
			_, _ = fmt.Fprintln(w, "\n(nothing to reap — every cursor past the cutoff is in a protected live namespace)")
		} else {
			_, _ = fmt.Fprintln(w, "\n(nothing to reap — no cursor is that old)")
		}
		return
	}

	sources := make([]string, 0, len(plan.bySource))
	for s := range plan.bySource {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nSOURCE\tROWS")
	for _, s := range sources {
		_, _ = fmt.Fprintf(tw, "%s\t%d\n", s, plan.bySource[s])
	}
	_, _ = fmt.Fprintf(tw, "TOTAL\t%d\n", len(plan.candidates))
	_ = tw.Flush()

	sample := make([]timescale.Cursor, len(plan.candidates))
	copy(sample, plan.candidates)
	sort.SliceStable(sample, func(i, j int) bool { return sample[i].UpdatedAt.Before(sample[j].UpdatedAt) })
	if len(sample) > reapSampleRows {
		sample = sample[:reapSampleRows]
	}
	_, _ = fmt.Fprintf(w, "\nSample (oldest %d of %d):\n", len(sample), len(plan.candidates))
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SOURCE\tSUB\tLAST LEDGER\tUPDATED")
	for _, c := range sample {
		sub := c.Sub
		if sub == "" {
			sub = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", c.Source, sub, c.LastLedger, c.UpdatedAt.UTC().Format(time.RFC3339))
	}
	_ = tw.Flush()

	if !opts.write {
		_, _ = fmt.Fprintln(w, "\n(dry-run — re-run with -write to delete these rows)")
	}
}

func sourceScope(source string) string {
	if source == "" {
		return ""
	}
	return ", source=" + source
}
