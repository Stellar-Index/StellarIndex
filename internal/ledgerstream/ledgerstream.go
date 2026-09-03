// Package ledgerstream reads Galexie-exported ledger-meta from an
// S3-compatible datastore (MinIO in production, Filesystem in
// tests) and yields one xdr.LedgerCloseMeta per ledger to a caller-
// supplied callback.
//
// This package is the **only** production path into the ingest
// pipeline. Per docs/architecture/ingest-pipeline.md, every source
// decoder receives its events via this package's output, never
// via stellar-rpc. The scripts/ci/lint-imports.sh rule
// A/no-rpc-in-ingest blocks stellarrpc imports from the ingest
// codepath as a structural guardrail.
//
// Design: this is a **thin wrapper** around the SDK's
// ingest.ApplyLedgerMetadata. The SDK already implements the
// buffered, parallel-fetch, retry-on-error reader; we don't
// reimplement it. This package exists to:
//
//  1. Give us a stable seam for testing (inject a Filesystem
//     datastore in tests, MinIO in integration, S3 in prod).
//  2. Centralize logger + Prometheus registry wiring.
//  3. Provide a single place for any future customization
//     (bounded-vs-unbounded, cursor persistence, etc.).
//
// If the wrapper turns out to be pure delegation, that's still
// the correct value — one import boundary, one test seam.
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

// Config binds the SDK's datastore configuration + BufferedStorageBackend
// tuning + optional observability into one unit. Typical production
// values come from our config.Stellar section; unit tests use the
// filesystem datastore pointed at a tempdir.
//
// The zero-value DataStore is invalid — [Stream] returns an error
// rather than silently skipping.
type Config struct {
	// DataStore — required. Describes the Galexie output bucket
	// (S3/MinIO/GCS) or a filesystem directory for tests. In a
	// tiered deployment this is the **hot** tier (local
	// galexie-archive on r1).
	DataStore datastore.DataStoreConfig

	// ColdDataStore — optional. When non-zero (Type set), Stream
	// constructs a [TieredDataStore] wrapping DataStore (hot) +
	// ColdDataStore (cold) per ADR-0027, so reads of LCMs absent
	// from the local mirror transparently fall back to a cold
	// upstream (typically `aws-public-blockchain` S3 — the AWS
	// Open Data Sponsorship bucket). Writes always target hot.
	// The zero-value disables tiering; the legacy single-source
	// path through ingest.ApplyLedgerMetadata is used instead —
	// behaviour exactly matches pre-#7-step-1.
	ColdDataStore datastore.DataStoreConfig

	// ColdDataStoreFactory — optional. When non-nil, [Stream] calls
	// this INSTEAD OF datastore.NewDataStore(ctx, ColdDataStore) to
	// open the cold tier; ColdDataStore is still required (its
	// non-empty Type is the tiering opt-in, and its NetworkPassphrase
	// / Schema still drive datastore.LoadSchema on the cold side).
	//
	// It exists because the SDK's datastore.NewDataStore builds every
	// S3 client through config.LoadDefaultConfig, i.e. the ambient AWS
	// credential chain — and on r1 that chain carries local MinIO's
	// credentials (the HOT tier authenticates through it). Those keys
	// were then presented to real AWS, so every cold read failed with
	// `InvalidAccessKeyId: The AWS Access Key Id you provided does not
	// exist in our records` and the tier silently degraded to hot-only
	// (2026-07-25 diagnosis; the tier had never worked). One process
	// cannot serve two S3 backends with different credentials through
	// datastore.NewDataStore.
	//
	// This package takes datastore.DataStoreConfig, not our
	// config.Config, so it cannot resolve the storage.s3_cold_*_key_env
	// names itself — hence a hook rather than more fields. Production
	// wires pipeline.NewColdDataStore in via
	// pipeline.LedgerstreamConfig.
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

	// Registry — optional. When non-nil, the backend registers
	// Prometheus metrics (buffer_fetch_latency_seconds, etc.) under
	// RegistryNamespace. Use our main obs registry in production.
	// When ColdDataStore is also set, the [TieredDataStore]'s
	// tier_read_total + cold_read_duration_seconds metrics
	// register under the same registry.
	Registry          *prometheus.Registry
	RegistryNamespace string

	// LiveRetryWait overrides the BufferedStorageBackend's RetryWait
	// for an **unbounded (live-tail)** stream only. The SDK default
	// is 30s: when a fetch worker requests the next ledger object and
	// it isn't in the datastore yet, the worker sleeps RetryWait
	// before re-checking. Once the indexer has caught up to galexie's
	// tip every next-ledger fetch misses, so a 30s default makes the
	// end-to-end ingest lag sawtooth 0→30s even though galexie
	// uploads the next LCM within ~5s. Setting this to a few seconds
	// lets a caught-up worker re-check promptly. Zero leaves the
	// SDK/derived default untouched. Ignored for bounded ranges (a
	// missing object there is a hard error, not a wait-for-tip).
	LiveRetryWait time.Duration

	// LiveRetryBudget is how long a live-tail fetch worker keeps
	// retrying a datastore FAULT before giving up, for an **unbounded
	// (live-tail)** stream only (#371 F3). Zero leaves the SDK/derived
	// RetryLimit untouched.
	//
	// The SDK's ledger buffer treats the two failure modes very
	// differently, and that asymmetry is what this knob exists to
	// exploit (go-stellar-sdk ingest/ledgerbackend/ledger_buffer.go
	// `worker`):
	//
	//   - `os.ErrNotExist` on an unbounded range — "the tip hasn't been
	//     written yet". Sleeps RetryWait and retries WITHOUT consuming
	//     an attempt, forever. This is the hot path on a caught-up
	//     indexer and LiveRetryWait keeps it fast (see above).
	//   - anything else (connection refused, 5xx, TLS reset) — consumes
	//     an attempt; after RetryLimit of them the backend cancels and
	//     Stream returns, which in the indexer means process exit.
	//
	// Because the two share RetryWait, shortening it for tip-latency
	// also shortened the FAULT tolerance: at LiveRetryWait=500ms and
	// the SDK's RetryLimit=5, a MinIO blip was survivable for 2.5
	// SECONDS. A MinIO restart of a couple of minutes therefore exited
	// the indexer repeatedly until systemd's StartLimit parked the unit
	// in `failed`. Expressing the tolerance as a TIME budget instead of
	// an attempt count keeps the two concerns independent: RetryLimit
	// is derived as ceil(budget / RetryWait), so tuning tip latency can
	// never again silently shrink fault tolerance.
	//
	// Ignored for bounded ranges, like LiveRetryWait: a bounded walk's
	// missing object is a hard error and its caller decides.
	LiveRetryBudget time.Duration

	// TolerateTrailingMissing — when true, a bounded Stream that
	// fails with the SDK's "ledger object containing sequence X is
	// missing" error is converted to a clean walk-complete (returns
	// nil with a WARN log) provided X is within TrailingMissingWindow
	// of the bounded To. Use for backfills that may race the live
	// tip (Galexie writes partition files lazily) or for archive-
	// integrity walks where a trailing-edge gap is "the tip isn't
	// here yet" rather than corruption. False (default) preserves
	// strict bounded semantics: any missing file is an error,
	// matching pre-2026-05-26 behaviour.
	//
	// Mid-range gaps still error regardless of this flag — the
	// window check guards against masking real corruption. The
	// 2026-05-26 audit walks against the same archive confirmed
	// the chain is intact up to the live tip; the failure mode
	// this targets is exclusively the trailing 1-2 partitions
	// that Galexie hasn't finished uploading yet.
	//
	// Delivery caveat: when the SDK's BufferedStorageBackend hits a
	// missing file it cancels its internal context, dropping any
	// pre-fetched ledgers in the buffer that hadn't been delivered
	// to the callback yet. This is SDK-level behaviour. Result: the
	// last delivered ledger can be up to BufferSize ledgers behind
	// the missing-file's seq. Operators relying on full coverage
	// (e.g. 100%-density backfills) must clamp -to below the live
	// tip in advance; the tolerate flag's role is graceful exit on
	// trailing-edge races (chain-check, defence in depth), not a
	// substitute for tip-aware -to selection.
	TolerateTrailingMissing bool

	// TrailingMissingWindow — how close to the bounded range's To
	// the missing-file's sequence must be to qualify as
	// trailing-edge. Default 65536 (one full Galexie 64k-ledger
	// partition plus slack — covers any "Galexie hasn't written
	// the next partition yet" race plus operator-set To values
	// that overshoot the tip by hours). Mid-range gaps farther
	// from To than the window error regardless of the tolerate
	// flag. Ignored when TolerateTrailingMissing is false.
	TrailingMissingWindow uint32
}

// tieringEnabled reports whether Config requests a tiered
// (hot + cold) read path. The zero-value ColdDataStore disables
// tiering; any non-empty Type opts in.
func (c *Config) tieringEnabled() bool {
	return c.ColdDataStore.Type != ""
}

// Stream reads ledgers in [from, to] from the datastore and invokes
// callback once per xdr.LedgerCloseMeta.
//
//   - to == 0 → unbounded live tail. Stream returns only on ctx
//     cancellation, a datastore error, or a callback error.
//   - to >= from → bounded range. Stream returns nil on successful
//     completion of the range.
//
// from is clamped upward to the Stellar genesis ledger (2), per
// the SDK's ApplyLedgerMetadata contract. Callers passing 0 or 1
// get data from ledger 2 onward — that's an SDK behavior, not ours.
//
// The callback blocks Stream's goroutine; expensive work inside
// callback directly affects ingest throughput. For multi-consumer
// fanout, have callback send onto a channel and let consumers read
// off it.
//
// Blocking: yes. Call Stream in its own goroutine if the caller
// needs concurrent work.
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

	var buffered ledgerbackend.BufferedStorageBackendConfig
	if cfg.Buffered != nil {
		buffered = *cfg.Buffered
	} else {
		lpf := cfg.DataStore.Schema.LedgersPerFile
		if lpf == 0 {
			// Galexie's default at the time of writing is 1 ledger per
			// file; the SDK's schema discovery will override this if
			// the datastore's manifest says otherwise, but we still
			// need a value to seed the default config.
			lpf = 1
		}
		buffered = ingest.DefaultBufferedStorageBackendConfig(lpf)
	}

	// Live-tail retry policy — see Config.LiveRetryWait /
	// Config.LiveRetryBudget. Only an unbounded range (to == 0) waits
	// for the tip; on a bounded range a missing object is a hard error,
	// so the overrides are meaningless there and deliberately not
	// applied.
	if to == 0 {
		applyLiveRetryPolicy(cfg, &buffered)
	}

	var ledgerRange ledgerbackend.Range
	if to == 0 {
		ledgerRange = ledgerbackend.UnboundedRange(from)
	} else {
		ledgerRange = ledgerbackend.BoundedRange(from, to)
	}

	// delivered counts every ledger actually handed to the caller's
	// callback, regardless of which path below produced it. COR-01
	// (audit-2026-07-23): maybeTolerateTrailingMissing must know
	// whether ANYTHING landed before converting a missing-file error
	// into a clean success — see its godoc.
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
			// The SDK's ingest.ApplyLedgerMetadata rejects a bounded
			// range of exactly one ledger (producer.go: `To() <=
			// From()`) even though the SDK exports SingleLedgerRange.
			// Walk it with our own backend loop instead — this is
			// ch-live-catchup's tip-extend case whenever the timer
			// fires exactly one ledger behind the galexie tip.
			return streamHot(ctx, cfg, ledgerRange, buffered, countingCallback)
		default:
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
	return maybeTolerateTrailingMissing(cfg, from, to, delivered, err)
}

// retryLiveStart re-runs a live tail that failed WITHOUT EVER DELIVERING
// A LEDGER, until Config.LiveRetryBudget is exhausted (#371 F3, residual).
// It returns the last error, or nil if a re-attempt eventually ran clean.
//
// Why this exists on top of [applyLiveRetryPolicy]. That policy spends the
// budget inside the SDK's fetch worker, and the worker only exists once
// the datastore has been opened and its schema loaded. Both of those
// happen ONCE, up front, with no retry anywhere:
//
//	dataStore, err := datastoreFactory(ctx, publisherConfig.DataStoreConfig)
//	if err != nil { return fmt.Errorf("failed to create datastore: %w", err) }
//	schema, err := datastore.LoadSchema(context.Background(), dataStore, …)
//	if err != nil { return fmt.Errorf("failed to retrieve datastore schema: %w", err) }
//
// (go-stellar-sdk ingest/producer.go; our streamTiered/walkDataStore have
// the same shape, and LoadSchema is a live round-trip — it LISTS the
// bucket to discover the ledger file extension.) So a lake outage that is
// present when the stream STARTS was not covered by the budget at all:
// Stream returned in microseconds, not five minutes. Measured on the
// unfixed code at 85µs against a 3s budget.
//
// That is not a cosmetic difference, because the indexer's supervisor
// counts starts, not seconds. The first process burns its 5-minute
// in-walk budget and exits; every restart after that dies in the startup
// path in about a second, so the start cycle collapses to RestartSec
// (10s). Sixty of those fit inside StartLimitIntervalSec=15min, the unit
// parks in `failed`, and it stays parked after MinIO comes back until a
// human runs `systemctl reset-failed`. The whole point of the budget was
// that a lake blip degrades to stall-and-retry rather than needing an
// operator, so the startup path has to honour it too.
//
// Three properties make the retry safe:
//
//   - It CANNOT skip a ledger. It re-attempts only while delivered == 0,
//     re-issuing the identical range, so the caller's callback — which is
//     where the cursor is written — has not run and there is nothing to
//     resume past. The moment any ledger lands, a later failure is
//     returned untouched: resuming mid-stream would need a cursor-aware
//     restart, which belongs to the caller, not here.
//   - It is BOUNDED, by wall clock rather than attempts. The deadline is
//     fixed before the first re-attempt, so an attempt that itself
//     consumes the whole in-walk budget leaves no time for another and
//     the total stays ~one budget. An unbounded reconnect would convert a
//     visible outage into a silent freeze, which is strictly worse than
//     the crash it replaces.
//   - It is VISIBLE while it runs, via
//     stellarindex_ledgerstream_live_start_retries_total. Exhaustion
//     itself is deliberately NOT a counter: it increments once and the
//     process exits, so no scrape would ever see it — the honest cover
//     for exhaustion is the process exit, which pages via
//     stellarindex_ingestion_ledger_stalled (and, for the MinIO case,
//     stellarindex_minio_exporter_down within ~2 min).
//
// Errors are deliberately NOT classified into transient and permanent
// here. On an unbounded tail the only correct response to "I could not
// start" is "try again shortly" whatever the cause, and the classes are
// not reliably separable at this seam anyway — the SDK wraps with
// pkg/errors and exposes no sentinel, and a 403 is as likely to be a
// half-restarted MinIO as a revoked key. A genuinely permanent fault
// (bad credentials, deleted bucket) therefore surfaces one budget later
// instead of immediately, exactly as [Config.LiveRetryBudget] already
// documents for the in-walk case.
//
// Backoff is exponential from LiveRetryWait, capped at
// [maxLiveStartRetryWait]. No jitter: jitter exists to de-correlate many
// clients against one server, and there is exactly one indexer per host
// reading a MinIO on 127.0.0.1 — it would buy nothing here and make the
// timing untestable.
//
// Callers must gate on the range being unbounded; see [Stream].
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
			// Shutdown landed in the backoff window — the one place this
			// function blocks that a plain walk does not. Carry the
			// cancellation in the error so the indexer's
			// `errors.Is(err, context.Canceled)` check still reads it as a
			// clean stop; without it, a SIGTERM during a lake outage would
			// exit non-zero and park the unit on a deliberate `systemctl
			// stop`. The original cause is wrapped alongside so the journal
			// still says WHY the tail had not started.
			return fmt.Errorf("ledgerstream: live tail start abandoned on shutdown (last error: %w): %w", err, ctx.Err())
		}
		err = attempt()
		// Stop on success, on anything the caller's callback has already
		// seen (see the no-skip property above), and on shutdown — where
		// the attempt's own error already carries the cancellation, since
		// it came from a ctx-aware walk rather than from this loop.
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

// sleepWithContext sleeps for d, returning false if ctx was cancelled
// first. Mirrors the SDK ledger buffer's helper of the same name so the
// two retry loops behave identically on shutdown.
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
	// defaultLiveStartRetryWait is the first backoff step used by
	// [retryLiveStart] when the caller set a budget but no
	// Config.LiveRetryWait. Production always sets both
	// (pipeline.LedgerstreamConfig), so this only covers direct callers.
	defaultLiveStartRetryWait = time.Second

	// maxLiveStartRetryWait caps [retryLiveStart]'s exponential backoff.
	// 30s matches the SDK's own default RetryWait, and at a 5-minute
	// budget it keeps a full outage to ~16 re-attempts rather than the
	// ~600 a flat 500ms step would make — each of which rebuilds an S3
	// client through the AWS credential chain.
	maxLiveStartRetryWait = 30 * time.Second
)

// applyLiveRetryPolicy stamps the live-tail retry overrides onto the
// BufferedStorageBackend config (#371 F3). Split out of [Stream] so the
// policy — not just its inputs — is unit-testable: the ONLY correctness
// property that matters here is that the derived attempt count times the
// wait covers the configured budget, and that is invisible from Stream's
// signature.
//
// Order is load-bearing: RetryWait is overridden first, because the
// attempt count is derived FROM it. Deriving the limit from the SDK's
// 30s default while the worker actually sleeps 500ms would give a
// tolerance 60× shorter than asked for — the same coupling bug in a new
// costume.
//
// Callers must gate on the range being unbounded; see [Stream].
func applyLiveRetryPolicy(cfg Config, buffered *ledgerbackend.BufferedStorageBackendConfig) {
	if cfg.LiveRetryWait > 0 {
		buffered.RetryWait = cfg.LiveRetryWait
	}
	if limit := liveRetryLimit(buffered.RetryWait, cfg.LiveRetryBudget); limit > 0 {
		buffered.RetryLimit = limit
	}
}

// liveRetryLimit converts a wall-clock fault-tolerance budget into the
// SDK's attempt count, rounding UP so the budget is a floor rather than
// a ceiling. Returns 0 (meaning "leave the SDK default alone") when
// either input is non-positive.
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

// validateRange rejects malformed ranges before PrepareRange. A
// bounded range of exactly one ledger (To == From) is VALID — the
// SDK models it as a first-class concept
// (ledgerbackend.SingleLedgerRange) and the walk loop handles it as
// a single iteration. The previous `To() <= From()` check rejected
// it, which made ch-live-catchup's tip-extend fail every time the
// timer fired exactly one ledger behind the galexie tip (an
// intermittent ~flap whenever the 10-min cadence landed on a
// 1-ledger delta; observed on r1 2026-06-11).
func validateRange(r ledgerbackend.Range) error {
	if r.Bounded() && r.To() < r.From() {
		return fmt.Errorf("ledgerstream: invalid end value for bounded range, must not be less than start")
	}
	if !r.Bounded() && r.To() > 0 {
		return fmt.Errorf("ledgerstream: invalid end value for unbounded range, must be zero")
	}
	return nil
}

// maybeTolerateTrailingMissing converts a bounded-stream missing-
// file error into a clean walk-complete (nil) when the operator
// opted in via Config.TolerateTrailingMissing AND the missing
// sequence is within the trailing window of the bounded To. All
// other error shapes pass through unchanged. Always returns nil
// for nil err.
//
// A single-ledger bounded range (from == to) whose one ledger IS the
// missing one requires delivered > 0 to tolerate (COR-01,
// audit-2026-07-23): that used to tolerate unconditionally, so Stream
// returned nil having invoked the callback ZERO times — a silent
// no-op indistinguishable from a genuinely empty, successfully-walked
// range. A wider bounded range is NOT held to this: the SDK's
// BufferedStorageBackend can legitimately race-cancel its prefetch
// buffer on a trailing-edge miss and deliver anywhere from zero to
// all of the ledgers that were actually present on disk before the
// gap (see TestStream_TolerateTrailingMissing_HappyPath) — treating
// THAT delivered==0 as "never tolerate" would make an already-
// materialised, otherwise-successful backfill range flaky depending
// on prefetch-worker scheduling. Single-ledger is unambiguous: there
// is only one possible outcome (delivered) and "0" always means
// "nothing exists here at all", never a race artifact.
func maybeTolerateTrailingMissing(cfg Config, from, to, delivered uint32, err error) error {
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
	if cfg.Logger != nil {
		cfg.Logger.WithFields(map[string]interface{}{
			"missing_ledger": seq,
			"range_to":       to,
			"delivered":      delivered,
			"gap_to_tip":     to - seq,
			"window":         window,
		}).Warn("ledgerstream: bounded walk hit trailing-edge missing file — treating as walk-complete (TolerateTrailingMissing=true)")
	}
	return nil
}

// defaultTrailingMissingWindow is one Galexie 64k-ledger partition
// plus 1536 ledgers of slack, covering any tip-race between the
// operator's chosen -to and the partition file Galexie hasn't
// finished writing yet.
const defaultTrailingMissingWindow uint32 = 65536

// trailingMissingRE matches the SDK's
// `ledger object containing sequence X is missing` error wrap. The
// SDK uses `pkg/errors.Wrapf`, which produces a colon-joined chain
// — the sequence appears verbatim in the message regardless of how
// many layers deep the wrap is. Capturing group 1 is the sequence
// as a decimal integer.
var trailingMissingRE = regexp.MustCompile(`ledger object containing sequence (\d+) is missing`)

// parseTrailingMissingSeq extracts the sequence number from the
// SDK's trailing-edge missing-file error. Returns (0, false) when
// the error does not match the SDK's known wrap shape.
//
// We match on the error string because the SDK
// (github.com/stellar/go-stellar-sdk/ingest/ledgerbackend.ledger_buffer)
// wraps with pkg/errors.Wrapf and exposes no typed sentinel.
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

// streamTiered is the hot+cold branch of [Stream]. It mirrors the
// SDK's ingest.ApplyLedgerMetadata loop (producer.go) but injects
// a [TieredDataStore] as the BufferedStorageBackend's underlying
// store instead of letting the SDK construct one from
// DataStoreConfig. Both hot and cold instances of the SDK's
// concrete DataStore are built from cfg.DataStore + cfg.ColdDataStore
// respectively, then wrapped.
//
// Behavioural parity with ApplyLedgerMetadata: same bounded/unbounded
// validation, same from-clamp (max(2, range.From)), same GetLedger
// loop, same error wrapping.
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
		// Cold tier is OPTIONAL by design (ADR-0027) — it's a
		// fallback for ledger ranges trimmed from local
		// galexie-archive. If cold init fails (wrong region,
		// network issue, anonymous auth rejected by the upstream
		// bucket, etc.) we should NOT abort — local galexie-archive
		// is still authoritative for everything the system was
		// reading pre-tier-enable. Hot-only path via the legacy
		// ApplyLedgerMetadata is byte-equivalent to pre-#7-step-1b
		// behaviour.
		//
		// Fail-loud-but-degrade: log a Warn (operator-visible) and
		// fall back; don't propagate the cold-side error as a
		// blocking failure. The pre-fix behaviour cascaded a
		// cold-misconfig (region mismatch in r1's 2026-05-20 §3
		// enable) into a backfill abort — opposite of the cold
		// tier being optional.
		if cfg.Logger != nil {
			cfg.Logger.WithField("err", err).Warn("ledgerstream: cold datastore init failed; falling back to hot-only single-source path")
		}
		if ledgerRange.Bounded() && ledgerRange.To() == ledgerRange.From() {
			// ApplyLedgerMetadata rejects single-ledger bounded
			// ranges (see the [Stream] dispatch) — reuse the
			// already-open hot store via our own walk.
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
	// INT-01 (audit-2026-07-23): object keys for every tiered read are
	// computed from ONE schema — the one [walkDataStore] loads from
	// whichever tier answers first, i.e. hot's (TieredDataStore.
	// GetFileMetadata prefers hot). If cold's actual Galexie export
	// used a different LedgersPerFile/FilesPerPartition/FileExtension
	// shape, every hot-shaped key handed to cold on fallback is simply
	// wrong for cold's layout — cold 404s exactly like hot did, and
	// the "fallback" silently never fires. An operator who configured
	// ColdDataStore expecting archive-range recovery gets a confusing
	// both-tiers-missing error deep in a later Stream call instead of
	// a clear diagnostic now. Validate both schemas agree before
	// wrapping them — hard-fail rather than silently degrading, since
	// a shape mismatch is a config bug the operator needs to fix, not
	// a transient condition to route around.
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

	tiered := NewTieredDataStore(hot, cold)
	return walkDataStore(ctx, cfg, tiered, ledgerRange, buffered, callback)
}

// openColdDataStore opens the cold tier, preferring
// [Config.ColdDataStoreFactory] when the caller supplied one. The
// datastore.NewDataStore path remains for callers that construct a
// ledgerstream.Config directly (tests, Filesystem-backed fixtures)
// and for whom the ambient AWS credential chain is either irrelevant
// or already correct — see ColdDataStoreFactory's godoc for why the
// production S3 path cannot use it.
func openColdDataStore(ctx context.Context, cfg Config) (datastore.DataStore, error) {
	if cfg.ColdDataStoreFactory != nil {
		return cfg.ColdDataStoreFactory(ctx)
	}
	return datastore.NewDataStore(ctx, cfg.ColdDataStore)
}

// streamHot is the hot-only counterpart of [streamTiered]: same
// backend construction + walk loop, but over cfg.DataStore alone
// with no tiering wrapper. It exists because the SDK's
// ingest.ApplyLedgerMetadata rejects a bounded range of exactly one
// ledger (`To() <= From()` in producer.go) even though the SDK
// itself exports ledgerbackend.SingleLedgerRange — so [Stream]
// routes single-ledger non-tiered requests here instead.
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

// walkDataStore builds the buffered storage backend over `store`
// and runs the GetLedger walk — the shared tail of [streamTiered]
// and [streamHot]. Closes `store` itself on every return path (AGT-08,
// audit-2026-07-23): the SDK's BufferedStorageBackend.Close only
// closes its internal ledger buffer, NOT the underlying
// datastore.DataStore it was built over — this docstring previously
// claimed backend.Close() closed the store "thereby", which was
// false, and walkDataStore leaked the store's open connections/file
// handles on every non-early-return path. Behavioural parity with the
// SDK's ingest.ApplyLedgerMetadata loop: same from-clamp (max(2,
// From)), same GetLedger loop, same error wrapping — except
// single-ledger bounded ranges are accepted (see [validateRange]).
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
	// COR-01 (audit-2026-07-23): a single-ledger (or any) bounded
	// request entirely below genesis — e.g. from=0/1, to=1 — used to
	// PrepareRange against the UNCLAMPED range (which accepted it)
	// while the walk loop below started at the CLAMPED `from`. With
	// clamped-from > To, the loop condition was false on its very
	// first check, so the function returned nil (success) having
	// delivered ZERO ledgers — a silent no-op indistinguishable from
	// "walked an empty range on purpose". Refuse loudly instead: every
	// ledger the caller asked for predates what the SDK's
	// ApplyLedgerMetadata contract will ever serve.
	if ledgerRange.Bounded() && from > ledgerRange.To() {
		return fmt.Errorf("ledgerstream: requested range [%d,%d] is entirely before genesis ledger 2",
			ledgerRange.From(), ledgerRange.To())
	}
	// Rebuild the range from the CLAMPED from so PrepareRange (which
	// stages/validates the range against the datastore) and the walk
	// loop below always agree on the same bounds.
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
