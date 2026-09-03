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

// xlmBaseRestampRun carries one `-tier xlm-base` invocation's fixed
// inputs and its running totals across days.
type xlmBaseRestampRun struct {
	store *timescale.Store

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
func runXLMBaseRestamp(ctx context.Context, store *timescale.Store, cfgPath string, from, to time.Time, opts xlmBaseRestampOptions) error {
	run := &xlmBaseRestampRun{
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
	verb := "would restamp"
	if opts.Write {
		verb = "restamped"
	}
	fmt.Printf("\nusd-volume-restamp: %s %d row(s) in [%s, %s] (tier xlm-base)\n",
		verb, run.totals.Changed, from.Format(time.DateOnly), to.Format(time.DateOnly))
	if opts.Write && run.written != run.totals.Changed {
		// The write set IS the plan, so these can only diverge when a
		// concurrent writer moved a row past the generation guard between
		// the plan and the UPDATE. That is a one-writer-contract
		// violation, not a rounding difference — say so loudly.
		fmt.Printf("WARNING: %d row(s) planned but %d row(s) changed — a concurrent writer moved rows past the derive_generation guard\n",
			run.totals.Changed, run.written)
	}
	fmt.Printf("acceptance: stellarindex-ops verify-usd-volume -config %s -day %s -days %d\n",
		cfgPath, to.Format(time.DateOnly), int(to.Sub(from).Hours()/24)+1)
	return nil
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
	var (
		dayStats = timescale.NewXLMBaseRestampStats()
		written  int64
	)
	end := day.AddDate(0, 0, 1)
	for lo := day; lo.Before(end); lo = lo.Add(r.slice) {
		if err := ctx.Err(); err != nil {
			return err
		}
		hi := lo.Add(r.slice)
		if hi.After(end) {
			hi = end
		}
		plan, err := r.store.PlanXLMBaseUSDVolumeRestamp(ctx, timescale.XLMBaseRestampParams{
			From: lo, To: hi, Sources: r.allow, FillNull: r.fillNull,
			MaxGeneration: r.maxGen, MinRelDelta: r.minRelDelta,
		})
		if err != nil {
			return err
		}
		r.observe(plan)
		dayStats.Merge(plan.Stats)
		if r.write && len(plan.Rows) > 0 {
			n, aerr := r.store.ApplyXLMBaseUSDVolumeRestamp(ctx, plan, r.generation, r.batch)
			written += n
			r.written += n
			if aerr != nil {
				return aerr
			}
		}
		r.progress += uint64(plan.Stats.Changed)     //nolint:gosec // a non-negative row count
		r.hb.Progress(r.progress, uint64(lo.Unix())) //nolint:gosec // post-1970 timestamp
		if plan.Stats.Changed > 0 {
			fmt.Printf("  %s %s..%s  %d change(s) (%d null-fill, %d anchor-declined)\n",
				day.Format(time.DateOnly), lo.Format("15:04"), hi.Format("15:04"),
				plan.Stats.Changed, plan.Stats.NullFilled,
				plan.Stats.AnchorDeclinedNull+plan.Stats.AnchorDeclinedStored)
		}
	}
	r.totals.Merge(dayStats)
	verb := "would change"
	if r.write {
		verb = fmt.Sprintf("changed %d, planned", written)
	}
	fmt.Printf("=== %s: scanned %d, %s %d (null-fill %d, anchor-declined %d/%d null/valued, already correct %d)\n",
		day.Format(time.DateOnly), dayStats.Scanned, verb, dayStats.Changed,
		dayStats.NullFilled, dayStats.AnchorDeclinedNull, dayStats.AnchorDeclinedStored,
		dayStats.Unchanged)
	return nil
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
	if opts.FillNull {
		b.WriteString(" -fill-null")
	}
	if opts.Slice != time.Hour {
		fmt.Fprintf(&b, " -slice %s", opts.Slice)
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
