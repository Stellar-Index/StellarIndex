package ledgerstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TieredDataStore wraps a hot + cold [datastore.DataStore] in a
// fallback chain. Reads try the hot store first; on a not-found
// error (and only that — not transient errors) they fall through
// to the cold store. Writes always target the hot store; the cold
// store is treated as read-only.
//
// Per ADR-0027 the production hot tier is local galexie-archive
// (MinIO on r1) and the cold tier is `aws-public-blockchain` S3
// (the Open Data Sponsorship bucket — the same source R2 reads
// per ADR-0016). The cold path is read-only; PutFile +
// PutFileIfNotExists always target hot.
//
// Fail-loud-not-silent: transient errors from the hot store
// propagate immediately. A misconfigured hot endpoint surfaces
// as the operator's actual problem rather than being masked by
// a slow cold fallback that succeeds for every read.
//
// Metrics (always emitted — package-level, registered once at boot):
//
//   - stellarindex_ledgerstream_tier_read_total
//     {outcome="hot"|"cold"|"both_missing"} (obs.LedgerstreamTierReadTotal)
//   - stellarindex_ledgerstream_cold_read_duration_seconds
//     {outcome="ok"|"miss"|"error"} (obs.LedgerstreamColdReadDurationSeconds)
//
// These are obs package-level metrics registered unconditionally at
// process boot — NOT gated on a per-instance registry (W5-mon-3). The
// production ledgerstream.Config leaves Registry nil (the SDK's
// BufferedStorageBackend registration panics across the
// archive→live→catch-up Stream calls), so a per-instance metric here
// was nil in production and the `both_missing` page could never fire.
// Sourcing them from obs decouples this observability from the SDK's
// registry constraint.
//
// Operators chart `cold` rate as a proxy for "is the trim window
// correctly sized, or am I paying cross-Atlantic latency for
// ranges that should be hot?". A `cold` rate spike on live ingest
// = trim window too tight; cold rate on backfill is expected.
type TieredDataStore struct {
	hot  datastore.DataStore
	cold datastore.DataStore
}

// NewTieredDataStore builds a TieredDataStore wrapping hot + cold.
//
// Metrics are the obs package-level [obs.LedgerstreamTierReadTotal] /
// [obs.LedgerstreamColdReadDurationSeconds], registered once at process
// boot — so there is no per-instance registry to wire and no typed-nil
// footgun (W5-mon-3). Repeated construction across the
// archive→live→catch-up Stream calls is safe precisely because the
// metrics are NOT re-registered here.
func NewTieredDataStore(hot, cold datastore.DataStore) *TieredDataStore {
	return &TieredDataStore{hot: hot, cold: cold}
}

// IsNotFound returns true when err is a missing-key error from any
// datastore backend. All three backends normalize to os.ErrNotExist:
// the filesystem store natively, and the SDK's S3DataStore converts
// AWS typed errors (types.NoSuchKey from GetObject, types.NotFound
// from HeadObject) via errors.As before returning os.ErrNotExist
// (go-stellar-sdk support/datastore/s3.go, isNotFoundError) — so no
// string matching is needed or performed (C2-064: the previous
// "NoSuchKey" string arm was dead code and its comment inverted the
// SDK's actual behavior). Manifest-empty errors
// ([datastore.ErrNoLedgerFiles] etc.) also count as not-found.
// Transient errors (network timeouts, auth failures, throttling)
// DO NOT match and so propagate up rather than falsely triggering
// a cold fallback.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return errors.Is(err, datastore.ErrNoLedgerFiles) || errors.Is(err, datastore.ErrNoValidLedgerFiles)
}

// GetFile reads from hot; on IsNotFound, reads from cold.
// Transient errors from hot propagate without trying cold.
//
// The int64 is the object size in bytes (go-stellar-sdk v0.6 added it to the
// datastore.DataStore.GetFile contract); we thread the underlying store's
// value through unchanged.
func (t *TieredDataStore) GetFile(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	rc, size, err := t.hot.GetFile(ctx, path)
	if err == nil {
		t.observeHot()
		return rc, size, nil
	}
	if !IsNotFound(err) {
		// Transient hot-side error — fail loud rather than mask.
		return nil, 0, fmt.Errorf("tiered: hot GetFile %q: %w", path, err)
	}
	return t.coldGetFile(ctx, path)
}

func (t *TieredDataStore) coldGetFile(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	start := time.Now()
	rc, size, cerr := t.cold.GetFile(ctx, path)
	elapsed := time.Since(start).Seconds()
	switch {
	case cerr == nil:
		t.observeCold("ok", elapsed)
		t.bumpTotal("cold")
		return rc, size, nil
	case IsNotFound(cerr):
		t.observeCold("miss", elapsed)
		t.bumpTotal("both_missing")
		return nil, 0, cerr
	default:
		t.observeCold("error", elapsed)
		return nil, 0, fmt.Errorf("tiered: cold GetFile %q: %w", path, cerr)
	}
}

// GetFileMetadata: hot first, cold on not-found.
func (t *TieredDataStore) GetFileMetadata(ctx context.Context, path string) (map[string]string, error) {
	md, err := t.hot.GetFileMetadata(ctx, path)
	if err == nil {
		return md, nil
	}
	if !IsNotFound(err) {
		return nil, err
	}
	return t.cold.GetFileMetadata(ctx, path)
}

// GetFileLastModified: hot first, cold on not-found.
func (t *TieredDataStore) GetFileLastModified(ctx context.Context, filePath string) (time.Time, error) {
	tm, err := t.hot.GetFileLastModified(ctx, filePath)
	if err == nil {
		return tm, nil
	}
	if !IsNotFound(err) {
		return time.Time{}, err
	}
	return t.cold.GetFileLastModified(ctx, filePath)
}

// Exists returns true if either tier has the object. Hot is
// preferred (sub-ms intra-host); cold is consulted only when hot
// returns false.
func (t *TieredDataStore) Exists(ctx context.Context, path string) (bool, error) {
	ok, err := t.hot.Exists(ctx, path)
	if err != nil {
		return false, err
	}
	if ok {
		return true, nil
	}
	return t.cold.Exists(ctx, path)
}

// Size: hot first, cold on not-found.
func (t *TieredDataStore) Size(ctx context.Context, path string) (int64, error) {
	n, err := t.hot.Size(ctx, path)
	if err == nil {
		return n, nil
	}
	if !IsNotFound(err) {
		return 0, err
	}
	return t.cold.Size(ctx, path)
}

// ListFilePaths returns the union of hot + cold listings. Cold
// paths that are also in hot are deduplicated — hot wins (the
// fresh path). This is the right shape for backfills that span
// the hot/cold boundary: a single call returns every available
// partition.
//
// Order is hot-first then cold-only; callers that need a sorted
// view sort downstream.
func (t *TieredDataStore) ListFilePaths(ctx context.Context, options datastore.ListFileOptions) ([]string, error) {
	hot, err := t.hot.ListFilePaths(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("tiered: hot ListFilePaths: %w", err)
	}
	cold, err := t.cold.ListFilePaths(ctx, options)
	if err != nil {
		// Cold-side list failures are non-fatal for ranges already
		// in hot; return the hot list and a context-wrapped error
		// so callers can decide whether to retry.
		return hot, fmt.Errorf("tiered: cold ListFilePaths (returning hot-only list): %w", err)
	}
	seen := make(map[string]struct{}, len(hot))
	out := make([]string, 0, len(hot)+len(cold))
	for _, p := range hot {
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, p := range cold {
		if _, dup := seen[p]; dup {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// PutFile always targets the hot tier. The cold tier (AWS public
// bucket) is treated as read-only.
func (t *TieredDataStore) PutFile(ctx context.Context, path string, in io.WriterTo, metaData map[string]string) error {
	return t.hot.PutFile(ctx, path, in, metaData)
}

// PutFileIfNotExists always targets the hot tier.
func (t *TieredDataStore) PutFileIfNotExists(ctx context.Context, path string, in io.WriterTo, metaData map[string]string) (bool, error) {
	return t.hot.PutFileIfNotExists(ctx, path, in, metaData)
}

// Close closes both tiers. A hot-side Close error masks any
// cold-side Close error; callers that need both should call them
// individually before wrapping in a TieredDataStore.
func (t *TieredDataStore) Close() error {
	hotErr := t.hot.Close()
	coldErr := t.cold.Close()
	if hotErr != nil {
		return hotErr
	}
	return coldErr
}

func (t *TieredDataStore) observeHot() {
	obs.LedgerstreamTierReadTotal.WithLabelValues("hot").Inc()
}

func (t *TieredDataStore) bumpTotal(outcome string) {
	obs.LedgerstreamTierReadTotal.WithLabelValues(outcome).Inc()
}

func (t *TieredDataStore) observeCold(outcome string, seconds float64) {
	obs.LedgerstreamColdReadDurationSeconds.WithLabelValues(outcome).Observe(seconds)
}

// Compile-time assertion that TieredDataStore satisfies the SDK
// interface. If the SDK adds a method, this build break is the
// signal to extend the wrapper.
var _ datastore.DataStore = (*TieredDataStore)(nil)
