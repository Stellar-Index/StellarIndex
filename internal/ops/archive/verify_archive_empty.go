package archive

import (
	"context"
	"fmt"
	"os"

	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// emptyArchiveMaxLiveFirst is the highest first-live ledger for which an empty archive is
// correct: the archive's expected range [1, first-live − 1] is then below one file, so
// there is nothing to write or verify. Mirrors EMPTY_ARCHIVE_MAX_LIVE_FIRST in
// galexie-archive-tip-lag.sh and galexie-archive-contiguity.sh.
const emptyArchiveMaxLiveFirst = 64

// archiveEmptyByDesign reports whether the archive bucket is empty because its expected
// range is empty (a test net reset and re-ingested from ledger 2). It is true only when the
// archive holds no object at all — no ledger file and no manifest — AND the oldest live
// ledger is at or below emptyArchiveMaxLiveFirst. Every error answers false, so an
// unreachable bucket, a missing live manifest or an unmeasurable live start still fails
// the walk exactly as before.
func archiveEmptyByDesign(ctx context.Context, archive, live datastore.DataStore, liveCfg datastore.DataStoreConfig) (bool, error) {
	keys, err := archive.ListFilePaths(ctx, datastore.ListFileOptions{Limit: 1})
	if err != nil {
		return false, fmt.Errorf("list archive bucket: %w", err)
	}
	if len(keys) > 0 {
		return false, nil
	}
	schema, err := datastore.LoadSchema(ctx, live, liveCfg)
	if err != nil {
		return false, fmt.Errorf("load live schema: %w", err)
	}
	first, err := datastore.FindOldestLedgerSequence(ctx, live, schema)
	if err != nil {
		return false, fmt.Errorf("find oldest live ledger: %w", err)
	}
	return first <= emptyArchiveMaxLiveFirst, nil
}

// archiveBucketEmptyByDesign opens the archive and live buckets named by cfg and applies
// archiveEmptyByDesign. It only applies to the configured archive bucket: with no live
// bucket, or when the walk is aimed elsewhere, it answers false.
func archiveBucketEmptyByDesign(ctx context.Context, cfg config.Config, bucket string) bool {
	archiveName, liveName := cfg.Storage.S3BucketArchive, cfg.Storage.S3BucketLive
	if archiveName == "" || liveName == "" || bucket != archiveName {
		return false
	}
	archiveCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, archiveName, 1).DataStore
	liveCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, liveName, 1).DataStore
	archive, err := datastore.NewDataStore(ctx, archiveCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-archive: empty-archive probe: open archive: %v\n", err)
		return false
	}
	defer func() { _ = archive.Close() }()
	live, err := datastore.NewDataStore(ctx, liveCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-archive: empty-archive probe: open live: %v\n", err)
		return false
	}
	defer func() { _ = live.Close() }()
	empty, err := archiveEmptyByDesign(ctx, archive, live, liveCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-archive: empty-archive probe: %v\n", err)
		return false
	}
	return empty
}
