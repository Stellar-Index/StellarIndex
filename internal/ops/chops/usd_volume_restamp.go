// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// usdVolumeRestamp is the stellarindex-ops `usd-volume-restamp` subcommand
// — the corrective WRITE half of verify-usd-volume (W5.3, v1-launch-plan;
// docs/operations/usd-volume-rederive-2026-08.md §5).
//
// verify-usd-volume judges the EXACT tiers of the usd_volume waterfall
// (quote leg or base leg USD-pegged → usd_volume == pegged_leg /
// 10^decimals) and reports every day-group that fails. This tool repairs
// them: for every exact-tier (source, base, quote) group in the window it
// rewrites each row whose stored value differs from the identity to
// exactly the value the insert path would write today, stamped with this
// run's derive_generation (INV-3).
//
// It is NOT a general re-derive. Estimated-tier rows (FX rate / XLM anchor
// at trade time) need the resolver-backed waterfall and belong to
// `ch-rebuild`; this tool never reads them. What it fixes is the
// pre-2026-07-23 class measured on 2026-07-30: [2026-05-12, 2026-07-22],
// 66 dirty days, every violation a `[base_pegged] sdex` USDC-base row
// valued by the resolver's VWAP (+0.7%) instead of the $1 peg identity —
// repaired that night by hand SQL, which this tool replaces.
//
// Discipline:
//   - fail-closed DRY RUN by default; -write applies (opsutil.WriteGate);
//   - bounded window: -from/-to UTC days (inclusive), walked oldest →
//     newest one -slice at a time so no single UPDATE spans more than one
//     slice of a compressed chunk;
//   - idempotent: a row that already satisfies the identity is not
//     touched (value or generation), so a re-run reports 0;
//   - node_exporter heartbeat like ch-backfill (-heartbeat), so a wedged
//     run under run-heavy-job.sh trips the ops_job stall alerts;
//   - the tier + scale per group come from timescale.ClassifyUSDVolumeTier
//     with the SAME peg inputs as the insert path — never decided here.
//
// Acceptance after a run: `verify-usd-volume -day <last> -days <N>` over
// the span at 0 violations.
//
// Usage: usd-volume-restamp -config PATH -from YYYY-MM-DD -to YYYY-MM-DD
// [-slice DUR] [-sources a,b] [-fill-null] [-heartbeat PATH] [-write].
func usdVolumeRestamp(args []string) error {
	fs := flag.NewFlagSet("usd-volume-restamp", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml (Postgres DSN + the operator's USD peg list)")
	fromFlag := fs.String("from", "", "first UTC day of the window, YYYY-MM-DD (required)")
	toFlag := fs.String("to", "", "last UTC day of the window, YYYY-MM-DD, inclusive (required; must not be today — that chunk is still being written)")
	slice := fs.Duration("slice", time.Hour, "time span of one UPDATE statement; bounds the per-transaction decompression + lock footprint")
	sources := fs.String("sources", "", "comma-separated source allow-list (default: every exact-tier group found)")
	fillNull := fs.Bool("fill-null", false, "also stamp exact-tier rows whose usd_volume is NULL (a COVERAGE change; off by default)")
	heartbeat := fs.String("heartbeat", "", "node_exporter textfile path for the liveness/progress gauges. Empty = "+opsutil.DefaultTextfileDir+"/ops_job_usd_volume_restamp.prom when that directory exists (r1), otherwise no heartbeat at all")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	from, to, err := resolveRestampWindow(*fromFlag, *toFlag, time.Now())
	if err != nil {
		return err
	}
	if *slice <= 0 || *slice > 24*time.Hour {
		return fmt.Errorf("usd-volume-restamp: -slice must be in (0, 24h], got %s", *slice)
	}

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

	// The SAME peg inputs the insert path and verify-usd-volume use.
	spec, err := timescale.NewUSDVolumeQuoteSpec(cfg.Trades.USDPeggedClassicAssets, cfg.Supply.SACWrappers)
	if err != nil {
		return fmt.Errorf("usd-volume-restamp: usd-volume quote spec: %w", err)
	}

	write := gate.Banner()
	run := &restampRun{
		store:    store,
		spec:     spec,
		allow:    restampSourceAllowList(*sources),
		fillNull: *fillNull,
		slice:    *slice,
		write:    write,
		// INV-3: one generation for the whole run, like ch-rebuild.
		generation: time.Now().Unix(),
		hb:         opsutil.NewJobHeartbeat("usd-volume-restamp", *heartbeat, nil),
	}
	if run.hb.Enabled() {
		fmt.Fprintf(os.Stderr, "usd-volume-restamp: heartbeat -> %s\n", run.hb.Path())
	}
	run.hb.Start()
	ok := false
	defer func() { run.hb.Stop(ok) }()

	fmt.Fprintf(os.Stderr, "usd-volume-restamp: window [%s, %s] slice=%s generation=%d fill_null=%v\n",
		from.Format(time.DateOnly), to.Format(time.DateOnly), *slice, run.generation, *fillNull)

	var totalRows, totalGroups int64
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		rows, groups, derr := run.day(ctx, day)
		if derr != nil {
			return derr
		}
		totalRows += rows
		totalGroups += groups
	}
	ok = true
	verb := "would restamp"
	if write {
		verb = "restamped"
	}
	fmt.Printf("\nusd-volume-restamp: %s %d row(s) across %d exact-tier group-day(s) in [%s, %s]\n",
		verb, totalRows, totalGroups, from.Format(time.DateOnly), to.Format(time.DateOnly))
	fmt.Printf("acceptance: stellarindex-ops verify-usd-volume -config %s -day %s -days %d  → expect 0 violations\n",
		*cfgPath, to.Format(time.DateOnly), int(to.Sub(from).Hours()/24)+1)
	return nil
}

// resolveRestampWindow parses the inclusive -from/-to day pair. Refuses an
// empty pair (no "whole history" default on a money-column writer), a
// reversed pair, and a window reaching TODAY: today's chunk is still being
// written and the live writers are at generation 0, so restamping it would
// race the ingest path over rows it has not finished valuing.
func resolveRestampWindow(fromFlag, toFlag string, now time.Time) (from, to time.Time, err error) {
	if fromFlag == "" || toFlag == "" {
		return from, to, fmt.Errorf("usd-volume-restamp: -from and -to are required (YYYY-MM-DD, inclusive UTC days)")
	}
	if from, err = time.Parse(time.DateOnly, fromFlag); err != nil {
		return from, to, fmt.Errorf("usd-volume-restamp: -from %q: want YYYY-MM-DD: %w", fromFlag, err)
	}
	if to, err = time.Parse(time.DateOnly, toFlag); err != nil {
		return from, to, fmt.Errorf("usd-volume-restamp: -to %q: want YYYY-MM-DD: %w", toFlag, err)
	}
	from, to = from.UTC(), to.UTC()
	if to.Before(from) {
		return from, to, fmt.Errorf("usd-volume-restamp: -to %s is before -from %s", toFlag, fromFlag)
	}
	today := now.UTC().Truncate(24 * time.Hour)
	if !to.Before(today) {
		return from, to, fmt.Errorf("usd-volume-restamp: -to %s reaches today (%s) — today's chunk is still being written; restamp only closed days",
			toFlag, today.Format(time.DateOnly))
	}
	return from, to, nil
}

// restampSourceAllowList turns -sources into a set; nil means "all".
func restampSourceAllowList(csv string) map[string]bool {
	list := opsutil.SplitCSV(csv)
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}

// restampRun carries one invocation's fixed inputs across days.
type restampRun struct {
	store      *timescale.Store
	spec       *timescale.USDVolumeQuoteSpec
	allow      map[string]bool
	fillNull   bool
	slice      time.Duration
	write      bool
	generation int64
	hb         *opsutil.JobHeartbeat
	progress   uint64
}

// day restamps one UTC day: classify its groups, keep the exact tiers,
// then walk the day in -slice windows. Returns (rows, groups).
func (r *restampRun) day(ctx context.Context, day time.Time) (int64, int64, error) {
	groups, err := r.store.TradeValuationByDay(ctx, day)
	if err != nil {
		return 0, 0, err
	}
	targets, before := r.exactTierGroups(groups)
	fmt.Printf("=== %s: %d group(s), %d exact-tier target(s), Σ|Δ| before = %s USD\n",
		day.Format(time.DateOnly), len(groups), len(targets), before.FloatString(8))
	if len(targets) == 0 {
		return 0, 0, nil
	}

	var rows int64
	end := day.AddDate(0, 0, 1)
	for lo := day; lo.Before(end); lo = lo.Add(r.slice) {
		hi := lo.Add(r.slice)
		if hi.After(end) {
			hi = end
		}
		p := timescale.USDVolumeRestampParams{
			Groups: targets, From: lo, To: hi, FillNull: r.fillNull, Generation: r.generation,
		}
		var n int64
		if r.write {
			n, err = r.store.RestampExactTierUSDVolume(ctx, p)
		} else {
			n, err = r.store.CountUSDVolumeRestampCandidates(ctx, p)
		}
		if err != nil {
			return rows, int64(len(targets)), err
		}
		rows += n
		r.progress += uint64(n)                      //nolint:gosec // n is a non-negative row count
		r.hb.Progress(r.progress, uint64(lo.Unix())) //nolint:gosec // post-1970 timestamp
		if n > 0 {
			fmt.Printf("  %s..%s  %d row(s)\n", lo.Format("15:04"), hi.Format("15:04"), n)
		}
	}
	fmt.Printf("%s: %d row(s)\n", day.Format(time.DateOnly), rows)
	return rows, int64(len(targets)), nil
}

// exactTierGroups filters a day's groups to the exact-tier targets this
// run may touch and sums the verifier's |delta| over them — the "before"
// figure the operator compares against verify-usd-volume's report.
func (r *restampRun) exactTierGroups(groups []timescale.TradeValuationGroup) ([]timescale.USDVolumeRestampGroup, *big.Rat) {
	before := new(big.Rat)
	var targets []timescale.USDVolumeRestampGroup
	for _, g := range groups {
		if r.allow != nil && !r.allow[g.Source] {
			continue
		}
		tier, decimals, cerr := timescale.ClassifyUSDVolumeTier(g.Source, g.BaseAsset, g.QuoteAsset, r.spec)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "  UNCLASSIFIABLE %s %s/%s: %v\n", g.Source, g.BaseAsset, g.QuoteAsset, cerr)
			continue
		}
		if !tier.Exact() {
			continue
		}
		targets = append(targets, timescale.USDVolumeRestampGroup{
			Source: g.Source, BaseAsset: g.BaseAsset, QuoteAsset: g.QuoteAsset, Tier: tier, Decimals: decimals,
		})
		if delta, ok := timescale.ExactTierDelta(g, tier, decimals); ok {
			before.Add(before, new(big.Rat).Abs(delta))
		}
	}
	return targets, before
}
