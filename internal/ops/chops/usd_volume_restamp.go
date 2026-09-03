// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// usdVolumeRestamp is the stellarindex-ops `usd-volume-restamp` subcommand
// — the corrective WRITE half of verify-usd-volume (W5.3, v1-launch-plan;
// docs/operations/usd-volume-rederive-2026-08.md §5).
//
// It carries two TIERS, selected by -tier, because the usd_volume column
// holds two kinds of number and they are repairable by two different
// means:
//
//	-tier exact (default) — tiers 1/2/2b, where usd_volume is a pure
//	  decimal rescaling of an amount already on the row
//	  (`pegged_leg / 10^decimals`). Repairable as a SQL identity; that is
//	  what this file does. Fixes the pre-2026-07-23 class measured on
//	  2026-07-30: [2026-05-12, 2026-07-22], 66 dirty days, every violation
//	  a `[base_pegged] sdex` USDC-base row valued by the resolver's VWAP
//	  (+0.7%) instead of the $1 peg identity.
//
//	-tier xlm-base — the tier-4 XLM anchor
//	  (`base_amount/1e7 x XLM/USD at ts`), re-derived in GO through the
//	  store's own [timescale.Store] resolver rather than in SQL, because
//	  the value is a function of prices_1m at the row's timestamp and not
//	  of the row alone. Fixes issue #372: every XLM-base DEX trade written
//	  before `fd1860bd` was valued QUOTE-side through the counterparty's
//	  own thin book (a 43x under-valuation on the measured row) or left
//	  NULL (~31% of the population). See usd_volume_restamp_xlmbase.go.
//
// Discipline shared by both tiers:
//   - fail-closed DRY RUN by default; -write applies (opsutil.WriteGate);
//   - bounded window: -from/-to UTC days (inclusive), walked oldest →
//     newest one -slice at a time so no single UPDATE spans more than one
//     slice of a compressed chunk;
//   - a one-writer refusal: the window must sit BEHIND the live
//     ledgerstream cursor (checkRestampLiveOverlap), overridable only by
//     the explicit -allow-live-overlap;
//   - idempotent: a row that already holds the correct value is not
//     touched (value or derive_generation), so a re-run reports 0 — which
//     is also what makes an interrupted run resumable by re-running it;
//   - node_exporter heartbeat like ch-backfill (-heartbeat), so a wedged
//     run under run-heavy-job.sh trips the ops_job stall alerts;
//   - the tier decision and the value both come from the SAME functions
//     the insert path uses (timescale.ClassifyUSDVolumeTier for the exact
//     tiers, tradeUSDVolumeViaXLMBaseAnchor for the anchor) — never
//     re-spelled here.
//
// Acceptance after a run: `verify-usd-volume -day <last> -days <N>` over
// the span.
//
// Usage: usd-volume-restamp -config PATH -from YYYY-MM-DD -to YYYY-MM-DD
// [-tier exact|xlm-base] [-slice DUR] [-sources a,b] [-fill-null]
// [-heartbeat PATH] [-allow-live-overlap] [-write]
// and, for -tier xlm-base only: [-report] [-sample N] [-batch N]
// [-min-rel-delta F] [-max-generation N].
func usdVolumeRestamp(args []string) error { //nolint:gocognit,gocyclo,funlen // linear: parse, validate the tier's flag set, open+wire the store, run the live-overlap guard, dispatch — splitting scatters each guard away from the flag it guards.
	fs := flag.NewFlagSet("usd-volume-restamp", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml (Postgres DSN + the operator's USD peg list)")
	fromFlag := fs.String("from", "", "first UTC day of the window, YYYY-MM-DD (required)")
	toFlag := fs.String("to", "", "last UTC day of the window, YYYY-MM-DD, inclusive (required; must not be today — that chunk is still being written)")
	tier := fs.String("tier", restampTierExact, "which usd_volume tier to repair: "+restampTierExact+" (the SQL peg identity) or "+restampTierXLMBase+" (the tier-4 XLM anchor, re-derived in Go — issue #372)")
	slice := fs.Duration("slice", time.Hour, "time span of one planning/UPDATE window; bounds the per-transaction decompression + lock footprint")
	sources := fs.String("sources", "", "comma-separated source allow-list (default: every group the tier owns)")
	fillNull := fs.Bool("fill-null", false, "also stamp rows whose usd_volume is NULL (a COVERAGE change; off by default)")
	heartbeat := fs.String("heartbeat", "", "node_exporter textfile path for the liveness/progress gauges. Empty = "+opsutil.DefaultTextfileDir+"/ops_job_usd_volume_restamp.prom when that directory exists (r1), otherwise no heartbeat at all")
	allowLiveOverlap := fs.Bool("allow-live-overlap", false, "DANGEROUS: bypass the live-cursor guard and restamp a window the live ingest tail has not passed. Only pass this if you have independently verified the indexer will not write this range concurrently.")
	report := fs.Bool("report", false, "-tier "+restampTierXLMBase+" only: print the full decision block (candidates, NULL->value population, relative-move distribution, USD sums, extremes, sample) and write nothing. Refuses -write.")
	sample := fs.Int("sample", 10, "-tier "+restampTierXLMBase+" only: how many changed rows to print in the report's sample (deterministic reservoir)")
	batch := fs.Int("batch", 2000, "-tier "+restampTierXLMBase+" only: rows per UPDATE transaction")
	minRelDelta := fs.String("min-rel-delta", "", "-tier "+restampTierXLMBase+" only: skip writing rows whose relative move |new-old|/|old| is below this fraction (0.01 = 1%). Empty/0 = write every row that differs. Never skips a NULL fill.")
	maxGeneration := fs.Int64("max-generation", -1, "-tier "+restampTierXLMBase+" only: only consider rows at derive_generation <= this. -1 = the run's own generation (everything). 0 targets exactly the never-re-derived population.")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if err := validateRestampTierFlags(*tier, set); err != nil {
		return err
	}
	from, to, err := resolveRestampWindow(*fromFlag, *toFlag, time.Now())
	if err != nil {
		return err
	}
	if *slice <= 0 || *slice > 24*time.Hour {
		return fmt.Errorf("usd-volume-restamp: -slice must be in (0, 24h], got %s", *slice)
	}
	if *batch <= 0 {
		return fmt.Errorf("usd-volume-restamp: -batch must be > 0, got %d", *batch)
	}
	if *report && gate.Enabled() {
		return fmt.Errorf("usd-volume-restamp: -report is a READ-ONLY decision input and refuses -write; run the report first, then re-run with -write once you have decided")
	}
	minRel, err := parseMinRelDelta(*minRelDelta)
	if err != nil {
		return err
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
	// The xlm-base tier re-derives through the store's OWN waterfall, so
	// it needs the same wiring every trade writer gets — from the single
	// blessed installer, never a hand-tuned resolver (the drift class
	// InstallUSDVolumeResolution exists to close).
	if *tier == restampTierXLMBase {
		if err := timescale.InstallUSDVolumeResolution(store, cfg.Trades.USDPeggedClassicAssets, cfg.Supply.SACWrappers); err != nil {
			return fmt.Errorf("usd-volume-restamp: install usd-volume resolution: %w", err)
		}
	}

	write := gate.Banner()
	// INV-3: one generation for the whole run, like ch-rebuild.
	generation := time.Now().Unix()
	maxGen := *maxGeneration
	if maxGen < 0 {
		maxGen = generation
	}

	// ─── one-writer contract: the window must be BEHIND the live tail ──
	if err := restampLiveOverlapGuard(ctx, store, from, to, *allowLiveOverlap); err != nil {
		return err
	}

	hb := opsutil.NewJobHeartbeat("usd-volume-restamp", *heartbeat, nil)
	if hb.Enabled() {
		fmt.Fprintf(os.Stderr, "usd-volume-restamp: heartbeat -> %s\n", hb.Path())
	}
	hb.Start()
	ok := false
	defer func() { hb.Stop(ok) }()

	if *tier == restampTierXLMBase {
		rerr := runXLMBaseRestamp(ctx, store, *cfgPath, from, to, xlmBaseRestampOptions{
			Allow:         restampSourceAllowList(*sources),
			FillNull:      *fillNull,
			Slice:         *slice,
			Batch:         *batch,
			Write:         write,
			Report:        *report,
			SampleSize:    *sample,
			MinRelDelta:   minRel,
			MaxGeneration: maxGen,
			Generation:    generation,
			Heartbeat:     hb,
		})
		ok = rerr == nil
		return rerr
	}

	run := &restampRun{
		store:      store,
		spec:       spec,
		allow:      restampSourceAllowList(*sources),
		fillNull:   *fillNull,
		slice:      *slice,
		write:      write,
		generation: generation,
		hb:         hb,
	}

	fmt.Fprintf(os.Stderr, "usd-volume-restamp: tier=exact window [%s, %s] slice=%s generation=%d fill_null=%v\n",
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

// The two -tier values. Named constants so the flag help, the dispatch
// and the flag-compatibility check cannot disagree about the spelling.
const (
	restampTierExact   = "exact"
	restampTierXLMBase = "xlm-base"
)

// restampXLMBaseOnlyFlags are the flags that only mean something for
// `-tier xlm-base`. Passing one with `-tier exact` is an ERROR rather
// than a silent no-op: an operator who typed `-report` and got a write
// run, or who typed `-min-rel-delta 0.3` and got every row rewritten,
// has been actively misled by the tool on a money column.
var restampXLMBaseOnlyFlags = []string{"report", "sample", "batch", "min-rel-delta", "max-generation"}

// validateRestampTierFlags rejects an unknown -tier and any xlm-base-only
// flag passed alongside -tier exact.
func validateRestampTierFlags(tier string, set map[string]bool) error {
	switch tier {
	case restampTierExact:
		var stray []string
		for _, f := range restampXLMBaseOnlyFlags {
			if set[f] {
				stray = append(stray, "-"+f)
			}
		}
		if len(stray) > 0 {
			return fmt.Errorf("usd-volume-restamp: %s only apply to -tier %s; you passed -tier %s, where they would be silently ignored",
				strings.Join(stray, ", "), restampTierXLMBase, restampTierExact)
		}
		return nil
	case restampTierXLMBase:
		return nil
	default:
		return fmt.Errorf("usd-volume-restamp: -tier %q: want %q or %q", tier, restampTierExact, restampTierXLMBase)
	}
}

// restampLiveOverlapGuard resolves the window's top ledger and applies
// [checkRestampLiveOverlap]. Applies to BOTH tiers: they write the same
// column of the same table, so they carry the same one-writer hazard.
func restampLiveOverlapGuard(ctx context.Context, store *timescale.Store, from, to time.Time, allowOverlap bool) error {
	top, err := resolveWindowTopLedger(ctx, store, from, to)
	if err != nil {
		return fmt.Errorf("usd-volume-restamp: resolve window top ledger: %w", err)
	}
	liveCursor, gerr := store.GetCursor(ctx, "ledgerstream", "")
	haveLive := gerr == nil
	if gerr != nil && !errors.Is(gerr, timescale.ErrNotFound) {
		return fmt.Errorf("usd-volume-restamp: read live ledgerstream cursor: %w", gerr)
	}
	if err := checkRestampLiveOverlap(haveLive, liveCursor.LastLedger, top, allowOverlap); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "usd-volume-restamp: window top ledger %d, live ledgerstream cursor %d — restamping strictly behind the live tail\n",
		top, liveCursor.LastLedger)
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
