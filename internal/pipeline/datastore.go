package pipeline

import (
	"context"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
)

// liveTailRetryWait shortens the SDK BufferedStorageBackend's 30s
// default RetryWait for the live-tail stream. galexie uploads a new
// LCM roughly every ~5s; with the 30s default a caught-up fetch
// worker sleeps a full 30s between re-checks, making end-to-end
// ingest lag sawtooth 0→30s.
//
// 500ms (was 3s): this re-check was the single largest reducible term
// in the real-time movement latency budget — a caught-up worker sat a
// flat 3s behind the tip (measured: ingested_at−close_time ≈ 3000ms on
// r1). MinIO is LOCAL, so a caught-up re-check is a cheap bucket LIST
// that mostly finds nothing until galexie's next ~5s upload; 500ms
// re-checks promptly without meaningful load. See
// ledgerstream.Config.LiveRetryWait.
const liveTailRetryWait = 500 * time.Millisecond

// liveTailRetryBudget is how long the live tail keeps retrying a
// datastore FAULT (connection refused, 5xx, TLS reset) before the
// stream gives up — as opposed to a "the tip isn't written yet" miss,
// which the SDK retries forever without consuming attempts. See
// ledgerstream.Config.LiveRetryBudget for the mechanism.
//
// The SDK pairs RetryWait with RetryLimit=5, so a 500ms RetryWait alone caps
// MinIO-fault tolerance at **2.5 seconds**. A MinIO restart of a couple of
// minutes would then kill stellarindex-indexer until systemd's StartLimit
// parked the unit in `failed`, taking ingest, the CH live sink, hashdb and the
// projector down until a human ran `systemctl reset-failed`.
//
// 5 minutes exceeds a MinIO restart (observed 2–3 min worst case); a permanent
// fault (bad credentials, deleted bucket) still surfaces inside the cursor-lag
// alerts. It does NOT slow the tip: a not-yet-written ledger never consumes an
// attempt.
const liveTailRetryBudget = 5 * time.Minute

// LedgerstreamConfig builds a ledgerstream.Config pointing at one
// galexie bucket. Pass cfg.Storage.S3BucketArchive for historical
// reads (ledger < seam) or S3BucketLive for the live tail.
//
// Only S3/MinIO is wired today; Filesystem is reserved for tests,
// GCS for a hypothetical cloud deploy.
//
// When cfg.Storage.ColdTieringEnabled() (ADR-0027 — the cold-tier
// fields populated in TOML), the returned Config also carries a
// ColdDataStore pointing at the cold-tier bucket. ledgerstream's
// TieredDataStore then transparently falls back to cold on
// hot-side NoSuchKey. Only the **archive** bucket gets the
// tiering treatment — galexie-live is the rolling near-tip
// working set and never needs a cold fallback. Caller passes the
// archive bucket as `bucket` to opt the cold path in; passing
// the live bucket leaves ColdDataStore zero (single-source).
func LedgerstreamConfig(cfg config.Config, bucket string) ledgerstream.Config {
	out := ledgerstream.Config{
		DataStore: datastore.DataStoreConfig{
			Type: "S3",
			Params: map[string]string{
				"destination_bucket_path": bucket,
				"region":                  cfg.Storage.S3Region,
				"endpoint_url":            cfg.Storage.S3Endpoint,
			},
			NetworkPassphrase: cfg.Stellar.Passphrase(),
			Compression:       "zstd",
		},
		// Trailing-edge tolerance: bounded backfills routinely race
		// the live tip — Galexie writes partition files lazily, so a
		// chunk_to set hours into the future hits "object missing"
		// errors at the trailing edge. A soroban-events fill walk
		// failed exactly this way on chunk 11. Setting the
		// tolerance flag here applies it to every consumer of this
		// helper (currently: stellarindex-ops backfill, the live
		// indexer's bounded archive-then-live preamble). Has no
		// effect on unbounded streams (live tail) — those wait for
		// the file via RetryWait instead. See ledgerstream.Config
		// godoc for the delivery caveat (the SDK can drop pre-fetched
		// ledgers in the buffer race window).
		TolerateTrailingMissing: true,
	}

	// Live-tail latency + fault tolerance: the live bucket is read as
	// an unbounded stream, so shorten RetryWait (archive reads are
	// bounded and ignore it) and state the fault-tolerance budget
	// EXPLICITLY rather than inheriting whatever RetryWait × the SDK's
	// RetryLimit happens to come to. galexie-live is the only bucket
	// that gets either.
	if bucket == cfg.Storage.S3BucketLive {
		out.LiveRetryWait = liveTailRetryWait
		out.LiveRetryBudget = liveTailRetryBudget
	}

	// Tiered-read opt-in: only attach a ColdDataStore when the
	// operator has populated the cold-tier fields AND the caller
	// is reading the archive bucket (not the live tail). The live
	// tail's writer is galexie itself — it's authoritative
	// locally — so a cold fallback would be wrong.
	if cfg.Storage.ColdTieringEnabled() && bucket == cfg.Storage.S3BucketArchive {
		out.ColdDataStore = datastore.DataStoreConfig{
			Type: "S3",
			Params: map[string]string{
				"destination_bucket_path": cfg.Storage.S3ColdBucketArchive,
				"region":                  cfg.Storage.S3ColdRegion,
				"endpoint_url":            cfg.Storage.S3ColdEndpoint,
			},
			NetworkPassphrase: cfg.Stellar.Passphrase(),
			Compression:       "zstd",
		}
		// The Params above still describe the cold tier (schema
		// discovery reads them), but the CLIENT must not be built by
		// the SDK's datastore.NewDataStore: that resolves credentials
		// from the ambient AWS chain, which on r1 holds local MinIO's
		// keys (the hot tier authenticates through it) and so signs
		// every cold request to real AWS with them —
		// `InvalidAccessKeyId ... does not exist in our records`.
		// See pipeline.NewColdDataStore.
		out.ColdDataStoreFactory = func(ctx context.Context) (datastore.DataStore, error) {
			return NewColdDataStore(ctx, cfg.Storage)
		}
	}

	return out
}
