package ingest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// resume-stalled resumes stalled backfill cursors (sub_source "<from>-<to>:<decoders>") over [last_ledger+1, to] via runBackfillChunk
// with -resume semantics, sequentially; per-cursor failures are logged and skipped (non-zero exit if any errored).
const stalledCursorSubPattern = `^(\d+)-(\d+):(.+)$`

// dataGapGateTimeout bounds the data-derived gap-gate queries; rootCtx has no deadline (see [newDataGapGateContext]).
const dataGapGateTimeout = 10 * time.Minute

var stalledCursorSubRE = regexp.MustCompile(stalledCursorSubPattern)

// newDataGapGateContext bounds the data-gap gate queries: the gap scans are full-table reads and
// rootCtx only cancels on a signal, so without a deadline an unattended run could wedge.
func newDataGapGateContext(rootCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(rootCtx, dataGapGateTimeout)
}

// stalledCursorPlan is one cursor's remaining work, produced by parseStalledCursor and filtered by planResumeStalled before any
// runBackfillChunk.
type stalledCursorPlan struct {
	cursor     timescale.Cursor
	origFrom   uint32   // declared `from` parsed from sub_source (m[1]) — the ORIGINAL cursor key's start, distinct from rangeFrom
	rangeFrom  uint32   // last_ledger + 1 — the actual remaining-work start
	rangeTo    uint32   // parsed `to` from sub_source
	sources    []string // decoder CSV, sorted
	skip       bool
	skipReason string
	// skipRawSorobanEvents marks a raw soroban-events cursor skipped by
	// gateSourcePolicy, so it is logged and counted apart from other skips.
	skipRawSorobanEvents bool
}

// sorobanDecoderNames: a stalled cursor naming any of these is gated against its own decoder tables (decoderGapIndex); SDEX is
// deliberately absent (classicGapGate, retention-scoped, ADR-0034). Derived from pipeline.SorobanSourceNames; never hand-maintain it.
var sorobanDecoderNames = func() map[string]struct{} {
	m := make(map[string]struct{}, len(pipeline.SorobanSourceNames)+1)
	for _, name := range pipeline.SorobanSourceNames {
		m[name] = struct{}{}
	}
	m[SorobanEventsPseudoSource] = struct{}{}
	return m
}()

// planHasSorobanDecoder reports whether any plan decoder is Soroban-era. Mixed plans count, but a clean
// Soroban side alone never skips them; see gateMixedPlan.
func planHasSorobanDecoder(sources []string) bool {
	for _, s := range sources {
		if _, ok := sorobanDecoderNames[s]; ok {
			return true
		}
	}
	return false
}

// classicGapGate carries the data-derived gate inputs for classic (SDEX-only) plans, built lazily
// because the sdex gap scan is the heaviest. available=false falls back to the conservative blanket skip.
type classicGapGate struct {
	available bool
	// floor is the oldest served sdex ledger (ADR-0034 retention boundary); scans below it would report
	// retention artifacts as gaps, so scan and gate are clamped to [floor, tip].
	floor uint32
	gaps  []timescale.LedgerGap
}

// sdexGapTarget returns the gap detector's registered sdex/trades target so the resume gate uses the same scan definition as the 6h
// cycle.
func sdexGapTarget() (timescale.GapDetectorTarget, bool) {
	for _, t := range timescale.DefaultGapDetectorTargets {
		if t.Source == "sdex" && t.Table == "trades" {
			return t, true
		}
	}
	return timescale.GapDetectorTarget{}, false
}

// buildClassicGapGate runs the SDEX gap scan over [floor, tip], with floor at the actual retained
// boundary (as compute-completeness does), since census-vs-served below it is a retention artifact.
func buildClassicGapGate(ctx context.Context, store *timescale.Store, tip uint32, minGapSize int64) (classicGapGate, error) {
	target, ok := sdexGapTarget()
	if !ok || tip == 0 {
		return classicGapGate{}, nil
	}
	floor, ok, err := store.MinLedger(ctx, target.Table, target.LedgerColumn, target.WhereFilter, uint32(target.Genesis), tip) //nolint:gosec // sdex genesis is 2
	if err != nil {
		return classicGapGate{}, err
	}
	if !ok {
		return classicGapGate{}, nil // no served sdex rows at all — can't gate
	}
	gaps, err := store.FindPerSourceLedgerGaps(ctx, target, int64(floor), int64(tip), minGapSize, 0)
	if err != nil {
		return classicGapGate{}, err
	}
	return classicGapGate{available: true, floor: floor, gaps: gaps}, nil
}

// hasNonSorobanDecoder reports whether the plan has an SDEX portion needing the sdex data-derived gate (classic-only or mixed).
func hasNonSorobanDecoder(sources []string) bool {
	for _, s := range sources {
		if _, ok := sorobanDecoderNames[s]; !ok {
			return true
		}
	}
	return false
}

// anyPlanNeedsClassicGate reports whether any live plan has an SDEX portion (classic-only or mixed), i.e. the sdex gap scan is needed.
func anyPlanNeedsClassicGate(plans []stalledCursorPlan) bool {
	for _, p := range plans {
		if !p.skip && hasNonSorobanDecoder(p.sources) {
			return true
		}
	}
	return false
}

// decoderGapResult is one Soroban decoder's gap evidence. resolved=false (no target) means "cannot confirm clean", never "clean": only
// a decoder's OWN table is honest, since soroban_events is written by live ingest regardless of backfilled decoders.
type decoderGapResult struct {
	resolved bool
	gaps     []timescale.LedgerGap
}

// decoderGapIndex maps a decoder name to its gap evidence, built once per run by [buildDecoderGapIndex] so the gate stays pure.
type decoderGapIndex map[string]decoderGapResult

// perDecoderGapTargets returns every GapDetectorTarget whose SourceNetKey matches decoder; a decoder can
// own several tables and is clean only when ALL are.
func perDecoderGapTargets(decoder string) []timescale.GapDetectorTarget {
	var out []timescale.GapDetectorTarget
	for _, t := range timescale.DefaultGapDetectorTargets {
		if t.SourceNetKey() == decoder {
			out = append(out, t)
		}
	}
	return out
}

// distinctSorobanDecoderNames returns the sorted, de-duplicated real Soroban decoder names in any not-yet-skipped plan, for
// [buildDecoderGapIndex].
func distinctSorobanDecoderNames(plans []stalledCursorPlan) []string {
	seen := make(map[string]struct{})
	for _, p := range plans {
		if p.skip {
			continue
		}
		for _, s := range p.sources {
			if s == SorobanEventsPseudoSource {
				continue
			}
			if _, ok := sorobanDecoderNames[s]; ok {
				seen[s] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// buildDecoderGapIndex resolves per-decoder gap evidence: soroban_events uses sorobanEventsGaps, every other decoder the UNION of its
// OWN tables, never soroban_events. No registered target gives resolved=false and the gate fails closed.
func buildDecoderGapIndex(ctx context.Context, store *timescale.Store, plans []stalledCursorPlan, sorobanEventsGaps []timescale.LedgerGap, tip uint32, minGapSize int64) (decoderGapIndex, error) {
	idx := decoderGapIndex{
		SorobanEventsPseudoSource: {resolved: true, gaps: sorobanEventsGaps},
	}
	for _, name := range distinctSorobanDecoderNames(plans) {
		targets := perDecoderGapTargets(name)
		if len(targets) == 0 {
			idx[name] = decoderGapResult{resolved: false}
			continue
		}
		var union []timescale.LedgerGap
		for _, target := range targets {
			if int64(tip) < target.Genesis {
				continue // not yet live on this deployment — no rows possible, no gap
			}
			gaps, err := store.FindPerSourceLedgerGaps(ctx, target, target.Genesis, int64(tip), minGapSize, 0)
			if err != nil {
				return nil, fmt.Errorf("per-decoder data gap %s/%s: %w", name, target.Table, err)
			}
			union = append(union, gaps...)
		}
		idx[name] = decoderGapResult{resolved: true, gaps: union}
	}
	return idx, nil
}

// decoderPortionHasGap reports whether ANY source shows a gap overlapping [from,to] or is unresolved;
// unresolved counts as a gap so the caller fails closed.
func decoderPortionHasGap(sources []string, from, to uint32, idx decoderGapIndex) bool {
	for _, s := range sources {
		res, ok := idx[s]
		if !ok || !res.resolved {
			return true
		}
		if overlapsAnyDataGap(from, to, res.gaps) {
			return true
		}
	}
	return false
}

// sorobanSourcesOf filters a mixed plan's decoder CSV to its Soroban-era portion (excludes SDEX), for [gateMixedPlan].
func sorobanSourcesOf(sources []string) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		if _, ok := sorobanDecoderNames[s]; ok {
			out = append(out, s)
		}
	}
	return out
}

// gateAgainstDataGaps narrows plans to those overlapping a real data gap: Soroban plans use their own decoder tables, SDEX-only plans
// the retention-scoped sdex scan, mixed plans both (gateMixedPlan).
func gateAgainstDataGaps(plans []stalledCursorPlan, decoderGaps decoderGapIndex, classic classicGapGate, forceClassic bool) []stalledCursorPlan {
	out := make([]stalledCursorPlan, len(plans))
	copy(out, plans)
	for i := range out {
		if out[i].skip {
			continue
		}
		switch {
		case !planHasSorobanDecoder(out[i].sources):
			gateClassicPlan(&out[i], classic, forceClassic)
		case hasNonSorobanDecoder(out[i].sources):
			gateMixedPlan(&out[i], decoderGaps, classic, forceClassic)
		default:
			if !decoderPortionHasGap(out[i].sources, out[i].rangeFrom, out[i].rangeTo, decoderGaps) {
				out[i].skip = true
				out[i].skipReason = "remaining range fully covered by sibling cursors (no per-decoder data gap overlap) — cursor inventory false-positive"
			}
		}
	}
	return out
}

// gateMixedPlan handles plans with both Soroban and SDEX decoders. A real or unresolved gap on EITHER
// side keeps the plan actionable; it is skipped only when both are confirmed clean or --force-classic-cursors is set.
func gateMixedPlan(p *stalledCursorPlan, decoderGaps decoderGapIndex, classic classicGapGate, forceClassic bool) {
	if decoderPortionHasGap(sorobanSourcesOf(p.sources), p.rangeFrom, p.rangeTo, decoderGaps) {
		return // soroban side alone already justifies the resume
	}
	// Soroban side is clean. Do NOT conclude false-positive without
	// also running the SAME data-derived gate a classic-only plan's
	// SDEX portion gets.
	gateClassicPlan(p, classic, forceClassic)
}

// gateClassicPlan applies the SDEX data-derived gate to one classic-only plan. Order matters: the no-gap check runs before the
// below-floor-start check so a gapless plan gets the accurate "false-positive" reason.
func gateClassicPlan(p *stalledCursorPlan, classic classicGapGate, forceClassic bool) {
	switch {
	case forceClassic:
		// operator opt-in: trust cursor inventory for SDEX
	case !classic.available:
		p.skip = true
		p.skipReason = "sdex data-gap gate unavailable (no served sdex rows, or the sdex gap-detector target is missing). Pass --force-classic-cursors to act on cursor inventory alone."
	case p.rangeTo < classic.floor:
		p.skip = true
		p.skipReason = fmt.Sprintf("remaining range entirely below the served-tier trades retention floor (%d) — full history lives in the CH lake (ADR-0034); re-derive via ch-rebuild, not backfill", classic.floor)
	case !overlapsAnyDataGap(max(p.rangeFrom, classic.floor), p.rangeTo, classic.gaps):
		p.skip = true
		p.skipReason = "remaining range has no sdex data gap in trades within the retained window — cursor inventory false-positive"
	case p.rangeFrom < classic.floor:
		p.skip = true
		p.skipReason = fmt.Sprintf("overlaps an sdex data gap but starts below the retention floor (%d) — a resume would re-walk retention-dropped ledgers; review, then pass --force-classic-cursors if the cursor inventory is right", classic.floor)
	}
}

// overlapsAnyDataGap reports whether [from, to] intersects any gap; a linear scan, as gap slices are tiny.
func overlapsAnyDataGap(from, to uint32, gaps []timescale.LedgerGap) bool {
	planFrom := int64(from)
	planTo := int64(to)
	for _, g := range gaps {
		if g.End < planFrom || g.Start > planTo {
			continue
		}
		return true
	}
	return false
}

// applyMaxResumesCap truncates the actionable subset to maxResumes (<=0 means no cap), marking the
// excess skipped. MUST run AFTER gateAgainstDataGaps, or the cap could be spent on cursors the gate would skip.
func applyMaxResumesCap(plans []stalledCursorPlan, maxResumes int) []stalledCursorPlan {
	if maxResumes <= 0 {
		return plans
	}
	out := make([]stalledCursorPlan, len(plans))
	copy(out, plans)
	kept := 0
	for i := range out {
		if out[i].skip {
			continue
		}
		if kept >= maxResumes {
			out[i].skip = true
			out[i].skipReason = fmt.Sprintf("max-resumes cap (%d) reached — genuinely actionable, deferred to a future run", maxResumes)
			continue
		}
		kept++
	}
	return out
}

// decoderPortion returns the decoder-CSV part of a sub_source (after the first ':'), or the whole
// string when malformed; parseStalledCursor rejects those on its own.
func decoderPortion(sub string) string {
	if i := strings.IndexByte(sub, ':'); i >= 0 {
		return sub[i+1:]
	}
	return sub
}

// matchesSourceFilter matches -source-filter ONLY against the decoder-CSV portion, never the numeric
// <from>-<to> prefix, which could match unrelated cursors by digits.
func matchesSourceFilter(sub, filter string) bool {
	if filter == "" {
		return true
	}
	return strings.Contains(decoderPortion(sub), filter)
}

// parseStalledCursor extracts from/to/decoders from a sub_source and computes the remaining range; the
// plan is skip=true with a skipReason when not actionable.
func parseStalledCursor(c timescale.Cursor) stalledCursorPlan {
	p := stalledCursorPlan{cursor: c}
	m := stalledCursorSubRE.FindStringSubmatch(c.Sub)
	if m == nil {
		p.skip = true
		p.skipReason = "sub_source doesn't match `<from>-<to>:<decoders>` shape"
		return p
	}
	fromN, err := strconv.ParseUint(m[1], 10, 32)
	if err != nil {
		p.skip = true
		p.skipReason = fmt.Sprintf("parse from: %v", err)
		return p
	}
	toN, err := strconv.ParseUint(m[2], 10, 32)
	if err != nil {
		p.skip = true
		p.skipReason = fmt.Sprintf("parse to: %v", err)
		return p
	}
	if c.LastLedger >= uint32(toN) {
		p.skip = true
		p.skipReason = fmt.Sprintf("last_ledger %d already at-or-past target %d (stale-by-time, not by-position)", c.LastLedger, toN)
		return p
	}
	if uint32(fromN) > c.LastLedger+1 {
		// The cursor started later than its sub_source declares.
		// Shouldn't happen in well-formed cursors but worth flagging
		// rather than computing a negative-or-rewinding range.
		p.skip = true
		p.skipReason = fmt.Sprintf("declared from %d > last_ledger+1 %d — cursor inconsistent", fromN, c.LastLedger+1)
		return p
	}
	srcs := opsutil.SplitCSV(m[3])
	if len(srcs) == 0 {
		p.skip = true
		p.skipReason = "empty decoder set"
		return p
	}
	sort.Strings(srcs)
	p.origFrom = uint32(fromN)
	p.rangeFrom = c.LastLedger + 1
	p.rangeTo = uint32(toN)
	p.sources = srcs
	return p
}

// resumeStalled is the subcommand entry point.
type resumeStalledOpts struct {
	cfgPath        string
	minLag         time.Duration
	maxResumes     int
	sourceFilter   string
	bucket         string
	dryRun         bool
	parallel       int
	refreshCAGGs   bool
	forceClassic   bool
	dataGapMinSize int64
}

func parseResumeStalledFlags(args []string) (resumeStalledOpts, config.Config, error) {
	var opts resumeStalledOpts
	var cfg config.Config
	fs := flag.NewFlagSet("resume-stalled", flag.ContinueOnError)
	fs.StringVar(&opts.cfgPath, "config", "", "path to stellarindex.toml (required)")
	fs.DurationVar(&opts.minLag, "min-lag", time.Hour,
		"only resume cursors stalled longer than this (lag = now - last_updated)")
	fs.IntVar(&opts.maxResumes, "max-resumes", 0,
		"cap on cursors resumed in this run (0 = no cap)")
	fs.StringVar(&opts.sourceFilter, "source-filter", "",
		"only resume cursors whose decoder CSV contains this source name")
	bucketOverride := fs.String("bucket", "",
		"galexie bucket override; default = cfg.Storage.S3BucketArchive")
	gate := opsutil.RegisterWriteGate(fs)
	fs.IntVar(&opts.parallel, "parallel", 1,
		"per-cursor backfill parallelism (default 1 = sequential)")
	fs.BoolVar(&opts.refreshCAGGs, "refresh-caggs", true,
		"forward to runBackfillChunk's CAGG-refresh path")
	fs.BoolVar(&opts.forceClassic, "force-classic-cursors", false,
		"act on SDEX-only stalled cursors using cursor-inventory alone, "+
			"bypassing the data-derived gap gate (incl. the retention-floor guard)")
	fs.Int64Var(&opts.dataGapMinSize, "data-gap-min-size", int64(timescale.GapDetectorMinGapSize),
		"minimum contiguous gap size for both data-gap gates "+
			"(soroban_events for Soroban plans; trades[source='sdex'] for SDEX-only plans)")
	if err := fs.Parse(args); err != nil {
		return opts, cfg, err
	}
	if opts.cfgPath == "" {
		return opts, cfg, errors.New("-config required")
	}
	if opts.minLag < 0 {
		return opts, cfg, fmt.Errorf("-min-lag (%s) must be >= 0", opts.minLag)
	}
	if opts.parallel < 1 {
		return opts, cfg, fmt.Errorf("-parallel (%d) must be >= 1", opts.parallel)
	}
	gate.Banner()
	opts.dryRun = gate.DryRun()
	loaded, err := config.LoadWithEnv(opts.cfgPath)
	if err != nil {
		return opts, cfg, fmt.Errorf("load config: %w", err)
	}
	cfg = loaded
	opts.bucket = cfg.Storage.S3BucketArchive
	if *bucketOverride != "" {
		opts.bucket = *bucketOverride
	}
	if opts.bucket == "" {
		return opts, cfg, errors.New("no bucket — set -bucket or cfg.Storage.S3BucketArchive")
	}
	return opts, cfg, nil
}

// ArmTradeWriteStore arms a store for a trade-writing re-derive: a positive derive generation (so it wins the ON CONFLICT guard) AND the USD-volume resolvers, which are inseparable (generation alone would overwrite stored values with NULL usd_volume); route all such ops entry points through it.
func ArmTradeWriteStore(store *timescale.Store, cfg config.Config) error {
	store.SetDeriveGeneration(time.Now().Unix())
	if err := timescale.InstallUSDVolumeResolution(
		store,
		cfg.Trades.USDPeggedClassicAssets,
		cfg.Supply.SACWrappers,
		cfg.PricingGuard.FiatPeggedClassicAssets,
	); err != nil {
		return fmt.Errorf("install usd-volume resolution: %w", err)
	}
	return nil
}

func resumeStalled(args []string) error {
	opts, cfg, err := parseResumeStalledFlags(args)
	if err != nil {
		return err
	}

	rootCtx, cancel := opsutil.SignalContext()
	defer cancel()

	logger := opsutil.MkBackfillLogger()

	store, err := timescale.Open(rootCtx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	// resume-stalled uses the same runBackfillChunk trade-write path as backfill, so it MUST arm the store
	// via ArmTradeWriteStore or it would write DEX usd_volume=NULL at gen 0 and leave reDeriveNullVolumeGuard inert.
	if err := ArmTradeWriteStore(store, cfg); err != nil {
		return err
	}

	// maxResumes is NOT passed to planResumeStalled: the cap must apply after gateAgainstDataGaps, or it
	// could be spent on cursors the gate would skip while genuinely actionable ones were never gathered.
	plans, err := planResumeStalled(rootCtx, store, opts.minLag, opts.sourceFilter)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	plans = gateSourcePolicy(plans, cfg)

	// Gate Soroban-era plans against their own decoder tables, with the tip resolved as find-data-gaps does;
	// cursors fully covered by siblings are skipped as false positives.
	tipCursor, err := store.GetCursor(rootCtx, "ledgerstream", "")
	if err != nil {
		return fmt.Errorf("resolve tip for data-gap gate: %w", err)
	}
	gateCtx, gateCancel := newDataGapGateContext(rootCtx)
	defer gateCancel()
	dataGaps, err := rawSorobanEventsGaps(gateCtx, store, plans, tipCursor.LastLedger, opts.dataGapMinSize)
	if err != nil {
		return err
	}
	decoderGaps, err := buildDecoderGapIndex(gateCtx, store, plans, dataGaps, tipCursor.LastLedger, opts.dataGapMinSize)
	if err != nil {
		return fmt.Errorf("find per-decoder data gaps for gate: %w", err)
	}
	// SDEX-only plans get their own gate; the scan is the heaviest, so build it only when needed and not --force-classic-cursors.
	var classicGate classicGapGate
	if !opts.forceClassic && anyPlanNeedsClassicGate(plans) {
		classicGate, err = buildClassicGapGate(gateCtx, store, tipCursor.LastLedger, opts.dataGapMinSize)
		if err != nil {
			return fmt.Errorf("sdex data-gap gate: %w", err)
		}
	}
	plans = gateAgainstDataGaps(plans, decoderGaps, classicGate, opts.forceClassic)
	plans = applyMaxResumesCap(plans, opts.maxResumes)

	actionable, skippedRaw := 0, 0
	for _, p := range plans {
		switch {
		case !p.skip:
			actionable++
		case p.skipRawSorobanEvents:
			skippedRaw++
		}
	}
	logger.Info("resume-stalled plan",
		"candidates", len(plans),
		"actionable", actionable,
		"skipped", len(plans)-actionable,
		"skipped_soroban_events", skippedRaw,
		"data_gaps", len(dataGaps),
		"min_lag", opts.minLag.String(),
		"source_filter", opts.sourceFilter,
		"force_classic_cursors", opts.forceClassic,
		"dry_run", opts.dryRun,
	)

	if opts.dryRun || actionable == 0 {
		for _, p := range plans {
			printResumePlan(p)
		}
		return nil
	}

	failures := executeResumePlans(rootCtx, logger, plans, opts.cfgPath, opts.bucket, opts.parallel, opts.refreshCAGGs, cfg, store)
	if len(failures) > 0 {
		return fmt.Errorf("resume-stalled: %d of %d cursors failed: %w",
			len(failures), actionable, errors.Join(failures...))
	}
	logger.Info("resume-stalled: all cursors complete",
		"cursors", actionable,
	)
	return nil
}

// executeResumePlans runs each planned cursor via runResumeForCursor, collecting per-cursor failures and continuing.
func executeResumePlans(
	ctx context.Context,
	logger *slog.Logger,
	plans []stalledCursorPlan,
	cfgPath string,
	bucket string,
	parallel int,
	refreshCAGGs bool,
	cfg config.Config,
	store *timescale.Store,
) []error {
	var failures []error
	for _, p := range plans {
		if p.skip {
			lvl := slog.LevelInfo
			if p.skipRawSorobanEvents {
				lvl = slog.LevelWarn
			}
			logger.Log(ctx, lvl, "resume-stalled: skipping cursor",
				"sub_source", p.cursor.Sub,
				"last_ledger", p.cursor.LastLedger,
				"reason", p.skipReason,
			)
			continue
		}
		if err := runOneCursorPlan(ctx, logger, p, cfgPath, bucket, parallel, refreshCAGGs, cfg, store); err != nil {
			failures = append(failures, fmt.Errorf("cursor %q: %w", p.cursor.Sub, err))
		}
	}
	return failures
}

// resumeChunkFrom picks the range base for planBackfillChunks. At parallel<=1 it MUST be p.origFrom: backfillCursorSub keys the cursor
// row, so rangeFrom would create a SIBLING row and the stalled one never advance. At parallel>1 sub-chunks need own rows.
func resumeChunkFrom(p stalledCursorPlan, parallel int) uint32 {
	if parallel <= 1 {
		return p.origFrom
	}
	return p.rangeFrom
}

// runOneCursorPlan wires one stalledCursorPlan through the regular backfill chunk path, driven from the cursor row instead of CLI
// flags.
func runOneCursorPlan(
	ctx context.Context,
	logger *slog.Logger,
	p stalledCursorPlan,
	cfgPath string,
	bucket string,
	parallel int,
	refreshCAGGs bool,
	cfg config.Config,
	store *timescale.Store,
) error {
	chunkFrom := resumeChunkFrom(p, parallel)
	opts := backfillOpts{
		cfgPath:      cfgPath,
		from:         chunkFrom,
		to:           p.rangeTo,
		sources:      p.sources,
		bucket:       bucket,
		resume:       true, // monotonic-advance on the existing cursor row
		parallel:     parallel,
		refreshCAGGs: refreshCAGGs,
		wasmGate:     &replayGateOnce{},
	}
	chunks := planBackfillChunks(opts.from, opts.to, opts.parallel)
	cursorLogger := logger.With(
		"cursor_sub_source", p.cursor.Sub,
		"cursor_last_ledger", p.cursor.LastLedger,
		"resume_from", p.rangeFrom,
		"resume_to", p.rangeTo,
		"sources", p.sources,
	)
	cursorLogger.Info("resume-stalled: cursor start", "chunks", len(chunks))
	if err := runResumeForCursor(ctx, cursorLogger, opts, cfg, store, chunks); err != nil {
		cursorLogger.Error("resume-stalled: cursor failed", "err", err)
		return err
	}
	cursorLogger.Info("resume-stalled: cursor complete")
	return nil
}

// runResumeForCursor runs one cursor's remaining range through the same guarded fan-out as `backfill`; a single chunk is guarded too,
// so a panic fails that cursor, not every cursor after it.
func runResumeForCursor(
	ctx context.Context,
	logger *slog.Logger,
	opts backfillOpts,
	cfg config.Config,
	store *timescale.Store,
	chunks []chunkRange,
) error {
	return runChunksGuarded(logger, "resume-chunk", chunks, func(chunkLogger *slog.Logger, c chunkRange) error {
		return runBackfillChunk(ctx, chunkLogger, opts, cfg, store, c)
	})
}

// gateSourcePolicy skips plans `backfill` itself would refuse (withdrawn attestation, projector-owned source), and with
// ClickHouseProjectorSource on also raw soroban-events cursors, since projection reads the lake and a Postgres hole is no gap.
func gateSourcePolicy(plans []stalledCursorPlan, cfg config.Config) []stalledCursorPlan {
	out := make([]stalledCursorPlan, len(plans))
	copy(out, plans)
	for i := range out {
		if out[i].skip {
			continue
		}
		if cfg.Storage.ClickHouseProjectorSource && hasSorobanEventsPseudo(out[i].sources) {
			out[i].skip = true
			out[i].skipRawSorobanEvents = true
			out[i].skipReason = "raw soroban-events landing-zone cursor — projection reads the ClickHouse lake, so resume-stalled does not re-walk it; run `backfill -source soroban-events` by hand if the table is still needed"
			continue
		}
		if err := checkBackfillSourcePolicy(out[i].sources, cfg, out[i].rangeFrom, out[i].rangeTo); err != nil {
			out[i].skip = true
			out[i].skipReason = err.Error()
		}
	}
	return out
}

// planResumeStalled is the side-effect-free read side shared by dry-run and apply. It gathers EVERY
// candidate; applyMaxResumesCap runs after gateAgainstDataGaps.
func planResumeStalled(
	ctx context.Context,
	store *timescale.Store,
	minLag time.Duration,
	sourceFilter string,
) ([]stalledCursorPlan, error) {
	rows, err := store.ListCursors(ctx)
	if err != nil {
		return nil, fmt.Errorf("list cursors: %w", err)
	}
	now := time.Now().UTC()
	var plans []stalledCursorPlan
	for _, c := range rows {
		if !strings.HasPrefix(c.Source, "backfill") {
			continue
		}
		lag := now.Sub(c.UpdatedAt)
		if lag < minLag {
			continue
		}
		if !matchesSourceFilter(c.Sub, sourceFilter) {
			continue
		}
		plans = append(plans, parseStalledCursor(c))
	}
	return plans, nil
}

// rawSorobanEventsGaps runs the soroban_events gap scan only when a live
// plan is a raw soroban-events cursor; no other plan reads its result.
func rawSorobanEventsGaps(ctx context.Context, store *timescale.Store, plans []stalledCursorPlan, tip uint32, minGapSize int64) ([]timescale.LedgerGap, error) {
	if !anyPlanHasSorobanEventsPseudo(plans) {
		return nil, nil
	}
	gaps, err := store.FindSorobanEventsLedgerGaps(ctx, 0, int64(tip), minGapSize)
	if err != nil {
		return nil, fmt.Errorf("find data gaps for gate: %w", err)
	}
	return gaps, nil
}

// anyPlanHasSorobanEventsPseudo reports whether a not-yet-skipped plan is
// a raw soroban-events cursor, i.e. needs the soroban_events gap scan.
func anyPlanHasSorobanEventsPseudo(plans []stalledCursorPlan) bool {
	for _, p := range plans {
		if !p.skip && hasSorobanEventsPseudo(p.sources) {
			return true
		}
	}
	return false
}

// printResumePlan emits one human-readable line per cursor saying what the apply pass would do or why it skips (dry-run path).
func printResumePlan(p stalledCursorPlan) {
	if p.skip {
		fmt.Fprintf(os.Stderr, "  SKIP  %s  reason=%s\n",
			opsutil.Truncate(p.cursor.Sub, 65), p.skipReason)
		return
	}
	fmt.Fprintf(os.Stderr, "  PLAN  %s  resume=[%d, %d] (%d ledgers) sources=%v\n",
		opsutil.Truncate(p.cursor.Sub, 65), p.rangeFrom, p.rangeTo,
		p.rangeTo-p.rangeFrom+1, p.sources)
}
