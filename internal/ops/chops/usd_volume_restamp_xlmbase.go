// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"
	"math/big"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── `usd-volume-restamp -tier xlm-base` — the anchor re-derive ─────
//
// Unlike the exact tier (usd_volume_restamp.go), this re-derives an ESTIMATED tier via
// usdVolumeViaXLMBaseAnchor (the live insert calls it too). A row the anchor cannot price is
// reported, never guessed: a quote-side fall-through at a high derive_generation would be
// permanent. Refuses a range the live ingest tail has not passed (checkRestampLiveOverlap).
// `-report` prints the decision block to read BEFORE a production run; it writes nothing.

// xlmBaseRestampStore is the slice of [timescale.Store] the re-derive uses (plan a window, apply
// the plan), a seam so the chunk walk can be driven against a scripted double.
type xlmBaseRestampStore interface {
	PlanXLMBaseUSDVolumeRestamp(ctx context.Context, p timescale.XLMBaseRestampParams) (*timescale.XLMBaseRestampPlan, error)
	ApplyXLMBaseUSDVolumeRestamp(ctx context.Context, plan *timescale.XLMBaseRestampPlan, generation int64, batch int) (int64, error)
}

// xlmBaseRestampRun carries one ESTIMATED tier's fixed inputs and running totals across days or chunks; despite the name it serves every
// estimated tier (xlm-base, xlm-quote, cex-fx), which differ only in the [estimatedTierProfile] below.
type xlmBaseRestampRun struct {
	// tier is the -tier value this run repairs; plan and apply are that
	// tier's own halves of the walk.
	tier  string
	scope string
	plan  estimatedPlan
	apply xlmBaseApply
	// headerFlags / resumeFlags are the tier-specific flags the header
	// and the RESUME line carry; empty for a tier with none.
	headerFlags string
	resumeFlags string
	// scanLabel / declineLabel are the report's two tier-specific lines.
	scanLabel    string
	declineLabel string

	allow       map[string]bool
	fillNull    bool
	slice       time.Duration
	batch       int
	write       bool
	report      bool
	sampleSize  int
	minRelDelta *big.Rat
	maxGen      int64
	generation  int64

	hb       *opsutil.JobHeartbeat
	progress uint64

	totals  timescale.XLMBaseRestampStats
	written int64

	// maxAbs / maxRel are the running extremes over every CHANGED row
	// seen so far — the two rows an operator most wants to eyeball
	// before authorising a 28M-row rewrite.
	maxAbs    *timescale.XLMBaseRestampRow
	maxRel    *timescale.XLMBaseRestampRow
	sample    []timescale.XLMBaseRestampRow
	seen      int64
	sampleRNG *rand.Rand
}

// xlmBaseSampleSeed fixes the report sample's RNG so two runs over the same data print the same
// sample, which a re-checked decision input needs. Reservoir sampling spans the whole window.
const xlmBaseSampleSeed = 0x53544c4c // "STLL"

// runXLMBaseRestamp is the `-tier xlm-base` in-place entry point, called
// by usdVolumeRestamp once the shared window/flag validation has passed.
func runXLMBaseRestamp(ctx context.Context, store xlmBaseRestampStore, cfgPath string, from, to time.Time, opts xlmBaseRestampOptions) error {
	return runEstimatedRestamp(ctx, newXLMBaseRestampRun(store, opts), cfgPath, from, to, opts)
}

// runEstimatedRestamp is the in-place day walk for whichever estimated tier the run carries: one UTC day at a time, oldest first, each in
// -slice windows; only the population the run's planner returns differs per tier.
func runEstimatedRestamp(ctx context.Context, run *xlmBaseRestampRun, cfgPath string, from, to time.Time, opts xlmBaseRestampOptions) error {
	fmt.Fprintf(os.Stderr, "usd-volume-restamp: tier=%s window [%s, %s] slice=%s batch=%d generation=%d max-generation=%d fill_null=%v min_rel_delta=%s%s\n",
		run.tier, from.Format(time.DateOnly), to.Format(time.DateOnly), opts.Slice, opts.Batch,
		opts.Generation, opts.MaxGeneration, opts.FillNull, ratPercent(opts.MinRelDelta), run.headerFlags)
	fmt.Fprintf(os.Stderr, "usd-volume-restamp: %s\n", run.scope)

	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		if err := run.day(ctx, day); err != nil {
			run.printResumeHint(cfgPath, day, to, opts)
			return err
		}
	}
	run.printReport(from, to)
	fmt.Print(run.summary(cfgPath, from, to))
	return nil
}

// estimatedPlan is a tier's window planner (scans [lo, hi) and decides every row); this seam lets one run type carry three tiers, since
// only the population differs.
type estimatedPlan func(ctx context.Context, p timescale.RestampScanParams) (*timescale.RestampPlan, error)

// estimatedTierProfile is everything that distinguishes one estimated
// tier's run from another's.
type estimatedTierProfile struct {
	// Tier is the -tier value, printed in every header, summary and
	// RESUME line.
	Tier string
	// Scope is the operator-facing description of the population about
	// to be scanned, printed before the first read.
	Scope string
	// Plan is the tier's window planner; Apply its write half for a walk
	// that is not inside a decompressed chunk (the chunk walk passes the
	// guarded in-chunk apply per chunk instead).
	Plan  estimatedPlan
	Apply xlmBaseApply
	// HeaderFlags and ResumeFlags are the tier's own flags, rendered for
	// the run header and for the RESUME line. Empty for a tier with none.
	HeaderFlags string
	ResumeFlags string
	// ScanLabel names the SQL predicate the tier scans with, and
	// DeclineLabel what its valuer is, for the report's two lines that
	// cannot be written tier-agnostically.
	ScanLabel    string
	DeclineLabel string
}

// xlmBaseTierProfile is the XLM-base anchor re-derive's profile.
func xlmBaseTierProfile(store xlmBaseRestampStore) estimatedTierProfile {
	return estimatedTierProfile{
		Tier:         restampTierXLMBase,
		Scope:        "DEX sources in scope: " + strings.Join(timescale.DEXSourceNames(), ", "),
		Plan:         store.PlanXLMBaseUSDVolumeRestamp,
		Apply:        store.ApplyXLMBaseUSDVolumeRestamp,
		ScanLabel:    "source=DEX, base=XLM form",
		DeclineLabel: "anchor declined",
	}
}

// newXLMBaseRestampRun builds an xlm-base run from resolved options, shared by the day and chunk walks so they cannot drift in what a run
// carries.
func newXLMBaseRestampRun(store xlmBaseRestampStore, opts xlmBaseRestampOptions) *xlmBaseRestampRun {
	return newEstimatedRestampRun(opts, xlmBaseTierProfile(store))
}

// newEstimatedRestampRun builds a run for whichever estimated tier the
// profile describes.
func newEstimatedRestampRun(opts xlmBaseRestampOptions, p estimatedTierProfile) *xlmBaseRestampRun {
	return &xlmBaseRestampRun{
		tier:         p.Tier,
		scope:        p.Scope,
		plan:         p.Plan,
		apply:        p.Apply,
		headerFlags:  p.HeaderFlags,
		resumeFlags:  p.ResumeFlags,
		scanLabel:    p.ScanLabel,
		declineLabel: p.DeclineLabel,

		allow:       opts.Allow,
		fillNull:    opts.FillNull,
		slice:       opts.Slice,
		batch:       opts.Batch,
		write:       opts.Write,
		report:      opts.Report,
		sampleSize:  opts.SampleSize,
		minRelDelta: opts.MinRelDelta,
		maxGen:      opts.MaxGeneration,
		generation:  opts.Generation,
		hb:          opts.Heartbeat,
		totals:      timescale.NewXLMBaseRestampStats(),
		sampleRNG:   rand.New(rand.NewPCG(xlmBaseSampleSeed, xlmBaseSampleSeed)), //nolint:gosec // reporting sample, not a security decision
	}
}

// summary renders the run's closing block (row count, planned-vs-changed warning, CAGG follow-up, acceptance line), printed after the
// report by both walks.
func (r *xlmBaseRestampRun) summary(cfgPath string, from, to time.Time) string {
	var b strings.Builder
	verb := "would restamp"
	if r.write {
		verb = "restamped"
	}
	fmt.Fprintf(&b, "\nusd-volume-restamp: %s %d row(s) in [%s, %s] (tier %s)\n",
		verb, r.totals.Changed, from.Format(time.DateOnly), to.Format(time.DateOnly), r.tier)
	if r.write && r.written != r.totals.Changed {
		// The write set IS the plan, and the UPDATE skips a row that no
		// longer matches what the plan read, so these diverge only when a
		// concurrent writer moved rows between the plan and the UPDATE.
		// That is a one-writer-contract violation — say so loudly.
		fmt.Fprintf(&b, "WARNING: %d row(s) planned but %d row(s) changed — a concurrent writer moved rows after the plan read them; they were left as that writer set them\n",
			r.totals.Changed, r.written)
	}
	b.WriteString(r.followUp(from, to))
	fmt.Fprintf(&b, "acceptance: stellarindex-ops verify-usd-volume -config %s -day %s -days %d\n",
		cfgPath, to.Format(time.DateOnly), int(to.Sub(from).Hours()/24)+1)
	return b.String()
}

// xlmBaseRestampCAGGs is the ordered continuous aggregates a finished restamp must be followed by, with each one's minimum refresh window
// (Timescale rejects under 2x the bucket, SQLSTATE 22023). Served volume reads aggregates that auto-refresh only days to months back, so
// without this verify-usd-volume goes green while the API serves pre-restamp numbers. Aliases [timescale.TradesCAGGs].
var xlmBaseRestampCAGGs = timescale.TradesCAGGs

// followUp is the run's post-write block: the ordered CAGG refresh every tier needs plus the `-min-rel-delta` guidance only the XLM-base
// re-derive has measurements for.
func (r *xlmBaseRestampRun) followUp(from, to time.Time) string {
	if r.tier == restampTierXLMBase {
		return xlmBaseRestampFollowUp(from, to)
	}
	return restampCAGGFollowUp(from, to)
}

// xlmBaseRestampFollowUp renders the post-write follow-up (ordered CAGG refresh block plus `-min-rel-delta` guidance); returns a string so
// the ordering is testable.
func xlmBaseRestampFollowUp(from, to time.Time) string {
	return restampCAGGFollowUp(from, to) + xlmBaseMinRelDeltaGuidance
}

// xlmBaseMinRelDeltaGuidance is the anchor re-derive's flag advice; the exact tier has no such threshold (a row is either on the identity
// or off it).
const xlmBaseMinRelDeltaGuidance = "\nFLAG GUIDANCE — `-min-rel-delta 0.001` is the recommended setting for a full-window\n" +
	"run. The re-derive reads the FINALISED prices_1m bucket while the original insert\n" +
	"read the partially-materialised real-time bucket for the same minute, so a large\n" +
	"share of the write set moves by less than 0.1% — repairing nothing, on a\n" +
	"COMPRESSED hypertable where the write is the expensive part. Measured over\n" +
	"2026-01-01..2026-07-21: of 10,734,569 non-null-fill changes, 8,423,350 move >=0.1%,\n" +
	"so the flag drops 2,311,219 rows (8.1% of the 28,583,186-row write set) at a cost\n" +
	"of <0.1% each. It never suppresses a NULL fill, which is where the coverage\n" +
	"recovery lives, so the tool's whole point survives the flag.\n\n"

// restampCAGGFollowUp renders the ordered CAGG refresh block every finished restamp needs: served volume reads aggregates that don't
// refresh this far back. Window [from 00:00Z, to+1d 00:00Z) is padded by [timescale.PadRefreshWindow] to clear Timescale's 2-bucket
// minimum; a rejected call leaves a stale surface.
func restampCAGGFollowUp(from, to time.Time) string {
	lo := from.UTC().Truncate(24 * time.Hour)
	hi := to.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)

	var b strings.Builder
	b.WriteString("\nSTILL STALE AFTER A -write RUN — the acceptance check below CANNOT see this.\n")
	b.WriteString("verify-usd-volume reads `trades` directly; every served volume surface reads a\n")
	b.WriteString("continuous aggregate, and none auto-refresh further back than 7 days (prices_1m:\n")
	b.WriteString("5 minutes). Until these run, /v1/markets volume, asset volume, venue rankings and\n")
	b.WriteString("every chart keep serving the PRE-restamp numbers. Order is load-bearing:\n")
	b.WriteString("twap_1h/twap_1d are built ON prices_1m, so prices_1m goes first, FORCED over every\n")
	b.WriteString("window the twaps read. If prices_1m's retention policy is armed, disarm it first as\n")
	b.WriteString("migrations/0156_prices_1m_retention.up.sql states: a twap refresh over dropped\n")
	b.WriteString("minute rows deletes TWAP history.\n\n")
	plan := timescale.PlanCAGGRefresh(xlmBaseRestampCAGGs, func(c timescale.CAGGSpec) (time.Time, time.Time) {
		return timescale.PadRefreshWindow(lo, hi, c.MinWindow)
	})
	for _, st := range plan {
		force := ""
		if st.Force {
			force = ", force => true"
		}
		fmt.Fprintf(&b, "  CALL refresh_continuous_aggregate('%s', '%s', '%s'%s);\n",
			st.View, st.From.Format(time.RFC3339), st.To.Format(time.RFC3339), force)
	}
	b.WriteString("\nThen force the asset_volume_24h rollup (it re-sums prices_1m.volume_usd; it also\n")
	b.WriteString("self-heals on its own cadence, so verify rather than assume).\n")
	return b.String()
}

// xlmBaseRestampOptions is the already-resolved flag set, a struct so tests can build one without a flag.FlagSet.
type xlmBaseRestampOptions struct {
	Allow         map[string]bool
	FillNull      bool
	Slice         time.Duration
	Batch         int
	Write         bool
	Report        bool
	SampleSize    int
	MinRelDelta   *big.Rat
	MaxGeneration int64
	Generation    int64
	Heartbeat     *opsutil.JobHeartbeat
}

// day plans (and, under -write, applies) one UTC day in -slice windows.
func (r *xlmBaseRestampRun) day(ctx context.Context, day time.Time) error {
	w, err := r.walk(ctx, day, day.AddDate(0, 0, 1), xlmBaseWalkFull)
	if err != nil {
		return err
	}
	r.totals.Merge(w.stats)
	verb := "would change"
	if r.write {
		verb = fmt.Sprintf("changed %d, planned", w.written)
	}
	fmt.Printf("=== %s: scanned %d, %s %d (null-fill %d, anchor-declined %d/%d null/valued, already correct %d)\n",
		day.Format(time.DateOnly), w.stats.Scanned, verb, w.stats.Changed,
		w.stats.NullFilled, w.stats.AnchorDeclinedNull, w.stats.AnchorDeclinedStored,
		w.stats.Unchanged)
	return nil
}

// xlmBaseWalkMode selects what a walk does with each slice's plan.
type xlmBaseWalkMode int

const (
	// xlmBaseWalkFull folds every slice into the report and, under -write, applies its plan: the day walk and the inside of a decompressed
	// chunk.
	xlmBaseWalkFull xlmBaseWalkMode = iota
	// xlmBaseWalkProbe is the chunk walk's read-only pre-check: plans slice by slice, folds nothing (the caller folds a clean count) and
	// stops at the first slice that would change a row, so a clean chunk is skipped undecompressed.
	xlmBaseWalkProbe
)

// xlmBaseWalk is what one walk over [lo, hi) found: the merged window
// statistics and the rows the database actually changed.
type xlmBaseWalk struct {
	stats   timescale.XLMBaseRestampStats
	written int64
}

// xlmBaseApply is the write half of a walk: the day walk passes the store's apply, the chunk walk one that checks the chunk is still
// decompressed before every batch.
type xlmBaseApply func(ctx context.Context, plan *timescale.XLMBaseRestampPlan, generation int64, batch int) (int64, error)

// walk plans (and, in full mode under -write, applies) [lo, hi) in
// -slice windows with the store's own apply.
func (r *xlmBaseRestampRun) walk(ctx context.Context, lo, hi time.Time, mode xlmBaseWalkMode) (xlmBaseWalk, error) {
	return r.walkWith(ctx, lo, hi, mode, r.apply)
}

// walkWith is [xlmBaseRestampRun.walk] with the apply chosen by the caller, agnostic of whether [lo, hi) is a UTC day or a chunk's slice.
func (r *xlmBaseRestampRun) walkWith(ctx context.Context, lo, hi time.Time, mode xlmBaseWalkMode, apply xlmBaseApply) (xlmBaseWalk, error) {
	w := xlmBaseWalk{stats: timescale.NewXLMBaseRestampStats()}
	for s := lo; s.Before(hi); s = s.Add(r.slice) {
		if err := ctx.Err(); err != nil {
			return w, err
		}
		e := s.Add(r.slice)
		if e.After(hi) {
			e = hi
		}
		plan, err := r.plan(ctx, timescale.RestampScanParams{
			From: s, To: e, Sources: r.allow, FillNull: r.fillNull,
			MaxGeneration: r.maxGen, MinRelDelta: r.minRelDelta,
		})
		if err != nil {
			return w, err
		}
		w.stats.Merge(plan.Stats)
		if mode == xlmBaseWalkProbe {
			if plan.Stats.Changed > 0 {
				return w, nil
			}
			continue
		}
		r.observe(plan)
		if r.write && len(plan.Rows) > 0 {
			n, aerr := apply(ctx, plan, r.generation, r.batch)
			w.written += n
			r.written += n
			if aerr != nil {
				return w, aerr
			}
		}
		r.progress += uint64(plan.Stats.Changed)    //nolint:gosec // a non-negative row count
		r.hb.Progress(r.progress, uint64(s.Unix())) //nolint:gosec // post-1970 timestamp
		if plan.Stats.Changed > 0 {
			fmt.Printf("  %s %s..%s  %d change(s) (%d null-fill, %d anchor-declined)\n",
				s.Format(time.DateOnly), s.Format("15:04"), e.Format("15:04"),
				plan.Stats.Changed, plan.Stats.NullFilled,
				plan.Stats.AnchorDeclinedNull+plan.Stats.AnchorDeclinedStored)
		}
	}
	return w, nil
}

// observe folds a window's changed rows into the running extremes and report sample, then drops them: a whole-span run touches ~28M rows.
func (r *xlmBaseRestampRun) observe(plan *timescale.XLMBaseRestampPlan) {
	for i := range plan.Rows {
		row := plan.Rows[i]
		if row.AbsDelta != nil && (r.maxAbs == nil || row.AbsDelta.Cmp(r.maxAbs.AbsDelta) > 0) {
			cp := row
			r.maxAbs = &cp
		}
		if row.RelOK && (r.maxRel == nil || !r.maxRel.RelOK || row.RelDelta.Cmp(r.maxRel.RelDelta) > 0) {
			cp := row
			r.maxRel = &cp
		}
		r.seen++
		if r.sampleSize <= 0 {
			continue
		}
		if len(r.sample) < r.sampleSize {
			r.sample = append(r.sample, row)
			continue
		}
		// Classic reservoir: replace with probability sampleSize/seen.
		if j := r.sampleRNG.Int64N(r.seen); j < int64(r.sampleSize) {
			r.sample[j] = row
		}
	}
}

// printResumeHint tells the operator how to continue an interrupted run; the tool is idempotent (a row already holding the anchor's value
// is not a candidate), so restarting at the failed day re-checks it cheaply rather than re-writing it.
func (r *xlmBaseRestampRun) printResumeHint(cfgPath string, failed, to time.Time, opts xlmBaseRestampOptions) {
	var b strings.Builder
	fmt.Fprintf(&b, "\nRESUME: stellarindex-ops usd-volume-restamp -config %s -tier %s -from %s -to %s",
		cfgPath, r.tier, failed.Format(time.DateOnly), to.Format(time.DateOnly))
	b.WriteString(xlmBaseResumeFlags(opts))
	b.WriteString(r.resumeFlags)
	if opts.Batch != defaultRestampBatch {
		fmt.Fprintf(&b, " -batch %d", opts.Batch)
	}
	if opts.Write {
		b.WriteString(" -write")
	}
	b.WriteString("\n  (idempotent: days already restamped re-scan and report 0 changes)")
	fmt.Fprintln(os.Stderr, b.String())
}

// printReport writes the decision block after every run: under -report it is the whole output, after -write it is the record of what the
// run did.
func (r *xlmBaseRestampRun) printReport(from, to time.Time) {
	s := r.totals
	days := int(to.Sub(from).Hours()/24) + 1
	fmt.Printf("\n=== usd-volume-restamp REPORT — tier %s — [%s, %s] (%d day(s)) ===\n",
		r.tier, from.Format(time.DateOnly), to.Format(time.DateOnly), days)
	fmt.Printf("scanned (%s, derive_generation <= %d)   %d\n", r.scanLabel, r.maxGen, s.Scanned)
	fmt.Printf("  a leg is USD-pegged (EXACT tier, `-tier exact` owns these)   %d\n", s.QuotePegged)
	fmt.Printf("  outside this tier's source/leg scope                         %d\n", s.NotDEX)
	fmt.Printf("  unparseable amount or stored value                           %d\n", s.Unparseable)
	fmt.Printf("  %s, stored NULL   (coverage NOT recoverable)    %d\n", r.declineLabel, s.AnchorDeclinedNull)
	fmt.Printf("  %s, stored VALUE  (STAYS WRONG after this run)  %d\n", r.declineLabel, s.AnchorDeclinedStored)
	if s.FXDeclinedStale > 0 {
		fmt.Printf("    of those, refused for want of a quote within the as-of tolerance  %d\n", s.FXDeclinedStale)
	}
	fmt.Printf("  already correct                                              %d\n", s.Unchanged)
	fmt.Printf("  suppressed by -min-rel-delta                                 %d\n", s.BelowMinRelDelta)
	fmt.Printf("  WOULD CHANGE                                                 %d\n", s.Changed)
	fmt.Printf("    of which NULL -> value                                     %d\n", s.NullFilled)
	fmt.Printf("  stored-NULL rows the anchor CAN price (with -fill-null)      %d\n", s.NullCandidates)
	fmt.Printf("  residual (must be 0; non-zero = a row was filed nowhere)     %d\n", s.Residual())

	fmt.Printf("\nrelative move of the changed rows (|new-old|/|old|; NULL fills have no ratio)\n")
	for _, b := range timescale.XLMBaseRelBuckets {
		fmt.Printf("  %-8s %d\n", b.Label, s.RelBucket[b.Label])
	}
	fmt.Printf("\nUSD sums over the changed rows (excludes NULL fills on the stored side)\n")
	fmt.Printf("  stored  %s\n", s.SumStored.FloatString(8))
	fmt.Printf("  want    %s\n", s.SumWant.FloatString(8))
	fmt.Printf("  delta   %s\n", new(big.Rat).Sub(s.SumWant, s.SumStored).FloatString(8))

	if r.maxAbs != nil {
		fmt.Printf("\nlargest ABSOLUTE delta\n  %s\n", formatXLMBaseRow(*r.maxAbs))
	}
	if r.maxRel != nil {
		fmt.Printf("largest RELATIVE delta\n  %s\n", formatXLMBaseRow(*r.maxRel))
	}
	if len(r.sample) > 0 {
		fmt.Printf("\nsample (%d of %d changed rows, deterministic reservoir)\n", len(r.sample), s.Changed)
		for _, row := range r.sample {
			fmt.Printf("  %s\n", formatXLMBaseRow(row))
		}
	}
}

// formatXLMBaseRow renders one decided row for the report: enough of the
// primary key to look it up on r1, both values, and the move.
func formatXLMBaseRow(r timescale.XLMBaseRestampRow) string {
	stored := "NULL"
	if r.Stored != nil {
		stored = *r.Stored
	}
	rel := "n/a"
	if r.RelOK {
		rel = ratPercent(r.RelDelta)
	}
	abs := "0"
	if r.AbsDelta != nil {
		abs = r.AbsDelta.FloatString(8)
	}
	return fmt.Sprintf("%s L%d %s/%d %s %s/%s  %s -> %s  (abs %s, rel %s)",
		r.Source, r.Ledger, shortHash(r.TxHash), r.OpIndex,
		r.TS.Format(time.RFC3339), r.BaseAsset, shortAsset(r.QuoteAsset),
		stored, r.Want, abs, rel)
}

// shortHash trims a 64-char tx hash to its first 12 chars — enough to
// find the row, short enough that a report line fits a terminal.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12] + "…"
}

// shortAsset trims a CODE-ISSUER asset id to CODE-<first 4 of issuer>.
func shortAsset(a string) string {
	if len(a) <= 16 {
		return a
	}
	return a[:16] + "…"
}

// ratPercent renders a ratio as a percentage for the report. nil is "0%"
// — the "no threshold" default.
func ratPercent(r *big.Rat) string {
	if r == nil {
		return "0%"
	}
	return new(big.Rat).Mul(r, big.NewRat(100, 1)).FloatString(4) + "%"
}

// parseMinRelDelta turns -min-rel-delta (a fraction, 0.01 = 1%) into a *big.Rat. Empty or "0" writes every differing row, the default,
// since leaving a money column slightly wrong is not defensible; the knob only bounds a first production run to the large moves.
func parseMinRelDelta(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("usd-volume-restamp: -min-rel-delta %q: want a decimal fraction (0.01 = 1%%)", s)
	}
	if r.Sign() < 0 {
		return nil, fmt.Errorf("usd-volume-restamp: -min-rel-delta %s is negative", s)
	}
	if r.Sign() == 0 {
		return nil, nil
	}
	return r, nil
}

// checkRestampLiveOverlap is the one-writer contract for a `trades` re-stamp (cf. [checkLiveCursorGuard]): `trades.usd_volume` has one live
// writer, the indexer at the `ledgerstream` cursor, so a restamp is legitimate only behind it, else its high derive_generation wins ON
// CONFLICT and discards the ingest value. haveLive=false (no cursor) is a conservative REFUSAL; allowOverlap asserts the indexer will not
// touch the range.
func checkRestampLiveOverlap(haveLive bool, liveLastLedger, windowTopLedger uint32, allowOverlap bool) error {
	if allowOverlap {
		return nil
	}
	if windowTopLedger == 0 {
		// The window holds no on-chain rows at all, so there is nothing
		// for the live tail to collide with. Vacuously safe.
		return nil
	}
	if haveLive && liveLastLedger >= windowTopLedger {
		return nil
	}
	cur := "none (the indexer has never run)"
	if haveLive {
		cur = fmt.Sprintf("%d", liveLastLedger)
	}
	return fmt.Errorf("usd-volume-restamp: refusing to run — the live ledgerstream cursor is %s, "+
		"which is below the top of the requested window (ledger %d). A restamp only ever rewrites history the "+
		"live ingest tail has already passed: overlapping it lets the restamp's derive_generation win over the "+
		"ingest path's own value (or the reverse), so one of the two writers is silently discarded. "+
		"Wait for the indexer to pass ledger %d, narrow -to, or pass -allow-live-overlap if you have "+
		"independently verified this is safe",
		cur, windowTopLedger, windowTopLedger)
}

// resolveWindowTopLedger finds the highest on-chain ledger the window can contain for [checkRestampLiveOverlap]; ledger is monotonic in
// `ts`, so it walks back from the last day past empty days. A window with no on-chain rows returns (0, nil), vacuously safe.
func resolveWindowTopLedger(ctx context.Context, store *timescale.Store, from, to time.Time) (uint32, error) {
	for day := to; !day.Before(from); day = day.AddDate(0, 0, -1) {
		top, ok, err := store.MaxTradeLedgerInRange(ctx, day, day.AddDate(0, 0, 1))
		if err != nil {
			return 0, err
		}
		if ok {
			return top, nil
		}
	}
	return 0, nil
}
