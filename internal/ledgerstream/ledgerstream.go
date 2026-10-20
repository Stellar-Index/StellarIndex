// Package ledgerstream reads Galexie-exported ledger-meta from an S3-compatible datastore (MinIO
// in production, Filesystem in tests) and yields one xdr.LedgerCloseMeta per ledger to a
// caller-supplied callback.
//
// It is the only production path into the ingest pipeline: every source decoder receives its
// events via this package, never via stellar-rpc (lint-imports.sh rule A/no-rpc-in-ingest). It
// wraps the SDK's ingest.ApplyLedgerMetadata as one import boundary and test seam, and centralizes
// logger and Prometheus wiring.
package ledgerstream

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	sdklog "github.com/stellar/go-stellar-sdk/support/log"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Config binds the SDK datastore configuration, BufferedStorageBackend tuning and optional observability; the zero-value DataStore is invalid and [Stream] errors rather than skipping.
type Config struct {
	// DataStore — required. Describes the Galexie output bucket
	// (S3/MinIO/GCS) or a filesystem directory for tests. In a
	// tiered deployment this is the **hot** tier (local
	// galexie-archive on r1).
	DataStore datastore.DataStoreConfig

	// ColdDataStore — optional. When Type is set, Stream wraps DataStore (hot) and this (cold) in a
	// [TieredDataStore] (ADR-0027), so LCMs absent from the local mirror fall back to the cold
	// upstream. Writes always target hot. The zero value disables tiering.
	ColdDataStore datastore.DataStoreConfig

	// ColdDataStoreFactory is optional: when non-nil, [Stream] calls it INSTEAD OF
	// datastore.NewDataStore to open the cold tier; ColdDataStore is still required (its Type is
	// the tiering opt-in and its passphrase/Schema drive LoadSchema). NewDataStore builds every S3
	// client from the ambient AWS credential chain, which carries the hot tier's MinIO keys;
	// presented to real AWS, every cold read fails and the tier silently degrades to hot-only.
	// This package cannot resolve the cold credential env names, hence the hook (production wires
	// pipeline.NewColdDataStore).
	ColdDataStoreFactory func(ctx context.Context) (datastore.DataStore, error)

	// Buffered — optional. If nil, Stream derives sensible defaults
	// from DataStore.Schema.LedgersPerFile via
	// ingest.DefaultBufferedStorageBackendConfig. Override only when
	// profiling has shown the defaults are wrong for your workload.
	Buffered *ledgerbackend.BufferedStorageBackendConfig

	// Logger — optional. nil uses the SDK's package logger at info
	// level. Pass a configured logger to route the SDK's output
	// through our slog setup.
	Logger *sdklog.Entry

	// Registry — optional. When non-nil, the backend registers its Prometheus metrics under
	// RegistryNamespace. MUST stay nil in production: the SDK's registration is not idempotent and
	// Stream runs more than once per process (archive, live, catch-up), so a second call panics in
	// MustRegister. Only safe for a caller that invokes Stream exactly once.
	//
	// [TieredDataStore]'s tier metrics are package-level obs metrics registered once at boot and
	// do not depend on this field.
	Registry          *prometheus.Registry
	RegistryNamespace string

	// LiveRetryWait overrides the BufferedStorageBackend's RetryWait for an unbounded (live-tail)
	// stream only. Once caught up, every next-ledger fetch misses and the worker sleeps RetryWait;
	// the SDK's 30s default makes ingest lag sawtooth 0-30s although galexie uploads within ~5s.
	// Zero leaves the SDK default. Ignored for bounded ranges, where a missing object is a hard
	// error, not a wait-for-tip.
	LiveRetryWait time.Duration

	// LiveRetryBudget is how long a live-tail fetch worker keeps retrying a datastore FAULT before
	// giving up (unbounded streams only, like LiveRetryWait; zero leaves the SDK RetryLimit). The
	// SDK retries os.ErrNotExist (tip not written yet) forever, but any other failure (refused,
	// 5xx, TLS reset) consumes an attempt and after RetryLimit Stream returns, exiting the
	// indexer. Both share RetryWait, so shortening it for tip latency would also shorten fault
	// tolerance; a time budget gives RetryLimit = ceil(budget / RetryWait), so tuning latency
	// cannot silently shrink it.
	LiveRetryBudget time.Duration

	// TolerateTrailingMissing, when true, converts a bounded Stream failing with the SDK's "ledger
	// object containing sequence X is missing" error into a clean walk-complete (nil, WARN log) if
	// X is within TrailingMissingWindow of To, for backfills racing the live tip (Galexie writes
	// partition files lazily). False keeps strict semantics. A gap farther below To, or below the
	// DataStore's own latest ledger, still errors. The caller still owns the coverage check: count
	// delivered ledgers and fail a short walk. SDK caveat: on a missing file
	// BufferedStorageBackend drops pre-fetched ledgers, so the last delivered ledger can be up to
	// BufferSize behind the missing seq; full-coverage backfills must clamp -to below the live tip
	// in advance.
	TolerateTrailingMissing bool

	// TrailingMissingWindow — how close to To, and to the DataStore's latest ledger, the missing
	// sequence must be to count as trailing-edge. Default 65536 (one Galexie partition plus slack,
	// covering a not-yet-written partition and a To that overshoots the tip by hours). Ignored when
	// TolerateTrailingMissing is false.
	TrailingMissingWindow uint32
}

// tieringEnabled reports whether Config requests a tiered
// (hot + cold) read path. The zero-value ColdDataStore disables
// tiering; any non-empty Type opts in.
func (c *Config) tieringEnabled() bool {
	return c.ColdDataStore.Type != ""
}

// Stream reads ledgers in [from, to] and invokes callback once per xdr.LedgerCloseMeta, blocking. to == 0 is an unbounded live tail (returns on ctx cancel or error); to >= from is bounded. The callback runs on Stream's goroutine, so expensive work throttles ingest.
func Stream(
	ctx context.Context,
	cfg Config,
	from, to uint32,
	callback func(xdr.LedgerCloseMeta) error,
) error {
	if callback == nil {
		return fmt.Errorf("ledgerstream: callback is nil")
	}
	if cfg.DataStore.Type == "" {
		return fmt.Errorf("ledgerstream: DataStore.Type is empty — config missing")
	}

	buffered := bufferedConfig(cfg)

	// Only an unbounded range waits for the tip; on a bounded range a missing object is a hard
	// error, so the live retry overrides are deliberately not applied.
	if to == 0 {
		applyLiveRetryPolicy(cfg, &buffered)
	}

	var ledgerRange ledgerbackend.Range
	if to == 0 {
		ledgerRange = ledgerbackend.UnboundedRange(from)
	} else {
		ledgerRange = ledgerbackend.BoundedRange(from, to)
	}
	// Before any path opens a datastore: an inverted range must not cost a
	// bucket round-trip, and the SDK default path never reaches walkDataStore.
	if err := validateRange(ledgerRange); err != nil {
		return err
	}

	// delivered counts every ledger handed to the callback on any path below:
	// maybeTolerateTrailingMissing must know whether ANYTHING landed before it turns a
	// missing-file error into success.
	var delivered uint32
	countingCallback := func(lcm xdr.LedgerCloseMeta) error {
		delivered++
		return callback(lcm)
	}

	attempt := func() error {
		switch {
		case cfg.tieringEnabled():
			return streamTiered(ctx, cfg, ledgerRange, buffered, countingCallback)
		case ledgerRange.Bounded() && ledgerRange.To() == ledgerRange.From():
			// The SDK's ApplyLedgerMetadata rejects a bounded range of exactly one ledger
			// (To() <= From()), so walk it with our own backend loop; ch-live-catchup hits this
			// whenever it fires one ledger behind the tip.
			obs.LedgerstreamStreamPathTotal.WithLabelValues("hot_single_ledger").Inc()
			return streamHot(ctx, cfg, ledgerRange, buffered, countingCallback)
		default:
			obs.LedgerstreamStreamPathTotal.WithLabelValues("sdk").Inc()
			return ingest.ApplyLedgerMetadata(
				ledgerRange,
				ingest.PublisherConfig{
					Registry:              cfg.Registry,
					RegistryNamespace:     cfg.RegistryNamespace,
					BufferedStorageConfig: buffered,
					DataStoreConfig:       cfg.DataStore,
					Log:                   cfg.Logger,
				},
				ctx,
				countingCallback,
			)
		}
	}

	err := attempt()
	if to == 0 {
		err = retryLiveStart(ctx, cfg, &delivered, err, attempt)
	}
	return maybeTolerateTrailingMissing(cfg, from, to, delivered, err, func() (uint32, error) {
		return latestLedger(ctx, cfg.DataStore)
	})
}

// latestLedger returns the newest ledger held by the store dsCfg names.
func latestLedger(ctx context.Context, dsCfg datastore.DataStoreConfig) (uint32, error) {
	ds, err := datastore.NewDataStore(ctx, dsCfg)
	if err != nil {
		return 0, fmt.Errorf("open datastore: %w", err)
	}
	defer func() { _ = ds.Close() }()
	return datastore.FindLatestLedgerSequence(ctx, ds)
}

// retryLiveStart re-runs a live tail that failed WITHOUT EVER DELIVERING A LEDGER, until
// Config.LiveRetryBudget is exhausted. It returns the last error, or nil if a re-attempt ran
// clean.
//
// [applyLiveRetryPolicy] spends the budget inside the SDK's fetch worker, which exists only after
// the datastore is opened and its schema loaded, both done once with no retry; without this, a
// lake outage at START bypasses the budget and the supervisor (which counts starts, not seconds)
// parks the unit in `failed`.
//   - It cannot skip a ledger: it re-attempts only while delivered == 0.
//   - It is bounded by a wall-clock deadline fixed before the first re-attempt; an unbounded
//     reconnect would turn a visible outage into a silent freeze.
//   - Visible via stellarindex_ledgerstream_live_start_retries_total.
//
// Errors are not classified: on an unbounded tail "could not start" always means "try again
// shortly". Backoff is exponential from LiveRetryWait, capped at [maxLiveStartRetryWait], no
// jitter. Callers must gate on the range being unbounded.
func retryLiveStart(
	ctx context.Context,
	cfg Config,
	delivered *uint32,
	err error,
	attempt func() error,
) error {
	if err == nil || cfg.LiveRetryBudget <= 0 || *delivered > 0 || ctx.Err() != nil {
		return err
	}
	wait := cfg.LiveRetryWait
	if wait <= 0 {
		wait = defaultLiveStartRetryWait
	}
	deadline := time.Now().Add(cfg.LiveRetryBudget)
	for time.Now().Before(deadline) {
		if cfg.Logger != nil {
			cfg.Logger.WithFields(map[string]interface{}{
				"err":   err.Error(),
				"wait":  wait.String(),
				"until": deadline.Format(time.RFC3339),
			}).Warn("ledgerstream: live tail could not start — retrying inside the fault budget")
		}
		obs.LedgerstreamLiveStartRetriesTotal.Inc()
		if !sleepWithContext(ctx, wait) {
			// Wrap ctx.Err() so the indexer's errors.Is(err, context.Canceled) still reads this as
			// a clean stop; otherwise SIGTERM during a lake outage exits non-zero and parks the
			// unit on a deliberate stop. The last error is wrapped too, to say why it had not started.
			return fmt.Errorf("ledgerstream: live tail start abandoned on shutdown (last error: %w): %w", err, ctx.Err())
		}
		err = attempt()
		// Stop on success, once the callback has seen anything (no-skip property above), and on
		// shutdown, where the attempt's own error already carries the cancellation.
		if err == nil || *delivered > 0 || ctx.Err() != nil {
			return err
		}
		wait *= 2
		if wait > maxLiveStartRetryWait {
			wait = maxLiveStartRetryWait
		}
	}
	return err
}

// sleepWithContext sleeps for d, returning false if ctx was cancelled first; mirrors the SDK ledger
// buffer's helper so both retry loops behave the same on shutdown.
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

const (
	// defaultLiveStartRetryWait is [retryLiveStart]'s first backoff step when a budget is set
	// without Config.LiveRetryWait; production sets both, so it only covers direct callers.
	defaultLiveStartRetryWait = time.Second

	// maxLiveStartRetryWait caps [retryLiveStart]'s backoff. 30s matches the SDK default and keeps
	// a 5-minute outage to ~16 re-attempts, each of which rebuilds an S3 client through the AWS
	// credential chain, instead of ~600 at a flat 500ms.
	maxLiveStartRetryWait = 30 * time.Second
)

// applyLiveRetryPolicy stamps the live-tail retry overrides onto the BufferedStorageBackend config; split out of [Stream] so the
// attempts-times-wait budget is unit-testable. Order is load-bearing: RetryWait is overridden first because the attempt count is
// derived from it (the SDK's 30s default against a 500ms worker sleep would shrink tolerance 60x). Callers gate on an unbounded range.
func applyLiveRetryPolicy(cfg Config, buffered *ledgerbackend.BufferedStorageBackendConfig) {
	if cfg.LiveRetryWait > 0 {
		buffered.RetryWait = cfg.LiveRetryWait
	}
	if limit := liveRetryLimit(buffered.RetryWait, cfg.LiveRetryBudget); limit > 0 {
		buffered.RetryLimit = limit
	}
}

// liveRetryLimit converts a fault-tolerance time budget into the SDK's attempt count, rounding UP
// so the budget is a floor. It returns 0 (leave the SDK default) when either input is non-positive.
func liveRetryLimit(wait, budget time.Duration) uint32 {
	if wait <= 0 || budget <= 0 {
		return 0
	}
	attempts := (budget + wait - 1) / wait
	if attempts < 1 {
		attempts = 1
	}
	return uint32(attempts)
}

// validateRange rejects malformed ranges before PrepareRange. A bounded range of exactly one
// ledger (To == From) is valid (ledgerbackend.SingleLedgerRange); a `To() <= From()` check would
// make ch-live-catchup's tip-extend flap whenever it lands one ledger behind the tip.
func validateRange(r ledgerbackend.Range) error {
	if r.Bounded() && r.To() < r.From() {
		return fmt.Errorf("ledgerstream: invalid end value for bounded range, must not be less than start")
	}
	if !r.Bounded() && r.To() > 0 {
		return fmt.Errorf("ledgerstream: invalid end value for unbounded range, must be zero")
	}
	return nil
}

// maybeTolerateTrailingMissing turns a bounded-stream missing-file error into a clean walk-complete (nil) when
// Config.TolerateTrailingMissing is set AND the missing sequence is within the trailing window of To; other errors pass through.
// A single-ledger range requires delivered > 0 (else nil with ZERO callbacks looks like an empty walk); a wider range is not held
// to it, as the prefetch buffer can race-cancel on a trailing-edge miss. resolveTip is consulted last: only the store's latest
// ledger tells a not-yet-written object from a hole.
func maybeTolerateTrailingMissing(cfg Config, from, to, delivered uint32, err error, resolveTip func() (uint32, error)) error {
	if err == nil {
		return nil
	}
	if !cfg.TolerateTrailingMissing || to == 0 {
		return err
	}
	if from == to && delivered == 0 {
		return err
	}
	seq, ok := parseTrailingMissingSeq(err)
	if !ok {
		return err
	}
	window := cfg.TrailingMissingWindow
	if window == 0 {
		window = defaultTrailingMissingWindow
	}
	if seq > to || to-seq > window {
		return err
	}
	tip, tipErr := resolveTip()
	if tipErr != nil {
		return fmt.Errorf("%w (trailing-missing tolerance refused: datastore tip unresolved: %w)", err, tipErr)
	}
	if seq <= tip && tip-seq > window {
		return fmt.Errorf("%w (trailing-missing tolerance refused: ledger %d is %d below the datastore tip %d, "+
			"beyond the %d-ledger trailing window — a hole, not the tip)", err, seq, tip-seq, tip, window)
	}
	obs.LedgerstreamTrailingMissingToleratedTotal.Inc()
	if cfg.Logger != nil {
		cfg.Logger.WithFields(map[string]interface{}{
			"missing_ledger": seq,
			"range_to":       to,
			"datastore_tip":  tip,
			"delivered":      delivered,
			"gap_to_tip":     to - seq,
			"window":         window,
		}).Warn("ledgerstream: bounded walk hit trailing-edge missing file — treating as walk-complete (TolerateTrailingMissing=true)")
	}
	return nil
}

// defaultTrailingMissingWindow is one Galexie 64k-ledger partition plus 1536 ledgers of slack,
// covering a tip race between the chosen -to and a partition Galexie has not finished writing.
const defaultTrailingMissingWindow uint32 = 65536

// trailingMissingRE matches the SDK's `ledger object containing sequence X is missing` wrap. The
// sequence appears verbatim however deep the wrap chain is; group 1 is the decimal sequence.
var trailingMissingRE = regexp.MustCompile(`ledger object containing sequence (\d+) is missing`)

// parseTrailingMissingSeq extracts the sequence from the SDK's trailing-edge missing-file error,
// or returns (0, false). It matches the message because the SDK wraps with pkg/errors.Wrapf and
// exposes no typed sentinel.
func parseTrailingMissingSeq(err error) (uint32, bool) {
	if err == nil {
		return 0, false
	}
	m := trailingMissingRE.FindStringSubmatch(err.Error())
	if len(m) != 2 {
		return 0, false
	}
	n, perr := strconv.ParseUint(m[1], 10, 32)
	if perr != nil {
		return 0, false
	}
	return uint32(n), true
}

// streamTiered is the hot+cold branch of [Stream]. It mirrors the SDK's ApplyLedgerMetadata loop
// (same validation, from-clamp and error wrapping) but injects a [TieredDataStore] built from
// cfg.DataStore and cfg.ColdDataStore as the BufferedStorageBackend's store.
func streamTiered(
	ctx context.Context,
	cfg Config,
	ledgerRange ledgerbackend.Range,
	buffered ledgerbackend.BufferedStorageBackendConfig,
	callback func(xdr.LedgerCloseMeta) error,
) error {
	hot, err := datastore.NewDataStore(ctx, cfg.DataStore)
	if err != nil {
		return fmt.Errorf("ledgerstream: hot datastore: %w", err)
	}
	cold, err := openColdDataStore(ctx, cfg)
	if err != nil {
		// The cold tier is optional (ADR-0027): the local archive is still authoritative for
		// everything it holds. Degrade to hot-only with a Warn rather than propagate, which would
		// turn a cold misconfig (e.g. region mismatch) into a backfill abort.
		obs.LedgerstreamStreamPathTotal.WithLabelValues("cold_degraded").Inc()
		if cfg.Logger != nil {
			cfg.Logger.WithField("err", err).Warn("ledgerstream: cold datastore init failed; falling back to hot-only single-source path")
		}
		if ledgerRange.Bounded() && ledgerRange.To() == ledgerRange.From() {
			// ApplyLedgerMetadata rejects single-ledger ranges; reuse the open hot store.
			return walkDataStore(ctx, cfg, hot, ledgerRange, buffered, callback)
		}
		_ = hot.Close()
		return ingest.ApplyLedgerMetadata(
			ledgerRange,
			ingest.PublisherConfig{
				Registry:              cfg.Registry,
				RegistryNamespace:     cfg.RegistryNamespace,
				BufferedStorageConfig: buffered,
				DataStoreConfig:       cfg.DataStore,
				Log:                   cfg.Logger,
			},
			ctx,
			callback,
		)
	}
	// Every tiered read computes object keys from ONE schema, hot's (TieredDataStore prefers hot).
	// If cold's export used a different LedgersPerFile/FilesPerPartition/FileExtension, hot-shaped
	// keys 404 on cold and the fallback silently never fires. Hard-fail on a mismatch now: it is a
	// config bug, not a transient condition to route around.
	hotSchema, err := datastore.LoadSchema(ctx, hot, cfg.DataStore)
	if err != nil {
		_ = hot.Close()
		_ = cold.Close()
		return fmt.Errorf("ledgerstream: load hot schema: %w", err)
	}
	coldSchema, err := datastore.LoadSchema(ctx, cold, cfg.ColdDataStore)
	if err != nil {
		_ = hot.Close()
		_ = cold.Close()
		return fmt.Errorf("ledgerstream: load cold schema: %w", err)
	}
	if hotSchema.LedgersPerFile != coldSchema.LedgersPerFile ||
		hotSchema.FilesPerPartition != coldSchema.FilesPerPartition ||
		hotSchema.FileExtension != coldSchema.FileExtension {
		_ = hot.Close()
		_ = cold.Close()
		return fmt.Errorf(
			"ledgerstream: cold datastore schema (ledgers_per_file=%d files_per_partition=%d file_extension=%q) differs from hot's (ledgers_per_file=%d files_per_partition=%d file_extension=%q) — cold fallback would issue hot-shaped object keys to a differently-partitioned cold store and always miss",
			coldSchema.LedgersPerFile, coldSchema.FilesPerPartition, coldSchema.FileExtension,
			hotSchema.LedgersPerFile, hotSchema.FilesPerPartition, hotSchema.FileExtension)
	}

	obs.LedgerstreamStreamPathTotal.WithLabelValues("tiered").Inc()
	tiered := NewTieredDataStore(hot, cold)
	return walkDataStore(ctx, cfg, tiered, ledgerRange, buffered, callback)
}

// openColdDataStore opens the cold tier, preferring [Config.ColdDataStoreFactory]. The
// datastore.NewDataStore fallback is for tests and filesystem fixtures where the ambient AWS
// credential chain is irrelevant; production S3 cannot use it (see the factory's godoc).
func openColdDataStore(ctx context.Context, cfg Config) (datastore.DataStore, error) {
	if cfg.ColdDataStoreFactory != nil {
		return cfg.ColdDataStoreFactory(ctx)
	}
	return datastore.NewDataStore(ctx, cfg.ColdDataStore)
}

// streamHot is the hot-only counterpart of [streamTiered], over cfg.DataStore alone. [Stream]
// routes single-ledger non-tiered requests here because the SDK's ApplyLedgerMetadata rejects them.
func streamHot(
	ctx context.Context,
	cfg Config,
	ledgerRange ledgerbackend.Range,
	buffered ledgerbackend.BufferedStorageBackendConfig,
	callback func(xdr.LedgerCloseMeta) error,
) error {
	hot, err := datastore.NewDataStore(ctx, cfg.DataStore)
	if err != nil {
		return fmt.Errorf("ledgerstream: hot datastore: %w", err)
	}
	return walkDataStore(ctx, cfg, hot, ledgerRange, buffered, callback)
}

// StreamStore walks the bounded range [from, to] over a datastore the caller opened (the cold tier); cfg.DataStore supplies schema params and store is closed on return.
func StreamStore(
	ctx context.Context,
	cfg Config,
	store datastore.DataStore,
	from, to uint32,
	callback func(xdr.LedgerCloseMeta) error,
) error {
	if callback == nil {
		_ = store.Close()
		return fmt.Errorf("ledgerstream: callback is nil")
	}
	return walkDataStore(ctx, cfg, store, ledgerbackend.BoundedRange(from, to), bufferedConfig(cfg), callback)
}

func bufferedConfig(cfg Config) ledgerbackend.BufferedStorageBackendConfig {
	if cfg.Buffered != nil {
		return *cfg.Buffered
	}
	lpf := cfg.DataStore.Schema.LedgersPerFile
	if lpf == 0 {
		// Seed value (Galexie's default is 1 ledger per file); schema discovery overrides it.
		lpf = 1
	}
	return ingest.DefaultBufferedStorageBackendConfig(lpf)
}

// walkDataStore builds the buffered storage backend over store and runs the GetLedger walk, the shared tail of [streamTiered] and
// [streamHot]. It closes store on every return path because the SDK's BufferedStorageBackend.Close does not (connection leak).
func walkDataStore(
	ctx context.Context,
	cfg Config,
	store datastore.DataStore,
	ledgerRange ledgerbackend.Range,
	buffered ledgerbackend.BufferedStorageBackendConfig,
	callback func(xdr.LedgerCloseMeta) error,
) error {
	defer func() { _ = store.Close() }()

	schema, err := datastore.LoadSchema(ctx, store, cfg.DataStore)
	if err != nil {
		return fmt.Errorf("ledgerstream: load schema: %w", err)
	}

	var backend ledgerbackend.LedgerBackend
	backend, err = ledgerbackend.NewBufferedStorageBackend(buffered, store, schema)
	if err != nil {
		return fmt.Errorf("ledgerstream: new buffered storage backend: %w", err)
	}
	if cfg.Registry != nil {
		backend = ledgerbackend.WithMetrics(backend, cfg.Registry, cfg.RegistryNamespace)
	}
	defer func() { _ = backend.Close() }()

	if err := validateRange(ledgerRange); err != nil {
		return err
	}

	from := ledgerRange.From()
	if from < 2 {
		from = 2
	}
	// A bounded request entirely below genesis (e.g. from=0, to=1) leaves clamped-from > To, so the
	// walk would deliver ZERO ledgers and return nil, indistinguishable from a deliberately empty
	// range. Refuse loudly instead.
	if ledgerRange.Bounded() && from > ledgerRange.To() {
		return fmt.Errorf("ledgerstream: requested range [%d,%d] is entirely before genesis ledger 2",
			ledgerRange.From(), ledgerRange.To())
	}
	// Rebuild the range from the CLAMPED from so PrepareRange and the walk agree on the bounds.
	if from != ledgerRange.From() {
		if ledgerRange.Bounded() {
			ledgerRange = ledgerbackend.BoundedRange(from, ledgerRange.To())
		} else {
			ledgerRange = ledgerbackend.UnboundedRange(from)
		}
	}
	if err := backend.PrepareRange(ctx, ledgerRange); err != nil {
		return fmt.Errorf("ledgerstream: prepare range: %w", err)
	}

	for seq := from; seq <= ledgerRange.To() || !ledgerRange.Bounded(); seq++ {
		lcm, err := backend.GetLedger(ctx, seq)
		if err != nil {
			return fmt.Errorf("ledgerstream: get ledger %d: %w", seq, err)
		}
		if err := callback(lcm); err != nil {
			return fmt.Errorf("ledgerstream: callback %d: %w", seq, err)
		}
	}
	return nil
}
