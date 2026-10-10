package ledgerstream

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar/go-stellar-sdk/support/compressxdr"
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

// TestWalkDataStore_ClosesStoreOnEveryReturnPath guards the close path: walkDataStore's docstring
// must not claim backend.Close() closed the underlying store "thereby" — it
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

// TestWalkDataStore_LiveRetryBudgetRidesOutADatastoreOutage drives the
// SDK's real fetch-worker retry loop over a scripted datastore that
// fails 8 times and then succeeds. 8 exceeds the SDK's RetryLimit of 5,
// so with a policy that only overrides RetryWait (RetryWait overridden, RetryLimit left at
// the default) the backend cancels with "maximum retries exceeded" and
// the walk errors — which in the indexer is a process exit.
//
// The wait/budget are scaled down (10ms / 2s) so the test is fast; the
// POLICY under test is the same function production calls, and the
// production pairing is pinned separately above.
func TestWalkDataStore_LiveRetryBudgetRidesOutADatastoreOutage(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	putLedger(t, ctx, fsStore, dsCfg.Schema, 5)
	putLedger(t, ctx, fsStore, dsCfg.Schema, 6)

	const injectedFaults = 8
	faulty := &faultingStore{
		DataStore: fsStore,
		key:       dsCfg.Schema.GetObjectKeyFromSequenceNumber(5),
		failures:  injectedFaults,
	}

	cfg := Config{
		DataStore:       dsCfg,
		LiveRetryWait:   10 * time.Millisecond,
		LiveRetryBudget: 2 * time.Second, // → 200 attempts, well past 8
	}
	buffered := ingest.DefaultBufferedStorageBackendConfig(dsCfg.Schema.LedgersPerFile)
	applyLiveRetryPolicy(cfg, &buffered)

	var got []uint32
	err = walkDataStore(ctx, cfg, faulty, ledgerbackend.BoundedRange(5, 6), buffered,
		func(lcm xdr.LedgerCloseMeta) error {
			got = append(got, lcm.LedgerSequence())
			return nil
		})
	if err != nil {
		if strings.Contains(err.Error(), "maximum retries exceeded") {
			t.Fatalf("the stream gave up after the SDK's default 5 attempts — the retry "+
				"budget was not applied: %v", err)
		}
		t.Fatalf("walkDataStore: %v", err)
	}
	if len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Errorf("delivered %v, want [5 6]", got)
	}
	if n := faulty.faults(); n != injectedFaults {
		t.Errorf("injected faults observed = %d, want %d — the test must actually "+
			"exercise the retry loop, not pass because nothing failed", n, injectedFaults)
	}
}

// faultingStore returns a transient (non-NotExist) error for the first
// `failures` GetFile calls against `key`, then delegates. Everything
// else — the manifest LoadSchema reads, other ledger objects — passes
// straight through, so the test exercises exactly the fetch-worker
// retry loop and nothing else.
type faultingStore struct {
	datastore.DataStore

	mu       sync.Mutex
	key      string
	failures int
	attempts int
}

func (f *faultingStore) GetFile(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	if path == f.key && f.failures > 0 {
		f.failures--
		f.attempts++
		f.mu.Unlock()
		// What a MinIO that is mid-restart actually returns.
		return nil, 0, syscall.ECONNREFUSED
	}
	f.mu.Unlock()
	return f.DataStore.GetFile(ctx, path)
}

func (f *faultingStore) faults() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func putLedger(t *testing.T, ctx context.Context, store datastore.DataStore, schema datastore.DataStoreSchema, seq uint32) {
	t.Helper()
	batch := xdr.LedgerCloseMetaBatch{
		StartSequence: xdr.Uint32(seq),
		EndSequence:   xdr.Uint32(seq),
		LedgerCloseMetas: []xdr.LedgerCloseMeta{{
			V: 1,
			V1: &xdr.LedgerCloseMetaV1{
				LedgerHeader: xdr.LedgerHeaderHistoryEntry{
					Header: xdr.LedgerHeader{LedgerSeq: xdr.Uint32(seq)},
				},
				TxSet: xdr.GeneralizedTransactionSet{V: 1, V1TxSet: &xdr.TransactionSetV1{}},
			},
		}},
	}
	var buf bytes.Buffer
	if _, err := compressxdr.NewXDREncoder(compressxdr.DefaultCompressor, batch).WriteTo(&buf); err != nil {
		t.Fatalf("encode batch %d: %v", seq, err)
	}
	key := schema.GetObjectKeyFromSequenceNumber(seq)
	if err := store.PutFile(ctx, key, writerTo(buf.Bytes()), nil); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

type writerTo []byte

func (b writerTo) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b)
	return int64(n), err
}
