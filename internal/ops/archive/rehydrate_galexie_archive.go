package archive

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// ─── stellarindex-ops rehydrate-galexie-archive ──────────────────
//
// Per ADR-0027 §Step 2: a non-destructive operator that re-copies
// LCM files from the cold tier (aws-public-blockchain, the AWS
// Open Data Sponsorship bucket) back into the hot tier (local
// galexie-archive MinIO bucket) for a given ledger range.
//
// Use cases:
//   - Recovering from accidental trim — re-fetch a range an
//     operator trimmed too aggressively.
//   - Pre-warming hot before a planned backfill — pull the
//     historical range to local disk so the backfill avoids
//     per-LCM cross-Atlantic latency.
//   - Cold-tier integrity spot check — read every file the
//     trimmed range claims to hold upstream; surface any
//     unexpected gaps.
//
// Idempotent: uses PutFileIfNotExists, so re-running over a
// range that's already hydrated is a no-op (skipped files are
// counted but not refetched).
//
// Sees only schema-aligned ledger-file boundaries — the SDK's
// DataStoreSchema.GetObjectKeyFromSequenceNumber gives the path;
// we step by LedgersPerFile so each file is fetched once.

type rehydrateOpts struct {
	cfgPath string
	from    uint32
	to      uint32
	dryRun  bool
}

func rehydrateGalexieArchive(args []string) error { //nolint:gocognit,gocyclo,funlen // CLI plumbing + per-file loop + reporting; one continuous procedure is easier to follow than five helpers
	opts, err := parseRehydrateFlags(args)
	if err != nil {
		return err
	}
	if opts.from == 0 || opts.to == 0 || opts.from > opts.to {
		return fmt.Errorf("invalid -from / -to: from=%d to=%d (both required; from <= to)", opts.from, opts.to)
	}

	// LoadWithEnv (not bare Load) so the STELLARINDEX_* env overrides —
	// the injected Postgres DSN / Redis + ClickHouse secrets — take
	// effect, matching every other binary (cmd/stellarindex-{api,indexer,
	// aggregator} and the rest of internal/ops). Bare Load left this
	// operator reading the placeholder TOML DSN/secrets (C3-14).
	cfg, err := config.LoadWithEnv(opts.cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.Storage.ColdTieringEnabled() {
		return fmt.Errorf("cold tier not configured — set storage.s3_cold_bucket_archive in %s before rehydrating", opts.cfgPath)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	hot, err := datastore.NewDataStore(rootCtx, datastore.DataStoreConfig{
		Type: "S3",
		Params: map[string]string{
			"destination_bucket_path": cfg.Storage.S3BucketArchive,
			"region":                  cfg.Storage.S3Region,
			"endpoint_url":            cfg.Storage.S3Endpoint,
		},
		NetworkPassphrase: cfg.Stellar.Passphrase(),
		Compression:       "zstd",
	})
	if err != nil {
		return fmt.Errorf("hot datastore: %w", err)
	}
	defer func() { _ = hot.Close() }()

	cold, err := datastore.NewDataStore(rootCtx, datastore.DataStoreConfig{
		Type: "S3",
		Params: map[string]string{
			"destination_bucket_path": cfg.Storage.S3ColdBucketArchive,
			"region":                  cfg.Storage.S3ColdRegion,
			"endpoint_url":            cfg.Storage.S3ColdEndpoint,
		},
		NetworkPassphrase: cfg.Stellar.Passphrase(),
		Compression:       "zstd",
	})
	if err != nil {
		return fmt.Errorf("cold datastore: %w", err)
	}
	defer func() { _ = cold.Close() }()

	// Schema discovery via hot — both tiers must publish compatible
	// schemas (Galexie's manifest is bucket-local but the LedgersPerFile
	// + FilesPerPartition + extension match across mirrors of the same
	// network). Using hot is the safer side: hot is where we write.
	schema, err := datastore.LoadSchema(rootCtx, hot, datastore.DataStoreConfig{
		Type: "S3",
		Params: map[string]string{
			"destination_bucket_path": cfg.Storage.S3BucketArchive,
			"region":                  cfg.Storage.S3Region,
			"endpoint_url":            cfg.Storage.S3Endpoint,
		},
	})
	if err != nil {
		return fmt.Errorf("load schema (hot): %w", err)
	}

	paths := rehydratePaths(schema, opts.from, opts.to)
	logger.Info("rehydrate plan",
		"from", opts.from,
		"to", opts.to,
		"files", len(paths),
		"ledgers_per_file", schema.LedgersPerFile,
		"dry_run", opts.dryRun,
	)

	start := time.Now()
	skipped, copied, missing, errs, err := rehydrateFiles(rootCtx, logger, hot, cold, paths, opts.dryRun)
	if err != nil {
		return err
	}

	logger.Info("rehydrate complete",
		"files", len(paths),
		"skipped_already_present", skipped,
		"copied", copied,
		"missing_in_cold", missing,
		"errors", errs,
		"elapsed", time.Since(start).String(),
		"dry_run", opts.dryRun,
	)
	if errs > 0 {
		return fmt.Errorf("rehydrate finished with %d errors", errs)
	}
	return nil
}

// rehydrateStore is the seam rehydrateFiles depends on — the slice of
// datastore.DataStore this command actually calls, split out so the
// per-file copy loop (including the not-found-vs-transient
// distinction on the cold side) is unit-testable with a fake, without
// live S3/MinIO. datastore.DataStore satisfies this structurally.
type rehydrateStore interface {
	Exists(ctx context.Context, path string) (bool, error)
	GetFile(ctx context.Context, path string) (io.ReadCloser, int64, error)
	PutFileIfNotExists(ctx context.Context, path string, in io.WriterTo, metaData map[string]string) (bool, error)
}

// rehydrateOutcome buckets a single file's processing result — kept
// as a small enum so rehydrateFiles' loop body is a flat switch
// instead of a chain of if/continue (cognitive-complexity budget).
type rehydrateOutcome int

const (
	rehydrateSkipped rehydrateOutcome = iota
	rehydrateCopied
	rehydrateMissing
	rehydrateErrored
)

// rehydrateFiles is the DB/S3-seamed core of the per-file copy loop.
func rehydrateFiles(ctx context.Context, logger *slog.Logger, hot, cold rehydrateStore, paths []string, dryRun bool) (skipped, copied, missing, errs int, err error) {
	for i, path := range paths {
		if cerr := ctx.Err(); cerr != nil {
			return skipped, copied, missing, errs, fmt.Errorf("rehydrate aborted at %d/%d: %w", i, len(paths), cerr)
		}
		switch rehydrateOneFile(ctx, logger, hot, cold, path, dryRun) {
		case rehydrateSkipped:
			skipped++
		case rehydrateCopied:
			copied++
		case rehydrateMissing:
			missing++
		case rehydrateErrored:
			errs++
		}
	}
	return skipped, copied, missing, errs, nil
}

// rehydrateOneFile decides one path's outcome: already in hot
// (skipped), copied from cold, a genuine cold-side gap, or an error.
func rehydrateOneFile(ctx context.Context, logger *slog.Logger, hot, cold rehydrateStore, path string, dryRun bool) rehydrateOutcome {
	hotExists, herr := hot.Exists(ctx, path)
	if herr != nil {
		logger.Warn("hot.Exists failed", "path", path, "err", herr)
		return rehydrateErrored
	}
	if hotExists {
		return rehydrateSkipped
	}
	if dryRun {
		return rehydrateCopied
	}
	return copyColdToHot(ctx, logger, hot, cold, path)
}

// copyColdToHot performs the actual cold->hot copy for one path that
// is confirmed absent from hot.
//
// DAT-09: a cold.GetFile error was previously ALWAYS counted as
// "missing in cold" (a genuine archive gap), even when the failure
// was actually transient (network blip, throttling, auth hiccup) —
// which both misreports a real cold-tier read fault as a data gap
// AND still exits 0, since only `errs` (not `missing`) forces the
// caller's non-zero exit. cold.Exists is now checked FIRST: a
// definitive not-found (exists=false, err=nil) is the only path that
// reports rehydrateMissing; any Exists error, or any GetFile error
// AFTER Exists confirmed presence, reports rehydrateErrored instead.
func copyColdToHot(ctx context.Context, logger *slog.Logger, hot, cold rehydrateStore, path string) rehydrateOutcome {
	// Confirm presence on cold BEFORE reading, so a transient GetFile
	// fault (the file IS there but the read failed) is never
	// conflated with a genuine gap (the file is NOT there).
	coldExists, cerr := cold.Exists(ctx, path)
	if cerr != nil {
		logger.Warn("cold.Exists failed", "path", path, "err", cerr)
		return rehydrateErrored
	}
	if !coldExists {
		// Definitive absence — the genuine archive-hole signal
		// operators inspect missing.count to discover (e.g. an LCM
		// that genuinely never landed upstream). Don't abort.
		logger.Warn("cold file absent — genuine gap", "path", path)
		return rehydrateMissing
	}
	// Read from cold, write to hot. Stream through a buffer so
	// PutFile's WriterTo contract is satisfied; the LCM file sizes
	// are bounded (~tens of KB per file at LedgersPerFile=1 —
	// Galexie's default) so in-memory buffering is fine.
	rc, _, gerr := cold.GetFile(ctx, path)
	if gerr != nil {
		// cold.Exists just confirmed the file IS there — a GetFile
		// failure now is a transient/infra fault, NOT a gap. Must
		// NOT be counted as missing.
		logger.Warn("cold.GetFile failed despite Exists confirming presence", "path", path, "err", gerr)
		return rehydrateErrored
	}
	body, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		logger.Warn("cold body drain failed", "path", path, "err", rerr)
		return rehydrateErrored
	}
	ok, perr := hot.PutFileIfNotExists(ctx, path, byteWriterTo(body), nil)
	if perr != nil {
		logger.Warn("hot.PutFileIfNotExists failed", "path", path, "err", perr)
		return rehydrateErrored
	}
	if ok {
		return rehydrateCopied
	}
	// Race condition: another process hydrated this file between our
	// Exists check and our Put. Count as skipped.
	return rehydrateSkipped
}

// rehydratePaths enumerates the schema-aligned file paths covering
// [from, to]. Each file holds LedgersPerFile ledgers; we step by
// that amount to visit each path once. Paths are sorted ascending.
func rehydratePaths(schema datastore.DataStoreSchema, from, to uint32) []string {
	if schema.LedgersPerFile == 0 {
		// Defensive: LoadSchema should populate this. Falling back to
		// 1 ledger per file matches Galexie's default and prevents an
		// infinite loop if the schema is malformed.
		schema.LedgersPerFile = 1
	}
	// Align `from` down to its file's start boundary so a -from in
	// mid-file still rehydrates the file containing it.
	start := schema.GetSequenceNumberStartBoundary(from)
	estFiles := int((to-start)/schema.LedgersPerFile) + 1
	seen := make(map[string]struct{}, estFiles)
	out := make([]string, 0, estFiles)
	for seq := start; seq <= to; seq += schema.LedgersPerFile {
		path := schema.GetObjectKeyFromSequenceNumber(seq)
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

func parseRehydrateFlags(args []string) (rehydrateOpts, error) {
	fs := flag.NewFlagSet("rehydrate-galexie-archive", flag.ContinueOnError)
	var (
		opts rehydrateOpts
		from int64
		to   int64
	)
	fs.StringVar(&opts.cfgPath, "config", "/etc/stellarindex.toml", "Path to stellarindex.toml")
	fs.Int64Var(&from, "from", 0, "First ledger sequence to rehydrate (inclusive)")
	fs.Int64Var(&to, "to", 0, "Last ledger sequence to rehydrate (inclusive)")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "List would-copy files without writing to hot")
	if err := fs.Parse(args); err != nil {
		return rehydrateOpts{}, err
	}
	if from < 0 || from > int64(^uint32(0)) {
		return rehydrateOpts{}, fmt.Errorf("-from out of uint32 range: %d", from)
	}
	if to < 0 || to > int64(^uint32(0)) {
		return rehydrateOpts{}, fmt.Errorf("-to out of uint32 range: %d", to)
	}
	opts.from = uint32(from)
	opts.to = uint32(to)
	return opts, nil
}

// byteWriterTo adapts a []byte to the io.WriterTo interface required
// by datastore.DataStore.PutFile. The SDK's S3 backend buffers the
// full body anyway; passing a WriterTo is the interface shape, not
// a streaming-vs-buffered choice.
type byteWriterTo []byte

func (b byteWriterTo) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b)
	return int64(n), err
}
