package archive

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
)

// stellarindex-ops rehydrate-galexie-archive is a non-destructive re-copy of LCM files from the cold tier back into the hot galexie-archive bucket for a ledger range (ADR-0027).
// Idempotent via PutFileIfNotExists. It steps by LedgersPerFile and takes paths from DataStoreSchema.GetObjectKeyFromSequenceNumber.

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
	opsutil.PrintWriteBanner(!opts.dryRun)

	// LoadWithEnv (not bare Load) so the STELLARINDEX_* secret overrides take effect, as in every other binary; bare Load would leave placeholder TOML secrets.
	cfg, err := config.LoadWithEnv(opts.cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.Storage.ColdTieringEnabled() {
		return fmt.Errorf("cold tier not configured — set storage.s3_cold_bucket_archive in %s before rehydrating", opts.cfgPath)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	rootCtx, cancel := opsutil.SignalContext()
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

	// pipeline.NewColdDataStore, NOT datastore.NewDataStore: the SDK's ambient AWS credential chain carries local MinIO's keys on r1, and presenting them to real AWS fails every cold read, breaking the disaster-recovery path.
	cold, err := pipeline.NewColdDataStore(rootCtx, cfg.Storage)
	if err != nil {
		return fmt.Errorf("cold datastore: %w", err)
	}
	defer func() { _ = cold.Close() }()

	// Both tiers' schemas must agree before any path is built: keys are derived from hot's schema only, so a differently laid-out cold tier 404s on every key, indistinguishable from an archive gap. Mirrors the guard in internal/ledgerstream/ledgerstream.go.
	schema, err := loadAndVerifyRehydrateSchemas(rootCtx, hot, cold, cfg.Storage)
	if err != nil {
		return err
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
	return rehydrateExitError(len(paths), missing, errs)
}

// rehydrateExitError decides whether a finished run exits non-zero. `missing` alone exits 0 (a real gap or spot-check outcome), but every file missing with zero copied/skipped/errored is the mismatched-schema symptom, not a sparse archive, and must not report success.
func rehydrateExitError(totalPaths, missing, errs int) error {
	if errs > 0 {
		return fmt.Errorf("rehydrate finished with %d errors", errs)
	}
	if totalPaths > 0 && missing == totalPaths {
		return fmt.Errorf("rehydrate found ALL %d files missing in cold — likely a cold-tier misconfiguration or mirror mismatch, not a genuine gap", missing)
	}
	return nil
}

// loadAndVerifyRehydrateSchemas loads hot's schema (it builds every key rehydratePaths generates) and cold's, hard-failing on any shape
// mismatch rather than letting hot-shaped keys 404 on a differently-partitioned cold tier.
func loadAndVerifyRehydrateSchemas(ctx context.Context, hot, cold datastore.DataStore, storage config.StorageConfig) (datastore.DataStoreSchema, error) {
	hotSchema, err := datastore.LoadSchema(ctx, hot, datastore.DataStoreConfig{
		Type: "S3",
		Params: map[string]string{
			"destination_bucket_path": storage.S3BucketArchive,
			"region":                  storage.S3Region,
			"endpoint_url":            storage.S3Endpoint,
		},
	})
	if err != nil {
		return datastore.DataStoreSchema{}, fmt.Errorf("load schema (hot): %w", err)
	}
	coldSchema, err := datastore.LoadSchema(ctx, cold, datastore.DataStoreConfig{
		Type: "S3",
		Params: map[string]string{
			"destination_bucket_path": storage.S3ColdBucketArchive,
			"region":                  storage.S3ColdRegion,
			"endpoint_url":            storage.S3ColdEndpoint,
		},
	})
	if err != nil {
		return datastore.DataStoreSchema{}, fmt.Errorf("load schema (cold): %w", err)
	}
	if hotSchema.LedgersPerFile != coldSchema.LedgersPerFile ||
		hotSchema.FilesPerPartition != coldSchema.FilesPerPartition ||
		hotSchema.FileExtension != coldSchema.FileExtension {
		return datastore.DataStoreSchema{}, fmt.Errorf(
			"cold datastore schema (ledgers_per_file=%d files_per_partition=%d file_extension=%q) differs from hot's (ledgers_per_file=%d files_per_partition=%d file_extension=%q) — rehydrate would issue hot-shaped object keys to a differently-partitioned cold store and always miss",
			coldSchema.LedgersPerFile, coldSchema.FilesPerPartition, coldSchema.FileExtension,
			hotSchema.LedgersPerFile, hotSchema.FilesPerPartition, hotSchema.FileExtension)
	}
	return hotSchema, nil
}

// rehydrateStore is the slice of datastore.DataStore rehydrateFiles calls, so the copy loop (including not-found vs transient cold errors) is testable with a fake.
type rehydrateStore interface {
	Exists(ctx context.Context, path string) (bool, error)
	GetFile(ctx context.Context, path string) (io.ReadCloser, int64, error)
	PutFileIfNotExists(ctx context.Context, path string, in io.WriterTo, metaData map[string]string) (bool, error)
}

// rehydrateOutcome buckets one file's result as a small enum so rehydrateFiles' loop body is a flat switch (cognitive-complexity budget).
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

// copyColdToHot copies one path confirmed absent from hot. cold.Exists is checked FIRST: only a definitive not-found reports rehydrateMissing; any Exists error, or a GetFile error after Exists confirmed presence, reports rehydrateErrored,
// so a transient cold-tier fault is not misreported as a data gap that exits 0.
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

// rehydratePaths enumerates the schema-aligned file paths covering [from, to], stepping by LedgersPerFile to visit each once; sorted
// ascending.
func rehydratePaths(schema datastore.DataStoreSchema, from, to uint32) []string {
	if schema.LedgersPerFile == 0 {
		// Defensive: LoadSchema should populate this. Falling back to
		// 1 ledger per file matches Galexie's default and prevents an
		// infinite loop if the schema is malformed.
		schema.LedgersPerFile = 1
	}
	if from > to {
		return nil
	}
	// Align `from` down to its file's start boundary so a -from in
	// mid-file still rehydrates the file containing it.
	start := schema.GetSequenceNumberStartBoundary(from)
	estFiles := int((to-start)/schema.LedgersPerFile) + 1
	seen := make(map[string]struct{}, estFiles)
	out := make([]string, 0, estFiles)
	for seq := start; ; seq += schema.LedgersPerFile {
		path := schema.GetObjectKeyFromSequenceNumber(seq)
		if _, dup := seen[path]; !dup {
			seen[path] = struct{}{}
			out = append(out, path)
		}
		// Compare the remaining distance, not seq+step, so a -to near
		// MaxUint32 cannot wrap seq back below it and loop forever.
		if to-seq < schema.LedgersPerFile {
			return out
		}
	}
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
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return rehydrateOpts{}, err
	}
	opts.dryRun = gate.DryRun()
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

// byteWriterTo adapts a []byte to the io.WriterTo datastore.DataStore.PutFile requires; the SDK's S3 backend buffers the full body anyway,
// so this is interface shape, not streaming.
type byteWriterTo []byte

func (b byteWriterTo) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b)
	return int64(n), err
}
