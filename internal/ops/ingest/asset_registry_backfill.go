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

// assetRegistryPageDefault is how many distinct assets one ClickHouse page returns.
// LIMIT cannot stop the aggregation early, so each page re-scans the trustline
// range; 25,000 (~21 pages) keeps repeat scans cheap yet lands within the 30-minute progress alert.
const assetRegistryPageDefault = 25000

// assetRegistryBatchDefault is how many observations are handed to the
// store per call. The store re-batches internally against the Postgres
// bind-parameter ceiling; this bound is about how much work one
// cancellation point covers.
const assetRegistryBatchDefault = 2000

// assetRegistryBytesPerAsset is the pre-flight's deliberately generous (~3x) per-asset disk
// estimate (heap tuple, six indexes, WAL); it exists to refuse a run that would fill the volume.
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
	CountTrustlineAssets(ctx context.Context) (int64, error)
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

// assetRegistryBackfill populates classic_assets (and issuers) from trustline holdings in the lake.
// Held-but-never-traded assets never reach the registry via InsertTrade, so this closes the gap
// (lake is source of truth, ADR-0034). Keyset-paginated; early stops print a RESUME line with
// -resume-from, and writes are idempotent and monotone so a restart from zero converges.
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

	if perr := assetRegistryPreflight(ctx, o, scanner, before); perr != nil {
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
			// Timeout or signal is NOT an error: the walk is resumable and the RESUME line says from where;
			// a binding -timeout is the designed end of a bounded run.
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
			if errors.Is(err, errAssetRegistryWriteStoppedByContext) {
				_, _ = fmt.Fprintln(o.out, "asset-registry-backfill: budget reached or signal received mid-write — stopping early.")
				return false, nil
			}
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

// assetRegistryPageSize narrows the next page to what -limit still allows, so a small
// tranche does not count a full page's overshoot as progress. Never returns zero.
func assetRegistryPageSize(o assetRegistryOpts, scanned int64) int {
	if o.limit <= 0 {
		return o.page
	}
	if remaining := o.limit - scanned; remaining < int64(o.page) {
		return int(remaining)
	}
	return o.page
}

// errAssetRegistryWriteStoppedByContext marks a write-phase error that is really the budget or a
// signal; [assetRegistryWalk] turns it back into the non-error early stop.
var errAssetRegistryWriteStoppedByContext = errors.New("asset-registry-backfill: context ended during write")

// assetRegistryWritePage hands one lake page to the registry writer in -batch slices.
// Asset strings are PARSED, never split (identity is (code, issuer)); unparseable or non-credit
// assets are counted and dropped. c.cursor advances only past assets whose batch succeeded
// (resume is strictly-after), or to the page end once done; running ahead would skip assets.
func assetRegistryWritePage(
	ctx context.Context,
	store assetRegistryStore,
	o assetRegistryOpts,
	hb *opsutil.JobHeartbeat,
	seeds []clickhouse.TrustlineAssetSeed,
	c *assetRegistryCounts,
) error {
	obs := make([]timescale.ClassicAssetHolding, 0, len(seeds))
	obsAsset := make([]string, 0, len(seeds)) // seeds[i].Asset for obs[i], same order
	for _, s := range seeds {
		c.scanned++
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
		obsAsset = append(obsAsset, s.Asset)
	}
	c.registered += int64(len(obs))
	// Report progress for the page even in dry run and even when the page
	// registered nothing: the alert asks whether the process is doing
	// work, and a page of assets it deliberately skipped is work.
	hb.Progress(uint64(c.scanned), uint64(c.maxLedger)) //nolint:gosec // scanned is a non-negative running count
	if o.dryRun {
		c.cursor = seeds[len(seeds)-1].Asset
		return nil
	}
	for start := 0; start < len(obs); start += o.batch {
		end := min(start+o.batch, len(obs))
		assets, issuers, err := store.RegisterClassicAssetsHeld(ctx, obs[start:end])
		if err != nil {
			if ctx.Err() != nil {
				return errAssetRegistryWriteStoppedByContext
			}
			return fmt.Errorf("asset-registry-backfill: register batch at cursor %s: %w", c.cursor, err)
		}
		c.assetRowsTouched += assets
		c.issuerRowsInserted += issuers
		// Only NOW is it true that every asset up to and including this
		// one was written. Advance past this batch, never further.
		c.cursor = obsAsset[end-1]
		// Per BATCH, not per page: the flat-progress alert fires after 30 minutes, so page-level
		// reporting would put the whole write phase inside that window.
		hb.Progress(uint64(c.scanned), uint64(c.maxLedger)) //nolint:gosec // scanned is a non-negative running count
	}
	// The whole page wrote cleanly, including any trailing seeds that were
	// legitimately skipped (unparsed / non-classic) rather than batched —
	// safe to move past those too, since a skip is deterministic and
	// re-seeing them on resume would just re-skip them.
	c.cursor = seeds[len(seeds)-1].Asset
	return nil
}

// ─── pre-flight ─────────────────────────────────────────────────────────

// assetRegistryPreflight refuses a write run that could fill the Postgres data volume.
// Free space comes from -min-free-bytes (trusted as stated) else statfs on the classic_assets
// directory; neither available is a warning, since the worst case here is a failed INSERT.
func assetRegistryPreflight(ctx context.Context, o assetRegistryOpts, scanner assetRegistryScanner, before timescale.ClassicAssetRegistryStats) error {
	if o.dryRun {
		return nil
	}
	// The population the walk can add is bounded by the lake's asset count;
	// measure it now rather than trusting a constant that goes stale as the
	// lake grows. Without it the guard has no denominator, so refuse.
	lakeAssets, err := scanner.CountTrustlineAssets(ctx)
	if err != nil {
		return fmt.Errorf("asset-registry-backfill: pre-flight cannot size the run: %w", err)
	}
	pending := lakeAssets - before.Total
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

// assetRegistryResumeLine is the exact command that continues this walk, printed in full on every
// early exit with the population flags repeated verbatim.
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
