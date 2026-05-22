package pipeline

import (
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/RatesEngine/rates-engine/internal/config"
	"github.com/RatesEngine/rates-engine/internal/ledgerstream"
)

// liveTailRetryWait shortens the SDK BufferedStorageBackend's 30s
// default RetryWait for the live-tail stream. galexie uploads a new
// LCM roughly every ~5s; with the 30s default a caught-up fetch
// worker sleeps a full 30s between re-checks, making end-to-end
// ingest lag sawtooth 0→30s. 3s keeps the worker re-checking
// promptly without hammering MinIO. See ledgerstream.Config.LiveRetryWait.
const liveTailRetryWait = 3 * time.Second

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
	}

	// Live-tail latency: the live bucket is read as an unbounded
	// stream, so shorten RetryWait (archive reads are bounded and
	// ignore it). galexie-live is the only bucket that gets this.
	if bucket == cfg.Storage.S3BucketLive {
		out.LiveRetryWait = liveTailRetryWait
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
	}

	return out
}
