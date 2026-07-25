package ledgerstream

import (
	"context"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// closeTrackingStore wraps a fsStore datastore.DataStore, counting Close
// calls so a test can assert walkDataStore's cleanup contract without
// needing an SDK-internal hook.
type closeTrackingStore struct {
	datastore.DataStore
	closes int
}

func (c *closeTrackingStore) Close() error {
	c.closes++
	return c.DataStore.Close()
}

// TestWalkDataStore_ClosesStoreOnEveryReturnPath is the regression
// test for AGT-08 (audit-2026-07-23, low): walkDataStore's docstring
// claimed backend.Close() closed the underlying store "thereby" — it
// does not (the SDK's BufferedStorageBackend.Close only closes its
// own internal ledger buffer, never the datastore.DataStore it was
// built over). Every return path past the two early
// schema-load/backend-construction failures — including a normal
// validateRange rejection, and every walk-loop exit — leaked the
// store's open connections/file handles.
//
// Drives a range that fails validateRange, which runs AFTER schema
// load + backend construction have already succeeded — i.e. past the
// two pre-existing manual store.Close() calls this test does NOT
// exercise — and confirms the store is still closed exactly once via
// the new top-of-function defer.
func TestWalkDataStore_ClosesStoreOnEveryReturnPath(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fsStore, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}

	dsCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": tmp},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, fsStore, dsCfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}

	tracked := &closeTrackingStore{DataStore: fsStore}
	cfg := Config{DataStore: dsCfg}

	// An invalid bounded range (To < From) fails validateRange — a
	// check that runs strictly AFTER schema-load + backend
	// construction succeed, so reaching it proves those two steps
	// worked and the walk is exercising a return path beyond the
	// pre-existing manual store.Close() calls.
	badRange := ledgerbackend.BoundedRange(10, 5)

	err = walkDataStore(ctx, cfg, tracked, badRange,
		ingest.DefaultBufferedStorageBackendConfig(dsCfg.Schema.LedgersPerFile),
		func(xdr.LedgerCloseMeta) error { return nil })
	if err == nil {
		t.Fatal("expected validateRange to reject To < From")
	}
	if tracked.closes != 1 {
		t.Errorf("store.Close() called %d times, want exactly 1 — walkDataStore must close the store on every return path, not just the two early schema/backend-construction failures", tracked.closes)
	}
}
