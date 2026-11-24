package chops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"
	"golang.org/x/sync/errgroup"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// runCHBackfill walks a bounded ledger range from galexie into ClickHouse (ADR-0034), idempotent via ReplacingMergeTree.
// -parallel N splits the range into N contiguous chunks, each with its own goroutine and Sink.
func chBackfill(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("ch-backfill")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required)")
	bucket := fs.String("bucket", "", "galexie bucket override. Default: the range vs ingestion.live_seam_ledger picks archive-or-live; with no seam configured it stays cfg.Storage.S3BucketLive, which does NOT hold historic ranges — pass the archive bucket for those (see opsutil.ResolveStreamBucket)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	flushEvery := fs.Int("flush-every", 500, "flush to ClickHouse every N ledgers (per worker)")
	parallel := fs.Int("parallel", 1, "number of concurrent range-walkers")
	changesOnly := fs.Bool("changes-only", false, "write only the ledgers row (the per-ledger commit marker) and ledger_entry_changes; skips txs/ops/results/participants/events/supply flows. For re-deriving entry-change ordinals without rewriting the other Tier-1 tables")
	heartbeat := fs.String("heartbeat", "", "node_exporter textfile path for the liveness/progress gauges (C6-020). Empty = "+opsutil.DefaultTextfileDir+"/ops_job_ch_backfill.prom when that directory exists (r1), otherwise no heartbeat at all")
	ledgersFile := fs.String("ledgers", "", "file of ledger sequences to re-derive instead of -from/-to: one per line, # comments, each >= 2, at most "+strconv.Itoa(maxTargetedLedgers)+" distinct. Only the listed ledgers are written; neighbours sharing a galexie object are decoded and skipped")
	listUpgrades := fs.Bool("list-upgrade-ledgers", false, "with -dry-run: print to stdout, in -ledgers file format, every walked ledger (-from/-to or -ledgers) whose close meta carries a ledger upgrade, each annotated with its upgrade entry-change count. Writes nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *parallel < 1 {
		*parallel = 1
	}
	plan, err := resolveBackfillPlan(*from, *to, *ledgersFile, *parallel)
	if err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if err := gate.RequireStatedMode(); err != nil {
		return fmt.Errorf("ch-backfill: %w", err)
	}
	// A dry run still fetches + decodes every ledger and opens no Sink: the
	// ADR-0043 §2.2 restore-drill mode, measuring RTO without adding rows.
	dryRun := gate.DryRun()
	if err := listModeAllowed(*listUpgrades, dryRun, *changesOnly); err != nil {
		return fmt.Errorf("ch-backfill: %w", err)
	}
	gate.Banner()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	streamBucket, berr := backfillBucket(cfg, *bucket, plan.lo, plan.hi)
	if berr != nil {
		return fmt.Errorf("ch-backfill: %w", berr)
	}
	lsCfg := backfillStreamConfig(cfg, streamBucket, *parallel)

	prog := newBackfillProgress(*heartbeat)
	defer prog.stop()
	if *listUpgrades {
		return runListUpgradeLedgers(ctx, lsCfg, plan, *parallel, streamBucket, prog)
	}

	worker := backfillWorker{
		stream: ledgerstream.Stream, lsCfg: lsCfg, passphrase: cfg.Stellar.Passphrase(),
		changesOnly: *changesOnly, plan: plan, loss: &extractLoss{}, logProgress: prog.record,
	}
	sinkDesc := "DRY RUN (decode only, nothing written)"
	if !dryRun {
		worker.openSink = clickhouseSinkOpener(*chAddr, *flushEvery)
		sinkDesc = fmt.Sprintf("ClickHouse %s", *chAddr)
		if *changesOnly {
			sinkDesc += " (ledgers + ledger_entry_changes only)"
		}
	}
	shares := assignRanges(plan.ranges, *parallel)
	fmt.Fprintf(os.Stderr, "ch-backfill: streaming %s from %q -> %s (%d worker(s))\n",
		plan.label(), streamBucket, sinkDesc, len(shares))
	if err := runBackfillWalk(ctx, worker, shares, streamBucket, prog, dryRun); err != nil {
		return err
	}
	if !dryRun && !*changesOnly {
		fmt.Fprintln(os.Stderr, censusFollowUp(ctx, *chAddr, plan.lo))
	}
	return nil
}

// backfillLedgerCloseTimes is clickhouse.LedgerCloseTimes, a var so the follow-up is testable without a live lake.
var backfillLedgerCloseTimes = clickhouse.LedgerCloseTimes

// censusFollowUp names the recompute a written range owes: contracts_census_daily re-replaces only its newest day, so
// contract events re-derived into an earlier day stay out of /v1/contracts until the days from the range's first are redone.
func censusFollowUp(ctx context.Context, chAddr string, lo uint32) string {
	day := fmt.Sprintf("<UTC day of ledger %d>", lo)
	if ts, err := backfillLedgerCloseTimes(ctx, chAddr, []uint32{lo}); err == nil {
		if t, ok := ts[lo]; ok {
			day = t.UTC().Format("2006-01-02")
		}
	}
	return fmt.Sprintf("ch-backfill: FOLLOW-UP OWED — stellar.contracts_census_daily does not hold contract events re-derived "+
		"from ledger %d on; run `run-heavy-job.sh census-rollup-redo stellarindex-ops ch-census-rollup -from-day %s -write`, "+
		"or /v1/contracts counts for those days stay short", lo, day)
}

// runBackfillWalk runs the workers and fails closed on a short walk or an extract undercount.
func runBackfillWalk(ctx context.Context, worker backfillWorker, shares [][]opsutil.RangeChunk, streamBucket string,
	prog *backfillProgress, dryRun bool,
) error {
	plan := worker.plan
	g, gctx := errgroup.WithContext(ctx)
	for i, share := range shares {
		g.Go(func() error { return worker.run(gctx, i, share) })
	}
	walkErr := g.Wait()

	total := prog.total()
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return fmt.Errorf("ch-backfill: stream (%d done): %w", total, walkErr)
	}
	prog.reportRate()
	// A walk that did not cover its range must NOT exit 0 — see
	// backfillCoverage.
	if cerr := plan.coverage(total, streamBucket, ctx.Err() != nil); cerr != nil {
		return fmt.Errorf("ch-backfill: %w", cerr)
	}
	loss := worker.loss
	if s := loss.supplySkipSummary(); s != "" {
		fmt.Fprintf(os.Stderr, "ch-backfill: %s\n", s)
	}
	if lerr := loss.err(); lerr != nil {
		return fmt.Errorf("ch-backfill: %w", lerr)
	}
	prog.markOK()
	if dryRun {
		fmt.Fprintf(os.Stderr, "ch-backfill: done — %s re-derived from %q (DRY RUN, nothing written)\n", plan.label(), streamBucket)
		return nil
	}
	fmt.Fprintf(os.Stderr, "ch-backfill: done — %s complete from %q\n", plan.label(), streamBucket)
	return nil
}

// backfillProgress reports walk pace: a stderr rate line and the node_exporter heartbeat that tells a wedged walk
// from a working one, sharing one mutex-guarded counter.
type backfillProgress struct {
	hb      *opsutil.JobHeartbeat
	mu      sync.Mutex
	done    int64
	start   time.Time
	lastLog time.Time
	ok      bool
}

// newBackfillProgress starts the heartbeat; heartbeatPath is the -heartbeat flag (empty-path resolution and why it is
// inert off-r1: opsutil.NewJobHeartbeat).
func newBackfillProgress(heartbeatPath string) *backfillProgress {
	hb := opsutil.NewJobHeartbeat("ch-backfill", heartbeatPath, nil)
	if hb.Enabled() {
		fmt.Fprintf(os.Stderr, "ch-backfill: heartbeat -> %s\n", hb.Path())
	}
	// Seed every reason at 0 before Start writes the first sample, so increase() sees a run's first skip.
	for _, reason := range clickhouse.SupplySkipReasons {
		hb.Skipped(reason, 0)
	}
	hb.Start()
	now := time.Now()
	return &backfillProgress{hb: hb, start: now, lastLog: now}
}

// record is the per-ledger callback handed to every chunk worker.
func (p *backfillProgress) record(workerIdx int, seq uint32, supplySkips map[string]int) {
	p.mu.Lock()
	p.done++
	t := p.done
	if time.Since(p.lastLog) >= 15*time.Second {
		rate := float64(t) / time.Since(p.start).Seconds()
		fmt.Fprintf(os.Stderr, "ch-backfill: %d ledgers (worker %d at %d, %.1f ledgers/s)\n",
			t, workerIdx, seq, rate)
		p.lastLog = time.Now()
	}
	p.mu.Unlock()
	p.hb.Progress(uint64(t), uint64(seq)) //nolint:gosec // t is a non-negative running count; seq is a ledger sequence
	for reason, n := range supplySkips {
		p.hb.Skipped(reason, uint64(n)) //nolint:gosec // n is a non-negative skip count
	}
}

// total is the number of ledgers walked so far.
func (p *backfillProgress) total() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// reportRate prints the run's overall throughput line.
func (p *backfillProgress) reportRate() {
	total := p.total()
	elapsed := time.Since(p.start)
	rate := 0.0
	if elapsed.Seconds() > 0 {
		rate = float64(total) / elapsed.Seconds()
	}
	fmt.Fprintf(os.Stderr, "ch-backfill: %d ledgers in %s (%.1f ledgers/s)\n",
		total, elapsed.Round(time.Second), rate)
}

// markOK records the success path; any earlier return, including a short walk rejected by backfillCoverage, stops as
// a FAILED run so stellarindex_ops_job_last_exit_ok matches the process exit code.
func (p *backfillProgress) markOK() {
	p.mu.Lock()
	p.ok = true
	p.mu.Unlock()
}

// stop writes the heartbeat's terminal state. Deferred by chBackfill so it
// runs on every exit path.
func (p *backfillProgress) stop() {
	p.mu.Lock()
	ok := p.ok
	p.mu.Unlock()
	p.hb.Stop(ok)
}

// backfillStreamConfig is the bounded walk's stream config. Trimmed archive ranges (ADR-0027) read through
// to the cold tier, so a re-derive needs no rehydrate first.
func backfillStreamConfig(cfg config.Config, bucket string, parallel int) ledgerstream.Config {
	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, bucket, parallel)
	pipeline.AttachColdTier(cfg, bucket, &lsCfg)
	return lsCfg
}

// backfillBucket resolves which galexie bucket a bounded walk reads. The seam policy lives in
// opsutil.ResolveStreamBucket, shared with census-backfill so the rule has one copy.
func backfillBucket(cfg config.Config, override string, from, to uint32) (string, error) {
	return opsutil.ResolveStreamBucket(cfg, override, from, to)
}

// backfillCoverage fails a walk that missed its range, naming the bucket: TolerateTrailingMissing makes an absent range look clean and
// scripts/ops/ch-full-backfill.sh banks a window only on exit 0. Failing closed costs a re-run (idempotent); failing open a silent hole.
func backfillCoverage(from, to uint32, streamed int64, bucket string, interrupted bool) error {
	want := int64(to) - int64(from) + 1
	switch {
	case interrupted:
		return fmt.Errorf("INTERRUPTED after %d of %d ledgers in [%d,%d] from %q — the range is NOT complete; re-run it (idempotent under ReplacingMergeTree)",
			streamed, want, from, to, bucket)
	case streamed == 0:
		return fmt.Errorf("streamed ZERO of %d ledgers for [%d,%d] from %q — that bucket does not hold this range (wrong bucket for a historic range is the usual cause: pass -bucket <archive bucket>); NOT recording this range as backfilled",
			want, from, to, bucket)
	case streamed < want:
		return fmt.Errorf("streamed only %d of %d ledgers for [%d,%d] from %q — %d ledgers are missing from the bucket or failed to extract (see the per-ledger errors above); the range is NOT complete",
			streamed, want, from, to, bucket, want-streamed)
	}
	return nil
}

// streamFunc is ledgerstream.Stream's shape, a seam so a worker's walk can be driven without a bucket.
type streamFunc func(ctx context.Context, cfg ledgerstream.Config, from, to uint32, callback func(sdkxdr.LedgerCloseMeta) error) error

// extractSink is the part of *clickhouse.Sink a worker writes through.
type extractSink interface {
	Add(ctx context.Context, e clickhouse.LedgerExtract) error
	Flush(ctx context.Context) error
	Close(ctx context.Context) error
}

// clickhouseSinkOpener opens one Sink per worker; clickhouse.Open pins the stellar database.
func clickhouseSinkOpener(chAddr string, flushEvery int) func(context.Context) (extractSink, error) {
	return func(ctx context.Context) (extractSink, error) {
		s, err := clickhouse.Open(ctx, chAddr, flushEvery)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
}

// backfillWorker walks its share of the plan with one Sink; a ClickHouse write failure is fatal so errgroup cancels
// siblings. A nil openSink is the dry run: every planned ledger is still decoded (ADR-0043 drill).
type backfillWorker struct {
	stream      streamFunc
	openSink    func(context.Context) (extractSink, error)
	lsCfg       ledgerstream.Config
	passphrase  string
	changesOnly bool
	plan        backfillPlan
	loss        *extractLoss
	logProgress func(workerIdx int, seq uint32, supplySkips map[string]int)
}

// run walks ranges in order, then flushes the tail before Close so the final partial batch lands.
func (w backfillWorker) run(ctx context.Context, idx int, ranges []opsutil.RangeChunk) error {
	var sink extractSink
	if w.openSink != nil {
		opened, err := w.openSink(ctx)
		if err != nil {
			return err
		}
		sink = opened
		defer func() { _ = sink.Close(ctx) }()
	}
	for _, r := range ranges {
		err := w.stream(ctx, w.lsCfg, r.From, r.To, func(lcm sdkxdr.LedgerCloseMeta) error {
			return w.ledger(ctx, idx, sink, lcm)
		})
		if err != nil {
			return err
		}
	}
	if sink == nil {
		return nil
	}
	return sink.Flush(ctx)
}

// ledger extracts and writes one ledger; a ledger outside a -ledgers list is skipped before extraction.
func (w backfillWorker) ledger(ctx context.Context, idx int, sink extractSink, lcm sdkxdr.LedgerCloseMeta) error {
	if !w.plan.writes(lcm.LedgerSequence()) {
		return nil
	}
	ext, eerr := clickhouse.ExtractLedger(lcm, w.passphrase)
	if eerr != nil {
		fmt.Fprintf(os.Stderr, "ch-backfill: worker %d extract ledger %d: %v\n", idx, lcm.LedgerSequence(), eerr)
		return nil
	}
	if w.loss.record(ext, w.changesOnly) {
		fmt.Fprintf(os.Stderr, "ch-backfill: worker %d ledger %d extracted with a read undercount: tx_read=%d tx_event_read=%d entry_meta_unsupported=%d soroban_fee_meta_unsupported=%d evicted_keys_unreadable=%d entry_changes_unencodable=%d\n",
			idx, lcm.LedgerSequence(), ext.TxReadErrors, ext.TxEventReadErrors, ext.EntryMetaUnsupported,
			ext.SorobanFeeMetaUnsupported, ext.EvictedKeysUnreadable, ext.EntryChangesUnencodable)
	}
	if w.changesOnly {
		ext = changesOnlyExtract(ext)
	}
	if sink != nil {
		if aerr := sink.Add(ctx, ext); aerr != nil {
			return aerr // a ClickHouse write failure is fatal; retry the range
		}
	}
	w.logProgress(idx, lcm.LedgerSequence(), ext.SupplyFlowSkips)
	return nil
}

// extractLoss tallies what ExtractLedger read short across every worker. The ledger is still written to keep the lake
// contiguous, so its ledgers row reads complete and only this tally can fail the run.
type extractLoss struct {
	mu      sync.Mutex
	ledgers int64
	kinds   map[string]int64
	// supplySkips is reported, never failed on: any contract can emit a mint/burn/clawback topic, so failing would let one
	// transaction wedge every re-derive over its ledger.
	supplySkips map[string]int64
}

// supplySkipSummary is the one-line skip tally for the run summary, "" when nothing was skipped.
func (l *extractLoss) supplySkipSummary() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.supplySkips) == 0 {
		return ""
	}
	parts := make([]string, 0, len(l.supplySkips))
	for k, n := range l.supplySkips {
		parts = append(parts, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(parts)
	return "supply events with no supply_flows row (raw events kept in contract_events; only reasons other than " +
		clickhouse.SupplySkipNotTokenShaped + " and " + clickhouse.SupplySkipJudgedNonToken + " short a token's supply): " + strings.Join(parts, " ")
}

// record adds one ledger's undercounts and reports whether it had any. Changes-only writes no txs or events, so it
// ignores the event undercounts; an unreadable tx still counts there, since ExtractLedger never walks its entry changes.
func (l *extractLoss) record(ext clickhouse.LedgerExtract, changesOnly bool) bool {
	kinds := map[string]int{
		"tx_read_errors":            ext.TxReadErrors,
		"entry_meta_unsupported":    ext.EntryMetaUnsupported,
		"entry_changes_unencodable": ext.EntryChangesUnencodable,
		"evicted_keys_unreadable":   ext.EvictedKeysUnreadable,
	}
	if !changesOnly {
		kinds["tx_event_read_errors"] = ext.TxEventReadErrors
		kinds["soroban_fee_meta_unsupported"] = ext.SorobanFeeMetaUnsupported
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !changesOnly {
		for reason, n := range ext.SupplyFlowSkips {
			if l.supplySkips == nil {
				l.supplySkips = map[string]int64{}
			}
			l.supplySkips[reason] += int64(n)
		}
	}
	short := false
	for k, n := range kinds {
		if n == 0 {
			continue
		}
		if l.kinds == nil {
			l.kinds = map[string]int64{}
		}
		l.kinds[k] += int64(n)
		short = true
	}
	if short {
		l.ledgers++
	}
	return short
}

// err fails a run that wrote any ledger short, naming the totals per kind.
func (l *extractLoss) err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ledgers == 0 {
		return nil
	}
	parts := make([]string, 0, len(l.kinds))
	for k, n := range l.kinds {
		parts = append(parts, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(parts)
	return fmt.Errorf("%d ledger(s) extracted with a read undercount (%s) — those ledgers are written but MISSING rows (see the per-ledger lines above); the range is NOT complete",
		l.ledgers, strings.Join(parts, " "))
}

// changesOnlyExtract keeps the ledgers row (Sink.Flush writes it last as the per-ledger commit marker) and the entry
// changes. Built from an empty value so a table added to LedgerExtract later is excluded, not rewritten.
func changesOnlyExtract(e clickhouse.LedgerExtract) clickhouse.LedgerExtract {
	return clickhouse.LedgerExtract{Ledger: e.Ledger, Changes: e.Changes}
}
