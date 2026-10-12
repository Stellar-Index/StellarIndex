package ledgerstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	"github.com/stellar/go-stellar-sdk/support/compressxdr"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
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

// TestWalkDataStore_ClosesStoreOnEveryReturnPath guards the close path: the SDK's BufferedStorageBackend.Close never
// closes the datastore, so walkDataStore must close it on every return path. A range failing validateRange (after
// schema load and backend construction succeed) must still close the store exactly once via the top-level defer.
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

	// To < From fails validateRange, which runs strictly after schema load and backend
	// construction, so this exercises a return path past the early manual store.Close() calls.
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

// TestWalkDataStore_LiveRetryBudgetRidesOutADatastoreOutage drives the SDK's real fetch-worker retry loop over a
// datastore failing 8 times, then succeeding. 8 exceeds the SDK's RetryLimit of 5, so a policy overriding only
// RetryWait would cancel with "maximum retries exceeded", a process exit in the indexer.
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

// faultingStore returns a transient (non-NotExist) error for the first `failures` GetFile calls
// on `key`, then delegates; everything else passes through, so only the fetch-worker retry loop
// is exercised.
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

// TestValidateRange pins that a bounded range of exactly one ledger (To == From) is valid
// (ledgerbackend.SingleLedgerRange). A `To() <= From()` check would make ch-live-catchup's
// tip-extend fail whenever it fires one ledger behind the tip.
func TestValidateRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		r       ledgerbackend.Range
		wantErr bool
	}{
		{"bounded multi-ledger", ledgerbackend.BoundedRange(10, 20), false},
		{"bounded single ledger (To == From)", ledgerbackend.SingleLedgerRange(62984354), false},
		{"bounded inverted (To < From)", ledgerbackend.BoundedRange(20, 10), true},
		{"unbounded", ledgerbackend.UnboundedRange(10), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateRange(tc.r)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateRange(%v) err = %v, wantErr %v", tc.r, err, tc.wantErr)
			}
		})
	}
}

// A MinIO blip must not tear the live tail down. The SDK fetch worker spends a retry attempt on every non-NotExist
// error but none on "tip not written yet", and both sleep RetryWait, so shortening it for tip latency shrinks fault
// tolerance (5 x 500ms = 2.5s). Tolerance is a time budget with attempts derived from the wait; these tests pin that.

func TestLiveRetryLimit_DerivesAttemptsFromBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wait   time.Duration
		budget time.Duration
		want   uint32
	}{
		{
			// The production pairing: 500ms re-check for tip latency,
			// 5 minutes of fault tolerance.
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

// TestApplyLiveRetryPolicy_DerivesLimitFromTheOverriddenWait pins the ordering trap: the attempt
// count must derive from the wait the worker will actually sleep, not the SDK's 30s default, or
// tolerance is 60x shorter than configured.
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

// ─── fixture helpers (internal-package copies; the external test
// package has its own, and Go will not share across packages) ────────

// TestParseTrailingMissingSeq covers the SDK error-message parse, the only reliable identifier of a trailing-edge
// missing file since the SDK (ledgerbackend.ledger_buffer) uses pkg/errors.Wrapf with no typed sentinel.
func TestParseTrailingMissingSeq(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		err     error
		wantSeq uint32
		wantOK  bool
	}{
		{
			name:    "nil error",
			err:     nil,
			wantSeq: 0,
			wantOK:  false,
		},
		{
			name:    "unrelated error",
			err:     errors.New("postgres: connection refused"),
			wantSeq: 0,
			wantOK:  false,
		},
		{
			name: "bare SDK wrap from real r1 incident",
			err: errors.New(
				"ledger object containing sequence 62642880 is missing: " +
					"unable to retrieve file: " +
					"FC44EBFF--62592000-62655999/FC44253F--62642880.xdr.zst: " +
					"file does not exist"),
			wantSeq: 62642880,
			wantOK:  true,
		},
		{
			name: "wrapped through streamTiered prefix",
			err: errors.New("ledgerstream: get ledger 62642880: " +
				"ledger object containing sequence 62642880 is missing: ..."),
			wantSeq: 62642880,
			wantOK:  true,
		},
		{
			name: "wrapped through backfill chunk prefix",
			err: errors.New("backfill: chunk 11 [61639496, 62656054]: stream: " +
				"error getting ledger, failed getting next ledger batch from queue: " +
				"ledger object containing sequence 62642880 is missing: ..."),
			wantSeq: 62642880,
			wantOK:  true,
		},
		{
			name: "max retries variant (different SDK wrap — does NOT match)",
			err: errors.New(
				"maximum retries exceeded for downloading object containing sequence 12345"),
			wantSeq: 0,
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotSeq, gotOK := parseTrailingMissingSeq(tc.err)
			if gotSeq != tc.wantSeq || gotOK != tc.wantOK {
				t.Errorf("parseTrailingMissingSeq() = (%d, %v), want (%d, %v)",
					gotSeq, gotOK, tc.wantSeq, tc.wantOK)
			}
		})
	}
}

// TestMaybeTolerateTrailingMissing_CountsOnlyTolerated pins that a
// tolerated miss is visible as a counter, and that an error passed
// through is not counted. Not parallel: the counter is process-global.
func TestMaybeTolerateTrailingMissing_CountsOnlyTolerated(t *testing.T) {
	missing := func(seq string) error {
		return errors.New("ledger object containing sequence " + seq + " is missing: file does not exist")
	}
	tipAt := func(tip uint32) func() (uint32, error) {
		return func() (uint32, error) { return tip, nil }
	}
	tol := Config{TolerateTrailingMissing: true, TrailingMissingWindow: 10}
	cases := []struct {
		name      string
		cfg       Config
		err       error
		tip       func() (uint32, error)
		wantNil   bool
		wantDelta float64
	}{
		{"tolerated trailing miss", tol, missing("195"), tipAt(194), true, 1},
		{"miss beyond window", tol, missing("150"), tipAt(194), false, 0},
		{"flag off", Config{}, missing("195"), tipAt(194), false, 0},
		{"unrelated error", Config{TolerateTrailingMissing: true}, errors.New("boom"), tipAt(194), false, 0},
		{"miss near a chunk's To but far below the store tip", tol, missing("195"), tipAt(5_000), false, 0},
		{"miss within the window of the store tip", tol, missing("195"), tipAt(205), true, 1},
		{"store tip unresolvable", tol, missing("195"), func() (uint32, error) { return 0, errors.New("list denied") }, false, 0},
	}
	for _, tc := range cases {
		before := testutil.ToFloat64(obs.LedgerstreamTrailingMissingToleratedTotal)
		got := maybeTolerateTrailingMissing(tc.cfg, 100, 200, 5, tc.err, tc.tip)
		delta := testutil.ToFloat64(obs.LedgerstreamTrailingMissingToleratedTotal) - before
		if (got == nil) != tc.wantNil {
			t.Errorf("%s: err = %v, wantNil %v", tc.name, got, tc.wantNil)
		}
		if delta != tc.wantDelta {
			t.Errorf("%s: tolerated counter delta = %v, want %v", tc.name, delta, tc.wantDelta)
		}
	}
}
