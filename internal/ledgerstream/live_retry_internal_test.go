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
	"github.com/stellar/go-stellar-sdk/support/compressxdr"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ─── #371 F3: a MinIO blip must not tear the live tail down ──────────
//
// The SDK's fetch worker consumes a retry ATTEMPT for every non-
// NotExist datastore error and gives up after RetryLimit of them, but
// it consumes NOTHING for "the tip isn't written yet" (os.ErrNotExist
// on an unbounded range). Both sleep the same RetryWait. So dropping
// RetryWait to 500ms for tip latency silently cut fault tolerance to
// 5 × 500ms = 2.5s, and a MinIO restart exited the indexer over and
// over until systemd's StartLimit parked the unit in `failed`.
//
// The fix expresses tolerance as a TIME budget and derives the attempt
// count from the wait actually in force. These tests pin both halves:
// the arithmetic, and the end-to-end behaviour against a datastore that
// faults N times and then succeeds.

func TestLiveRetryLimit_DerivesAttemptsFromBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wait   time.Duration
		budget time.Duration
		want   uint32
	}{
		{
			// The production pairing: 500ms re-check for tip latency,
			// 5 minutes of fault tolerance. Pre-fix this was 5.
			name: "production_live_tail", wait: 500 * time.Millisecond,
			budget: 5 * time.Minute, want: 600,
		},
		{
			// Rounds UP: the budget is a floor, never a ceiling.
			name: "rounds_up", wait: 700 * time.Millisecond,
			budget: 2 * time.Second, want: 3,
		},
		{
			// A budget shorter than one wait still buys one attempt —
			// never zero, which would mean "give up immediately".
			name: "sub_wait_budget", wait: time.Second,
			budget: 10 * time.Millisecond, want: 1,
		},
		// Zero on either input means "leave the SDK default alone".
		{name: "no_budget", wait: time.Second, budget: 0, want: 0},
		{name: "no_wait", wait: 0, budget: time.Minute, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := liveRetryLimit(tc.wait, tc.budget); got != tc.want {
				t.Errorf("liveRetryLimit(%v, %v) = %d, want %d", tc.wait, tc.budget, got, tc.want)
			}
		})
	}
}

// TestApplyLiveRetryPolicy_DerivesLimitFromTheOverriddenWait pins the
// ordering trap: the attempt count must be derived from the wait the
// worker will ACTUALLY sleep, not from the SDK's 30s default. Getting
// this backwards yields a tolerance 60× shorter than configured — the
// same coupling defect in a new costume.
func TestApplyLiveRetryPolicy_DerivesLimitFromTheOverriddenWait(t *testing.T) {
	buffered := ingest.DefaultBufferedStorageBackendConfig(1)
	if buffered.RetryWait != 30*time.Second || buffered.RetryLimit != 5 {
		t.Fatalf("SDK defaults moved (RetryWait=%v RetryLimit=%d) — re-derive this test",
			buffered.RetryWait, buffered.RetryLimit)
	}

	applyLiveRetryPolicy(Config{
		LiveRetryWait:   500 * time.Millisecond,
		LiveRetryBudget: 5 * time.Minute,
	}, &buffered)

	if buffered.RetryWait != 500*time.Millisecond {
		t.Errorf("RetryWait = %v, want 500ms (tip-latency override)", buffered.RetryWait)
	}
	if buffered.RetryLimit != 600 {
		t.Errorf("RetryLimit = %d, want 600 (5min / 500ms)", buffered.RetryLimit)
	}
	tolerated := time.Duration(buffered.RetryLimit) * buffered.RetryWait
	if tolerated < 5*time.Minute {
		t.Errorf("fault tolerance = %v, want >= 5m — a MinIO restart must not exit the indexer", tolerated)
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

// TestWalkDataStore_LiveRetryBudgetRidesOutADatastoreOutage drives the
// SDK's real fetch-worker retry loop over a scripted datastore that
// fails 8 times and then succeeds. 8 exceeds the SDK's RetryLimit of 5,
// so with the pre-fix policy (RetryWait overridden, RetryLimit left at
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

// ─── fixture helpers (internal-package copies; the external test
// package has its own, and Go will not share across packages) ────────

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
