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

// ─── `usd-volume-restamp -tier xlm-base` — the #372 re-derive ─────────
//
// The exact-tier half of this command (usd_volume_restamp.go) repairs a
// SQL identity. This half re-derives an ESTIMATED tier, so it is a
// different shape of job and carries a different set of guards:
//
//   - the value is computed in Go by the store's own
//     tradeUSDVolumeViaXLMBaseAnchor — the function the live insert path
//     calls — against the installed VWAPUSDFXResolver, so the restamped
//     number and the number a re-inserted row would carry are the same
//     number by construction, not by two implementations agreeing;
//   - a row the anchor cannot price is REPORTED, never guessed at. The
//     live path would fall through to the quote side; that fall-through
//     IS the defect #372 is about, and committing it at a high
//     derive_generation would make it permanent;
//   - the run is bounded and resumable: -from/-to days, each walked in
//     -slice windows, each window's write set applied in -batch UPDATE
//     transactions. Nothing spans a chunk, and the tool is idempotent, so
//     a killed run is resumed by re-running it from the last printed day;
//   - it refuses to run over a range the live ingest tail has not passed
//     (checkRestampLiveOverlap), the same one-writer contract
//     projected-rebuild enforces for the projector.
//
// `-report` adds the decision block the operator reads BEFORE authorising
// a production run: candidate counts, the NULL->value population, the
// distribution of relative moves, the USD sums, the extremes and a
// sample. It writes nothing and refuses -write.

// xlmBaseRestampStore is the slice of [timescale.Store] the re-derive
// walks through: plan a window, apply the plan. A seam rather than the
// concrete store so the chunk walk (usd_volume_restamp_chunks.go) can be
// driven against a scripted double; production passes the store itself.
type xlmBaseRestampStore interface {
	PlanXLMBaseUSDVolumeRestamp(ctx context.Context, p timescale.XLMBaseRestampParams) (*timescale.XLMBaseRestampPlan, error)
	ApplyXLMBaseUSDVolumeRestamp(ctx context.Context, plan *timescale.XLMBaseRestampPlan, generation int64, batch int) (int64, error)
}

// xlmBaseRestampRun carries one `-tier xlm-base` invocation's fixed
// inputs and its running totals across days (or chunks).
type xlmBaseRestampRun struct {
	store xlmBaseRestampStore

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

// xlmBaseSampleSeed fixes the report sample's RNG so two runs over the
// same data print the SAME sample. A report is a decision input that gets
// pasted into an issue and re-checked later; a sample that reshuffles on
// every run cannot be re-checked. Reservoir sampling (rather than "the
// first N rows") is what makes the sample span the whole window instead
// of only its first slice.
const xlmBaseSampleSeed = 0x53544c4c // "STLL"

// runXLMBaseRestamp is the `-tier xlm-base` entry point, called by
// usdVolumeRestamp once the shared window/flag validation has passed.
func runXLMBaseRestamp(ctx context.Context, store xlmBaseRestampStore, cfgPath string, from, to time.Time, opts xlmBaseRestampOptions) error {
	run := newXLMBaseRestampRun(store, opts)

	sources := timescale.DEXSourceNames()
	fmt.Fprintf(os.Stderr, "usd-volume-restamp: tier=xlm-base window [%s, %s] slice=%s batch=%d generation=%d max-generation=%d fill_null=%v min_rel_delta=%s\n",
		from.Format(time.DateOnly), to.Format(time.DateOnly), opts.Slice, opts.Batch,
		opts.Generation, opts.MaxGeneration, opts.FillNull, ratPercent(opts.MinRelDelta))
	fmt.Fprintf(os.Stderr, "usd-volume-restamp: DEX sources in scope: %s\n", strings.Join(sources, ", "))

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

// newXLMBaseRestampRun builds a run from its resolved options. Shared by
// the day walk and the chunk walk so the two cannot drift in what a run
// carries.
func newXLMBaseRestampRun(store xlmBaseRestampStore, opts xlmBaseRestampOptions) *xlmBaseRestampRun {
	return &xlmBaseRestampRun{
		store:       store,
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

// summary renders the run's closing block: the row count, the
// planned-vs-changed warning, the CAGG follow-up and the acceptance line.
// Printed after the report by both walks.
func (r *xlmBaseRestampRun) summary(cfgPath string, from, to time.Time) string {
	var b strings.Builder
	verb := "would restamp"
	if r.write {
		verb = "restamped"
	}
	fmt.Fprintf(&b, "\nusd-volume-restamp: %s %d row(s) in [%s, %s] (tier xlm-base)\n",
		verb, r.totals.Changed, from.Format(time.DateOnly), to.Format(time.DateOnly))
	if r.write && r.written != r.totals.Changed {
		// The write set IS the plan, so these can only diverge when a
		// concurrent writer moved a row past the generation guard between
		// the plan and the UPDATE. That is a one-writer-contract
		// violation, not a rounding difference — say so loudly.
		fmt.Fprintf(&b, "WARNING: %d row(s) planned but %d row(s) changed — a concurrent writer moved rows past the derive_generation guard\n",
			r.totals.Changed, r.written)
	}
	b.WriteString(xlmBaseRestampFollowUp(from, to))
	fmt.Fprintf(&b, "acceptance: stellarindex-ops verify-usd-volume -config %s -day %s -days %d\n",
		cfgPath, to.Format(time.DateOnly), int(to.Sub(from).Hours()/24)+1)
	return b.String()
}

// xlmBaseRestampCAGGs is the ORDERED list of continuous aggregates a
// finished restamp must be followed by, with each one's minimum refresh
// window (Timescale rejects `SQLSTATE 22023: refresh window too small`
// for anything narrower than 2× the bucket).
//
// # Why this is printed at all (#372 F3)
//
// The `acceptance:` line below runs `verify-usd-volume`, which reads
// `trades` DIRECTLY (TradeValuationByDay). Every SERVED volume surface —
// /v1/markets volume, asset volume, venue rankings, market share, every
// chart — reads a continuous aggregate instead, and none of them
// auto-refresh anywhere near this far back. Measured `start_offset` on r1
// 2026-09-03: prices_1m 5 min, prices_15m 1 h, prices_1h 4 h,
// prices_4h 1 day, prices_1d / prices_1w / dex_volume_by_pair_1d /
// source_volume_1h / pools_per_source_1h 7 days (prices_1w 28 days,
// prices_1mo 3 months). So without this step the acceptance check goes
// GREEN while every served surface keeps serving pre-restamp numbers
// indefinitely.
//
// # The membership + order are DERIVED, not copied
//
// From `_timescaledb_catalog.continuous_agg` on r1 2026-09-03: exactly
// these twelve aggregates have `trades` as their root hypertable
// (oracle_prices_* hang off `oracle_updates`, supply_1d off
// `asset_supply_history` — a usd_volume restamp cannot move them).
//
// Ten of the twelve read `trades` directly and are mutually independent,
// so their relative order is free. `twap_1h` and `twap_1d` are the only
// HIERARCHICAL ones — `parent_mat_hypertable_id` points at prices_1m's
// materialisation — so prices_1m MUST be refreshed before them or they
// re-materialise from stale input. That is the one load-bearing edge,
// and it is why prices_1m leads and the twaps trail.
//
// Note the trap: prices_15m/1h/4h/1d/1w/1mo are NOT built on prices_1m
// (each reads `trades` itself), so the coarse ones do not inherit a
// prices_1m refresh — each needs its own call.
var xlmBaseRestampCAGGs = []timescale.CAGGSpec{
	// Must lead: twap_1h/twap_1d are materialised FROM this one.
	{Name: "prices_1m", MinWindow: 2 * time.Minute},
	{Name: "prices_15m", MinWindow: 30 * time.Minute},
	{Name: "prices_1h", MinWindow: 3 * time.Hour},
	{Name: "prices_4h", MinWindow: 12 * time.Hour},
	{Name: "prices_1d", MinWindow: 3 * 24 * time.Hour},
	{Name: "prices_1w", MinWindow: 3 * 7 * 24 * time.Hour},
	{Name: "prices_1mo", MinWindow: 93 * 24 * time.Hour},
	{Name: "dex_volume_by_pair_1d", MinWindow: 3 * 24 * time.Hour},
	{Name: "source_volume_1h", MinWindow: 3 * time.Hour},
	{Name: "pools_per_source_1h", MinWindow: 3 * time.Hour},
	// Must trail prices_1m.
	{Name: "twap_1h", MinWindow: 3 * time.Hour},
	{Name: "twap_1d", MinWindow: 3 * 24 * time.Hour},
}

// xlmBaseRestampFollowUp renders the operator's post-write follow-up: the
// ordered CAGG refresh block, and the `-min-rel-delta` guidance. Returned
// as a string rather than printed so the ordering contract is testable
// without capturing stdout.
//
// The window is [from 00:00Z, to+1d 00:00Z), padded per aggregate by
// [timescale.PadRefreshWindow] so each call clears Timescale's
// 2-bucket minimum. Padded buckets outside the restamp window
// re-materialise unchanged, which is cheap; a call that is REJECTED for a
// too-small window is the expensive outcome, because the operator reads
// the error, skips that aggregate, and ships a partially-stale surface.
func xlmBaseRestampFollowUp(from, to time.Time) string {
	lo := from.UTC().Truncate(24 * time.Hour)
	hi := to.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)

	var b strings.Builder
	b.WriteString("\nSTILL STALE AFTER A -write RUN — the acceptance check below CANNOT see this.\n")
	b.WriteString("verify-usd-volume reads `trades` directly; every served volume surface reads a\n")
	b.WriteString("continuous aggregate, and none auto-refresh further back than 7 days (prices_1m:\n")
	b.WriteString("5 minutes). Until these run, /v1/markets volume, asset volume, venue rankings and\n")
	b.WriteString("every chart keep serving the PRE-restamp numbers. Order is load-bearing:\n")
	b.WriteString("twap_1h/twap_1d are built ON prices_1m, so prices_1m goes first.\n\n")
	for _, c := range xlmBaseRestampCAGGs {
		pf, pt := timescale.PadRefreshWindow(lo, hi, c.MinWindow)
		fmt.Fprintf(&b, "  CALL refresh_continuous_aggregate('%s', '%s', '%s');\n",
			c.Name, pf.Format(time.RFC3339), pt.Format(time.RFC3339))
	}
	b.WriteString("\nThen force the asset_volume_24h rollup (it re-sums prices_1m.volume_usd; it also\n")
	b.WriteString("self-heals on its own cadence, so verify rather than assume).\n")
	b.WriteString("\nFLAG GUIDANCE — `-min-rel-delta 0.001` is the recommended setting for a full-window\n")
	b.WriteString("run. The re-derive reads the FINALISED prices_1m bucket while the original insert\n")
	b.WriteString("read the partially-materialised real-time bucket for the same minute, so a large\n")
	b.WriteString("share of the write set moves by less than 0.1% — repairing nothing, on a\n")
	b.WriteString("COMPRESSED hypertable where the write is the expensive part. Measured over\n")
	b.WriteString("2026-01-01..2026-07-21: of 10,734,569 non-null-fill changes, 8,423,350 move >=0.1%,\n")
	b.WriteString("so the flag drops 2,311,219 rows (8.1% of the 28,583,186-row write set) at a cost\n")
	b.WriteString("of <0.1% each. It never suppresses a NULL fill, which is where the coverage\n")
	b.WriteString("recovery lives, so the tool's whole point survives the flag.\n\n")
	return b.String()
}

// xlmBaseRestampOptions is the already-resolved flag set the run needs.
// Kept as a struct so runXLMBaseRestamp's signature stays readable and
// so tests can build one without a flag.FlagSet.
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
	// xlmBaseWalkFull folds every slice into the report and, under
	// -write, applies its plan. The day walk and the inside of a
	// decompressed chunk.
	xlmBaseWalkFull xlmBaseWalkMode = iota
	// xlmBaseWalkProbe is the chunk walk's read-only pre-check: plan slice
	// by slice, fold NOTHING into the report, and stop at the first slice
	// that would change a row. A chunk whose rows all already hold the
	// anchor's value is probed to its end and then skipped without ever
	// being decompressed; a chunk that needs work is found out after its
	// first dirty slice.
	xlmBaseWalkProbe
)

// xlmBaseWalk is what one walk over [lo, hi) found: the merged window
// statistics and the rows the database actually changed.
type xlmBaseWalk struct {
	stats   timescale.XLMBaseRestampStats
	written int64
}

// xlmBaseApply is the write half of a walk. The day walk passes the
// store's own apply; the chunk walk passes the one that checks the chunk
// is still decompressed ahead of every batch.
type xlmBaseApply func(ctx context.Context, plan *timescale.XLMBaseRestampPlan, generation int64, batch int) (int64, error)

// walk plans (and, in full mode under -write, applies) [lo, hi) in
// -slice windows with the store's own apply.
func (r *xlmBaseRestampRun) walk(ctx context.Context, lo, hi time.Time, mode xlmBaseWalkMode) (xlmBaseWalk, error) {
	return r.walkWith(ctx, lo, hi, mode, r.store.ApplyXLMBaseUSDVolumeRestamp)
}

// walkWith is [xlmBaseRestampRun.walk] with the apply chosen by the
// caller. Nothing here knows whether [lo, hi) is a UTC day or a chunk's
// slice of the run window; that is the caller's business.
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
		plan, err := r.store.PlanXLMBaseUSDVolumeRestamp(ctx, timescale.XLMBaseRestampParams{
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

// observe folds a window's changed rows into the running extremes and the
// report sample. The rows themselves are then dropped — a whole-span run
// touches ~28M of them and none of it belongs in memory.
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

// printResumeHint tells the operator exactly how to continue an
// interrupted run. The tool is idempotent (a row already holding the
// anchor's value is not a candidate), so restarting at the day that
// failed re-checks it cheaply rather than re-writing it.
func (r *xlmBaseRestampRun) printResumeHint(cfgPath string, failed, to time.Time, opts xlmBaseRestampOptions) {
	var b strings.Builder
	fmt.Fprintf(&b, "\nRESUME: stellarindex-ops usd-volume-restamp -config %s -tier xlm-base -from %s -to %s",
		cfgPath, failed.Format(time.DateOnly), to.Format(time.DateOnly))
	b.WriteString(xlmBaseResumeFlags(opts))
	if opts.Batch != defaultRestampBatch {
		fmt.Fprintf(&b, " -batch %d", opts.Batch)
	}
	if opts.Write {
		b.WriteString(" -write")
	}
	b.WriteString("\n  (idempotent: days already restamped re-scan and report 0 changes)")
	fmt.Fprintln(os.Stderr, b.String())
}

// printReport writes the decision block. Always printed after a run —
// under -report it is the whole output, and after a -write run it is the
// record of what the run did.
func (r *xlmBaseRestampRun) printReport(from, to time.Time) {
	s := r.totals
	days := int(to.Sub(from).Hours()/24) + 1
	fmt.Printf("\n=== usd-volume-restamp REPORT — tier xlm-base — [%s, %s] (%d day(s)) ===\n",
		from.Format(time.DateOnly), to.Format(time.DateOnly), days)
	fmt.Printf("scanned (source=DEX, base=XLM form, derive_generation <= %d)   %d\n", r.maxGen, s.Scanned)
	fmt.Printf("  quote leg USD-pegged (EXACT tier, `-tier exact` owns these)  %d\n", s.QuotePegged)
	fmt.Printf("  not DEX / base not an XLM form                               %d\n", s.NotDEX)
	fmt.Printf("  unparseable amount or stored value                           %d\n", s.Unparseable)
	fmt.Printf("  anchor declined, stored NULL   (coverage NOT recoverable)    %d\n", s.AnchorDeclinedNull)
	fmt.Printf("  anchor declined, stored VALUE  (STAYS WRONG after this run)  %d\n", s.AnchorDeclinedStored)
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

// parseMinRelDelta turns the -min-rel-delta flag (a fraction, e.g. 0.01
// for 1%) into a *big.Rat. Empty or "0" means "write every row whose
// value differs at all", which is the default: the anchor's number is a
// re-derivation of the correct value, and leaving a row 0.5% wrong for
// tidiness is not a defensible posture on a money column. The knob exists
// so an operator can bound a first production run to the large moves.
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

// checkRestampLiveOverlap is the one-writer contract for a `trades`
// re-stamp, the direct analogue of projected-rebuild's
// [checkLiveCursorGuard].
//
// `trades.usd_volume` has exactly one live writer: the indexer's ingest
// path, walking forward at the `ledgerstream` cursor. A restamp is only
// ever legitimate BEHIND that cursor — over history the live tail has
// already passed and will never walk into again. If the tail is still
// below the top of the requested window (a resync, a stalled indexer, a
// cursor rewind), the two writers can land on the same row: the restamp's
// high derive_generation would win the ON CONFLICT guard, so the ingest
// path's own value would be silently discarded — or, in the other
// interleaving, the restamp would rewrite a row the tail is about to
// overwrite, reporting a correction that did not survive.
//
// haveLive=false (no ledgerstream cursor at all) is a conservative
// REFUSAL, exactly as it is for projected-rebuild: an indexer that has
// never run will walk this range.
//
// allowOverlap is the operator's explicit "I have verified the live
// indexer will not touch this range" override.
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

// resolveWindowTopLedger finds the highest on-chain ledger the restamp
// window can contain, for [checkRestampLiveOverlap].
//
// Ledger sequence is monotonic in `ts`, so the answer lives in the LAST
// day of the window — one chunk, one bounded aggregate. Days with no
// on-chain rows (an outage, an off-chain-only day) are walked backwards
// past, so an empty tail day does not make the guard unusable; a window
// with no on-chain rows anywhere returns (0, nil), which the guard treats
// as vacuously safe.
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
