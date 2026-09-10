// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── sizing constants ───────────────────────────────────────────────────

// assetRegistryPageDefault is how many distinct assets one ClickHouse page
// returns.
//
// The LIMIT cannot stop an aggregation early, so every page re-aggregates
// the lake's trustline range and the page size trades passes against
// per-pass cost. 25,000 puts the ~512k-asset population at ~21 pages,
// which is few enough that the repeated scans stay a rounding error beside
// the Postgres write, and small enough that a page lands on the heartbeat
// well inside the 30-minute flat-progress alert window.
const assetRegistryPageDefault = 25000

// assetRegistryBatchDefault is how many observations are handed to the
// store per call. The store re-batches internally against the Postgres
// bind-parameter ceiling; this bound is about how much work one
// cancellation point covers.
const assetRegistryBatchDefault = 2000

// assetRegistryBytesPerAsset is the pre-flight's per-asset disk estimate:
// the heap tuple (an asset_id and a slug of ~70 bytes each, a 56-byte
// issuer strkey, a code, eight timestamps/integers, the tuple header) plus
// its share of the six indexes on the table, plus WAL for the write.
//
// It is an ESTIMATE and the pre-flight says so. It is deliberately generous
// — roughly 3x a tight reading of the row — because the number exists to
// refuse a run that would fill the volume, not to predict the final table
// size to the megabyte.
const assetRegistryBytesPerAsset = 800

// assetRegistryLogEvery throttles the stderr progress line to one per this
// many assets, so an unattended run leaves a readable journal rather than
// half a million lines.
const assetRegistryLogEvery = 25000

// ─── seams ──────────────────────────────────────────────────────────────

// assetRegistryScanner is the lake seam — clickhouse.HoldingsScanner
// satisfies it.
type assetRegistryScanner interface {
	TrustlineAssetsAfter(ctx context.Context, after string, limit int) ([]clickhouse.TrustlineAssetSeed, error)
}

// assetRegistryStore is the served-tier seam — timescale.Store satisfies
// it. Note what is NOT here: no INSERT, no UPDATE, no table name. This job
// is a feeder, and `classic_assets` / `issuers` keep the one writer they
// have always had (internal/storage/timescale/asset_registry.go).
type assetRegistryStore interface {
	RegisterClassicAssetsHeld(ctx context.Context, obs []timescale.ClassicAssetHolding) (int64, int64, error)
	ClassicAssetRegistryStats(ctx context.Context) (timescale.ClassicAssetRegistryStats, error)
}

// ─── counters ───────────────────────────────────────────────────────────

// assetRegistryCounts is one run's accounting. Every asset the lake offered
// lands in exactly one of registered / skippedNonClassic / skippedUnparsed,
// so an operator can check the arithmetic instead of trusting the summary.
type assetRegistryCounts struct {
	scanned            int64 // rows the lake returned
	registered         int64 // observations handed to the writer
	skippedNonClassic  int64 // parsed, but not a (code, issuer) classic asset
	skippedUnparsed    int64 // the lake asset string would not parse
	assetRowsTouched   int64 // classic_assets rows the upsert reported
	issuerRowsInserted int64 // issuers rows that did not already exist
	pages              int
	maxLedger          uint32
	cursor             string // last asset handed to the writer
}

// ─── entry point ────────────────────────────────────────────────────────

// assetRegistryBackfill populates `classic_assets` (and therefore
// `issuers`) from trustline holdings in the ClickHouse lake.
//
// # THE DEFECT THIS CLOSES
//
// `classic_assets` had exactly one population path: a trade. The registry
// writer is called from InsertTrade and BatchInsertTrades and from nowhere
// else, and `issuers` is written only from inside that writer. So the whole
// attestation chain hung off a trade:
//
//	trade -> classic_assets -> issuers -> SEP-1 fetch -> RWA candidacy
//
// An asset that is HELD but never traded on the SDEX was invisible at every
// step. Measured on the production lake 2026-09-10: 512,496 distinct classic
// assets have a trustline, 199,793 have a registry row — 312,703 absent, 61%
// of the population. Franklin Templeton's BENJI is the case that surfaced it:
// 12,498 trustlines, more than all eighteen impersonating BENJIs combined,
// zero rows in classic_assets and zero in issuers, because a money-market
// fund is bought and held rather than day-traded.
//
// # WHY AN OPS JOB AND NOT A LIVE HOOK
//
// The lake is the source of truth for entry state (ADR-0034), and coverage
// here is derived from the data rather than from a cursor (ADR-0031). A
// periodic reconcile that reads the lake and converges the registry is the
// same shape as the asset-holders rollup that already runs on a timer, and
// it is idempotent and monotone, so a missed run costs freshness and nothing
// else. The alternative — registering from every LedgerEntryChange on the
// dispatcher hot path — would put a Postgres upsert behind every trustline
// change on the network for an answer that only needs to be right on the
// cadence at which a SEP-1 refresh runs anyway.
//
// # RESUMING
//
// The walk is keyset-paginated on the asset string, ascending. Any early
// stop — signal, timeout, -limit, error — prints a RESUME line carrying
// -resume-from <asset_id>, and re-running from it re-enters exactly where
// the walk stopped.
//
// Resuming is an OPTIMISATION, not a correctness requirement. Both writes
// are idempotent and monotone (LEAST/GREATEST, no counter increment), so a
// run restarted from the beginning converges to the same rows; it just pays
// for ground it has already covered.
func assetRegistryBackfill(args []string) error {
	fs := flag.NewFlagSet("asset-registry-backfill", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	page := fs.Int("page", assetRegistryPageDefault, "Distinct assets per ClickHouse page")
	batch := fs.Int("batch", assetRegistryBatchDefault, "Observations per store call")
	limit := fs.Int64("limit", 0, "Stop after this many assets have been scanned (0 = walk to the end). Bounds a first tranche so the listing-spine growth can be measured between runs")
	resumeFrom := fs.String("resume-from", "", "Re-enter the walk strictly after this asset_id — the value a previous run printed on its RESUME line")
	timeout := fs.Duration("timeout", 6*time.Hour, "Wall-clock budget for the whole run. Reaching it is not an error: the walk stops cleanly and prints its RESUME line")
	minFree := fs.Int64("min-free-bytes", 0, "OVERRIDE the free-space measurement on the Postgres data volume with this figure. Use when running off the database host, where statfs measures the wrong filesystem")
	heartbeat := fs.String("heartbeat", "", "node_exporter textfile path for the liveness/progress gauges. Empty = "+opsutil.DefaultTextfileDir+"/ops_job_asset_registry_backfill.prom when that directory exists (r1), otherwise no heartbeat at all")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if *page <= 0 || *batch <= 0 {
		return errors.New("-page and -batch must be positive")
	}
	dryRun := !gate.Banner()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	// SignalContext first so a SIGTERM during the walk reaches the loop as
	// a clean stop with a RESUME line, then the wall-clock budget on top.
	sigCtx, cancelSig := opsutil.SignalContext()
	defer cancelSig()
	ctx, cancel := context.WithTimeout(sigCtx, *timeout)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	scanner, err := clickhouse.NewHoldingsScanner(ctx, *chAddr)
	if err != nil {
		return fmt.Errorf("asset-registry-backfill: clickhouse: %w", err)
	}
	defer func() { _ = scanner.Close() }()

	return runAssetRegistryBackfill(ctx, store, scanner, assetRegistryOpts{
		configPath:   *cfgPath,
		chAddr:       *chAddr,
		page:         *page,
		batch:        *batch,
		limit:        *limit,
		resumeFrom:   *resumeFrom,
		minFreeBytes: *minFree,
		heartbeat:    *heartbeat,
		dryRun:       dryRun,
		out:          os.Stderr,
		freeBytes:    freeBytesOnVolume,
		volumePath:   store.RelationDataVolumePath,
	})
}

type assetRegistryOpts struct {
	configPath   string
	chAddr       string
	page         int
	batch        int
	limit        int64
	resumeFrom   string
	minFreeBytes int64
	heartbeat    string
	dryRun       bool
	out          io.Writer

	// freeBytes / volumePath are the pre-flight seams, injected so the
	// refusal logic is testable without a database or a filesystem.
	freeBytes  func(path string) (uint64, error)
	volumePath func(ctx context.Context, relation string) (string, error)
}

// runAssetRegistryBackfill is the walk proper, split from the flag/config
// wiring so the page -> parse -> write -> report loop is testable against
// stub seams.
func runAssetRegistryBackfill(
	ctx context.Context,
	store assetRegistryStore,
	scanner assetRegistryScanner,
	o assetRegistryOpts,
) (err error) {
	before, err := store.ClassicAssetRegistryStats(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(o.out, "asset-registry-backfill: registry before — %s\n", describeRegistryStats(before))

	if perr := assetRegistryPreflight(ctx, o, before); perr != nil {
		return perr
	}

	hb := opsutil.NewJobHeartbeat("asset-registry-backfill", o.heartbeat, nil)
	if hb.Enabled() {
		_, _ = fmt.Fprintf(o.out, "asset-registry-backfill: heartbeat -> %s\n", hb.Path())
	}
	hb.Start()
	// exitOK is flipped only by a walk that reached the end of the lake.
	// A run that stopped on its timeout, a signal or -limit did real work
	// and is resumable, but it did NOT finish, and last_exit_ok must not
	// say it did.
	exitOK := false
	defer func() { hb.Stop(exitOK && err == nil) }()

	c := assetRegistryCounts{cursor: o.resumeFrom}
	complete, err := assetRegistryWalk(ctx, store, scanner, o, hb, &c)
	after, serr := store.ClassicAssetRegistryStats(context.WithoutCancel(ctx))
	if serr == nil {
		_, _ = fmt.Fprintf(o.out, "asset-registry-backfill: registry after  — %s\n", describeRegistryStats(after))
	}
	_, _ = fmt.Fprintf(o.out, "asset-registry-backfill: %s\n", describeRunCounts(c, o.dryRun))
	if err != nil {
		_, _ = fmt.Fprint(o.out, assetRegistryResumeLine(o, c.cursor))
		return err
	}
	if !complete {
		_, _ = fmt.Fprint(o.out, assetRegistryResumeLine(o, c.cursor))
		return nil
	}
	exitOK = true
	_, _ = fmt.Fprintln(o.out, "asset-registry-backfill: walk COMPLETE — the lake offered no asset after the final cursor.")
	return nil
}

// assetRegistryWalk pages the lake until it runs out, the budget runs out,
// or -limit binds. Returns complete=true only for the first of those.
func assetRegistryWalk(
	ctx context.Context,
	store assetRegistryStore,
	scanner assetRegistryScanner,
	o assetRegistryOpts,
	hb *opsutil.JobHeartbeat,
	c *assetRegistryCounts,
) (bool, error) {
	start := time.Now()
	nextLog := int64(assetRegistryLogEvery)
	for {
		if ctx.Err() != nil {
			// Timeout or signal. NOT an error: the walk is resumable by
			// construction and the RESUME line says from where. A
			// -timeout that binds is the DESIGNED end of a bounded run,
			// so surfacing it as a failure would make every healthy
			// nightly run exit non-zero.
			_, _ = fmt.Fprintln(o.out, "asset-registry-backfill: budget reached or signal received — stopping early.")
			return false, nil //nolint:nilerr // ctx.Err() here is the budget or the operator, not a fault
		}
		seeds, err := scanner.TrustlineAssetsAfter(ctx, c.cursor, assetRegistryPageSize(o, c.scanned))
		if err != nil {
			if ctx.Err() != nil {
				// The page failed because the context ended under it —
				// the same designed stop as above, reached mid-query.
				// A genuine ClickHouse error still falls through.
				_, _ = fmt.Fprintln(o.out, "asset-registry-backfill: budget reached or signal received mid-page — stopping early.")
				return false, nil //nolint:nilerr // the cause is ctx, not the query
			}
			return false, err
		}
		if len(seeds) == 0 {
			return true, nil
		}
		c.pages++
		if err := assetRegistryWritePage(ctx, store, o, hb, seeds, c); err != nil {
			return false, err
		}
		if c.scanned >= nextLog {
			nextLog = c.scanned + assetRegistryLogEvery
			elapsed := time.Since(start)
			_, _ = fmt.Fprintf(o.out, "  ... %d assets scanned, %d registered, %d issuers new, %d page(s), %s (%.0f assets/s), cursor=%s\n",
				c.scanned, c.registered, c.issuerRowsInserted, c.pages,
				elapsed.Round(time.Second), float64(c.scanned)/elapsed.Seconds(), c.cursor)
		}
		if o.limit > 0 && c.scanned >= o.limit {
			_, _ = fmt.Fprintf(o.out, "asset-registry-backfill: -limit %d reached — stopping early.\n", o.limit)
			return false, nil
		}
	}
}

// assetRegistryPageSize narrows the next page to what -limit still allows.
//
// Without it a tranche of 1,000 against a 25,000-row page would scan
// 25,000 assets and count the overshoot as progress — which matters
// because -limit exists precisely so an operator can land the population
// in measured steps and check the listing spine between them.
//
// Never returns zero: the caller stops as soon as scanned reaches the
// limit, so it is only ever asked while at least one asset remains.
func assetRegistryPageSize(o assetRegistryOpts, scanned int64) int {
	if o.limit <= 0 {
		return o.page
	}
	if remaining := o.limit - scanned; remaining < int64(o.page) {
		return int(remaining)
	}
	return o.page
}

// assetRegistryWritePage converts one lake page into observations and hands
// them to the registry writer in -batch slices.
//
// The lake's asset string is PARSED, never split: identity is (code,
// issuer), and eighteen assets on this network wear the code BENJI. A
// string that will not parse, or that parses as something other than a
// classic credit asset, is counted and dropped rather than guessed at.
func assetRegistryWritePage(
	ctx context.Context,
	store assetRegistryStore,
	o assetRegistryOpts,
	hb *opsutil.JobHeartbeat,
	seeds []clickhouse.TrustlineAssetSeed,
	c *assetRegistryCounts,
) error {
	obs := make([]timescale.ClassicAssetHolding, 0, len(seeds))
	for _, s := range seeds {
		c.scanned++
		c.cursor = s.Asset
		if s.LastLedger > c.maxLedger {
			c.maxLedger = s.LastLedger
		}
		asset, perr := canonical.ParseAsset(s.Asset)
		if perr != nil {
			c.skippedUnparsed++
			continue
		}
		if asset.Type != canonical.AssetClassic {
			c.skippedNonClassic++
			continue
		}
		obs = append(obs, timescale.ClassicAssetHolding{
			Asset:       asset,
			FirstLedger: s.FirstLedger,
			FirstAt:     s.FirstAt,
			LastLedger:  s.LastLedger,
			LastAt:      s.LastAt,
		})
	}
	c.registered += int64(len(obs))
	// Report progress for the page even in dry run and even when the page
	// registered nothing: the alert asks whether the process is doing
	// work, and a page of assets it deliberately skipped is work.
	hb.Progress(uint64(c.scanned), uint64(c.maxLedger)) //nolint:gosec // scanned is a non-negative running count
	if o.dryRun {
		return nil
	}
	for start := 0; start < len(obs); start += o.batch {
		end := min(start+o.batch, len(obs))
		assets, issuers, err := store.RegisterClassicAssetsHeld(ctx, obs[start:end])
		c.assetRowsTouched += assets
		c.issuerRowsInserted += issuers
		if err != nil {
			return fmt.Errorf("asset-registry-backfill: register batch at cursor %s: %w", c.cursor, err)
		}
		// Per BATCH, not per page. stellarindex_ops_job_no_progress
		// fires on 30 minutes of flat progress_total, and the only
		// structurally-flat phase here is the ClickHouse aggregation
		// at the head of a page; reporting once per page would put the
		// whole Postgres write phase inside that flat window too.
		hb.Progress(uint64(c.scanned), uint64(c.maxLedger)) //nolint:gosec // scanned is a non-negative running count
	}
	return nil
}

// ─── pre-flight ─────────────────────────────────────────────────────────

// assetRegistryPreflight refuses a write run that could fill the Postgres
// data volume.
//
// It is deliberately modest about what it is guarding. The write here is a
// few hundred megabytes, not the hundreds of gigabytes a chunk decompress
// moves — so this reports the arithmetic and gets out of the way, rather
// than pretending to a precision it does not have. What it will not do is
// start a multi-hour run on a volume that is already nearly full.
//
// The figure compared is, in order of preference: -min-free-bytes when the
// operator gave one (trusted as stated, warned about loudly), otherwise
// statfs on the directory Postgres reports for `classic_assets`. Neither
// available is a WARNING here rather than a refusal — unlike the chunk
// restamp, whose failure mode is a half-decompressed 160 GB chunk, the
// worst this job can do to a full volume is fail an INSERT.
func assetRegistryPreflight(ctx context.Context, o assetRegistryOpts, before timescale.ClassicAssetRegistryStats) error {
	if o.dryRun {
		return nil
	}
	// The population the walk can add is bounded by the lake's asset count,
	// which this job does not know before it starts. Size the guard on the
	// measured gap instead — 512,496 in the lake against what is registered
	// now — and let the generous per-asset figure absorb the error.
	const lakeClassicAssets = 512496
	pending := int64(lakeClassicAssets) - before.Total
	if pending < 0 {
		pending = 0
	}
	need := uint64(pending) * assetRegistryBytesPerAsset //nolint:gosec // pending is clamped non-negative

	if o.minFreeBytes > 0 {
		_, _ = fmt.Fprintf(o.out, "pre-flight: free %s (-min-free-bytes, NOT measured); estimated need %s for ~%d new rows\n",
			fmtByteCount(o.minFreeBytes), fmtByteCount(int64(need)), pending) //nolint:gosec // need derives from a clamped int64
		_, _ = fmt.Fprintln(o.out, "WARNING: trusting -min-free-bytes as the free space on the Postgres data volume; nothing was measured.")
		if uint64(o.minFreeBytes) <= need { //nolint:gosec // minFreeBytes is positive here
			return fmt.Errorf("asset-registry-backfill: -min-free-bytes %s is not more than the estimated need %s — free space or run with -limit",
				fmtByteCount(o.minFreeBytes), fmtByteCount(int64(need))) //nolint:gosec // need derives from a clamped int64
		}
		return nil
	}

	path, perr := o.volumePath(ctx, "classic_assets")
	if perr != nil {
		_, _ = fmt.Fprintf(o.out, "pre-flight: free space UNKNOWN (%v) — run on the database host as a role that can read data_directory, or pass -min-free-bytes N. Estimated need %s; proceeding.\n",
			perr, fmtByteCount(int64(need))) //nolint:gosec // need derives from a clamped int64
		return nil
	}
	free, ferr := o.freeBytes(path)
	if ferr != nil {
		_, _ = fmt.Fprintf(o.out, "pre-flight: cannot measure free space on %s (%v) — this host is probably not the database host; pass -min-free-bytes N to assert it. Estimated need %s; proceeding.\n",
			path, ferr, fmtByteCount(int64(need))) //nolint:gosec // need derives from a clamped int64
		return nil
	}
	_, _ = fmt.Fprintf(o.out, "pre-flight: free %s on %s (measured); estimated need %s for ~%d new rows\n",
		fmtByteCount(int64(free)), path, fmtByteCount(int64(need)), pending) //nolint:gosec // statfs product, bounded by the volume size
	if free <= need {
		return fmt.Errorf("asset-registry-backfill: free space %s on %s is not more than the estimated need %s — free space, or bound this run with -limit",
			fmtByteCount(int64(free)), path, fmtByteCount(int64(need))) //nolint:gosec // statfs product
	}
	return nil
}

// freeBytesOnVolume is statfs, in bytes available to a non-root writer.
func freeBytesOnVolume(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec,unconvert // block size is positive; the field width differs per OS
}

// ─── reporting ──────────────────────────────────────────────────────────

// assetRegistryResumeLine is the exact command that continues this walk.
//
// Printed on every early exit, in full, because an operator reading a
// journal at 03:00 should not have to reconstruct which flags the run
// carried. The population flags are repeated verbatim for the same reason
// the chunk restamp repeats its own.
func assetRegistryResumeLine(o assetRegistryOpts, cursor string) string {
	if cursor == "" {
		return "RESUME: nothing was scanned — rerun the same command.\n"
	}
	var b strings.Builder
	b.WriteString("RESUME: stellarindex-ops asset-registry-backfill")
	fmt.Fprintf(&b, " -config %s -ch-addr %s -page %d -batch %d", o.configPath, o.chAddr, o.page, o.batch)
	if o.limit > 0 {
		fmt.Fprintf(&b, " -limit %d", o.limit)
	}
	if o.minFreeBytes > 0 {
		fmt.Fprintf(&b, " -min-free-bytes %d", o.minFreeBytes)
	}
	fmt.Fprintf(&b, " -resume-from %s", cursor)
	if !o.dryRun {
		b.WriteString(" -write")
	}
	b.WriteString("\n")
	return b.String()
}

// describeRegistryStats renders the registry population by source.
func describeRegistryStats(s timescale.ClassicAssetRegistryStats) string {
	return fmt.Sprintf("total=%d traded=%d held=%d held_never_traded=%d traded_no_holding_evidence=%d",
		s.Total, s.WithTrade, s.WithHolding, s.HoldingOnly, s.TradeOnly)
}

// describeRunCounts renders the run's own accounting. scanned must equal
// registered + skipped_non_classic + skipped_unparsed; an operator who
// checks that arithmetic is checking the walk, not trusting it.
func describeRunCounts(c assetRegistryCounts, dryRun bool) string {
	return fmt.Sprintf(
		"scanned=%d registered=%d skipped_non_classic=%d skipped_unparsed=%d asset_rows_considered=%d issuer_rows_new=%d pages=%d dry_run=%v",
		c.scanned, c.registered, c.skippedNonClassic, c.skippedUnparsed,
		c.assetRowsTouched, c.issuerRowsInserted, c.pages, dryRun)
}

// fmtByteCount renders a byte count for an operator, not for a machine.
func fmtByteCount(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
