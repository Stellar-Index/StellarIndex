package ledgerstream_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// #371 F3 (residual). Config.LiveRetryBudget must cover the datastore
// STARTUP, not only the SDK fetch worker's in-walk retries.
//
// The budget landed in 8992df8c is spent inside the SDK's ledger buffer,
// which only exists after datastore.NewDataStore + datastore.LoadSchema
// have both succeeded — and those run once, up front, with no retry
// (go-stellar-sdk ingest/producer.go:96-105; LoadSchema is a live LIST
// against the bucket). So a lake that is unreachable when the live tail
// STARTS was never covered: Stream returned in microseconds with
// "failed to retrieve datastore schema", the indexer exited, and because
// every restart then died in that same startup path in ~1s (rather than
// the ~5 min the unit file's StartLimit sizing assumes), sixty restarts
// fit inside StartLimitIntervalSec and the unit parked in `failed` —
// staying parked after MinIO recovered until a human intervened.
//
// A directory that does not exist yet is the hermetic stand-in for a
// MinIO that is down: FilesystemDataStore.ListFilePaths walks the base
// path, so LoadSchema fails exactly as it does against a refused socket.
// Creating it mid-test is the lake coming back.
func TestStream_liveTail_retriesWhenDataStoreIsDownAtStart(t *testing.T) {
	root := t.TempDir()
	// The datastore path deliberately does not exist yet.
	lakePath := filepath.Join(root, "galexie-live")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dsCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": lakePath},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}

	// Pre-encode the ledgers on the test goroutine — the fixture helpers
	// call t.Fatalf, which is illegal off it.
	type pending struct {
		key string
		buf []byte
	}
	var seed []pending
	for seq := uint32(5); seq <= 6; seq++ {
		seed = append(seed, pending{
			key: dsCfg.Schema.GetObjectKeyFromSequenceNumber(seq),
			buf: encodeBatch(t, xdr.LedgerCloseMetaBatch{
				StartSequence:    xdr.Uint32(seq),
				EndSequence:      xdr.Uint32(seq),
				LedgerCloseMetas: []xdr.LedgerCloseMeta{minimalLedgerCloseMeta(seq)},
			}),
		})
	}

	// Bring the "lake" up ~600ms in — i.e. after Stream has already
	// failed its first start and is inside the retry loop. Well short of
	// the 6s budget, and well beyond the ~85µs the un-fixed code took to
	// give up.
	bringUp := make(chan error, 1)
	go func() {
		time.Sleep(600 * time.Millisecond)
		bringUp <- func() error {
			store, err := datastore.NewFilesystemDataStoreWithPath(lakePath)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			if _, _, err := datastore.PublishConfig(ctx, store, dsCfg); err != nil {
				return err
			}
			for _, p := range seed {
				if err := store.PutFile(ctx, p.key, bufferWriterTo(p.buf), nil); err != nil {
					return err
				}
			}
			return nil
		}()
	}()

	before := testutil.ToFloat64(obs.LedgerstreamLiveStartRetriesTotal)

	stop := errors.New("stop")
	got := make([]uint32, 0, 2)
	start := time.Now()
	err := ledgerstream.Stream(ctx,
		ledgerstream.Config{
			DataStore:       dsCfg,
			LiveRetryWait:   50 * time.Millisecond,
			LiveRetryBudget: 6 * time.Second,
		},
		5, 0, /* unbounded — live tail */
		func(lcm xdr.LedgerCloseMeta) error {
			got = append(got, lcm.LedgerSequence())
			if lcm.LedgerSequence() == 6 {
				return stop
			}
			return nil
		},
	)
	elapsed := time.Since(start)

	if berr := <-bringUp; berr != nil {
		t.Fatalf("bring the lake up: %v", berr)
	}

	// The assertion that matters: the stream RODE OUT the outage in
	// process and went on to deliver the real ledgers. `stop` is the
	// caller's own sentinel from the callback, so reaching it proves the
	// walk actually ran — not merely that Stream returned late.
	if !errors.Is(err, stop) {
		t.Fatalf("live tail did not survive a datastore that was down at start: "+
			"err=%v after %v, delivered=%v (want ledgers [5 6] then the caller's stop)",
			err, elapsed, got)
	}
	if len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Errorf("delivered %v, want [5 6]", got)
	}
	// Pin the no-skip invariant explicitly: the retry re-issued the SAME
	// range, so the caller sees ledger 5 first. A resume that jumped
	// ahead would show up here as a missing 5.
	if len(got) > 0 && got[0] != 5 {
		t.Errorf("retry skipped ledger(s): first delivered ledger is %d, want 5", got[0])
	}

	// And the stall must not be silent while it happens.
	after := testutil.ToFloat64(obs.LedgerstreamLiveStartRetriesTotal)
	if after <= before {
		t.Errorf("stellarindex_ledgerstream_live_start_retries_total did not move: %v -> %v", before, after)
	}
}

// The budget is a CEILING, not an invitation to spin forever: a lake that
// never comes back must still surface the error, and must do so within
// roughly one budget rather than hanging until the caller's context dies.
// An unbounded reconnect would turn a visible outage into a silent freeze
// — strictly worse than the crash it replaces.
func TestStream_liveTail_startRetryIsBounded(t *testing.T) {
	root := t.TempDir()
	lakePath := filepath.Join(root, "never-arrives")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dsCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": lakePath},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}

	const budget = 700 * time.Millisecond
	start := time.Now()
	err := ledgerstream.Stream(ctx,
		ledgerstream.Config{
			DataStore:       dsCfg,
			LiveRetryWait:   50 * time.Millisecond,
			LiveRetryBudget: budget,
		},
		5, 0, /* unbounded — live tail */
		func(xdr.LedgerCloseMeta) error {
			t.Error("callback must not run: nothing was ever readable")
			return nil
		},
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Stream returned nil for a lake that never became readable")
	}
	// Loud, not silent: the operator-facing error still names the real
	// cause rather than a generic timeout.
	if got := err.Error(); !containsAll(got, "datastore", "schema") {
		t.Errorf("error lost the cause: %q", got)
	}
	if elapsed < budget {
		t.Errorf("gave up after %v, before the %v budget was spent", elapsed, budget)
	}
	// Generous ceiling: the deadline is fixed before the first
	// re-attempt, so total time is ~budget plus one final attempt.
	if elapsed > 10*budget {
		t.Errorf("retry was not bounded by the budget: ran %v for a %v budget", elapsed, budget)
	}
}

// A BOUNDED stream must be untouched by any of this: a missing object in
// a bounded range is a hard error whose handling belongs to the caller
// (see Config.LiveRetryBudget's godoc and maybeTolerateTrailingMissing).
// Retrying there would silently stretch every ops backfill's failure
// path — and the archive walk in StreamArchiveThenLive depends on failing
// fast rather than being retried behind the operator's back.
func TestStream_boundedRange_startFailureIsNotRetried(t *testing.T) {
	root := t.TempDir()
	lakePath := filepath.Join(root, "never-arrives")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dsCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": lakePath},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}

	start := time.Now()
	err := ledgerstream.Stream(ctx,
		ledgerstream.Config{
			DataStore:       dsCfg,
			LiveRetryWait:   50 * time.Millisecond,
			LiveRetryBudget: 10 * time.Second,
		},
		5, 7, /* BOUNDED */
		func(xdr.LedgerCloseMeta) error { return nil },
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("bounded Stream returned nil against an unreadable datastore")
	}
	if elapsed > 2*time.Second {
		t.Errorf("bounded range consumed the live-tail retry budget: returned after %v", elapsed)
	}
}

// The backoff window is the one place retryLiveStart blocks that a plain
// walk does not, so a SIGTERM landing in it must still read as a clean
// stop. cmd/stellarindex-indexer discriminates with
// `errors.Is(err, context.Canceled)`: anything else becomes fatalErr, so
// a deliberate `systemctl stop` during a lake outage would exit non-zero
// and mark the unit failed. The cause has to survive alongside it, or the
// journal loses the only line saying WHY the tail had not started.
func TestStream_liveTail_startRetryShutdownIsNotFatal(t *testing.T) {
	root := t.TempDir()
	lakePath := filepath.Join(root, "never-arrives")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dsCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": lakePath},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}

	// Cancel while the loop is asleep between re-attempts.
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	err := ledgerstream.Stream(ctx,
		ledgerstream.Config{
			DataStore: dsCfg,
			// Long wait so the cancellation reliably lands in the sleep.
			LiveRetryWait:   3 * time.Second,
			LiveRetryBudget: 30 * time.Second,
		},
		5, 0, /* unbounded — live tail */
		func(xdr.LedgerCloseMeta) error { return nil },
	)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown during the retry backoff is not reported as a clean stop: %v", err)
	}
	if got := err.Error(); !containsAll(got, "datastore", "schema") {
		t.Errorf("cancellation swallowed the cause: %q", got)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
