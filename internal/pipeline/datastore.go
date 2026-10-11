package pipeline

import (
	"context"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
)

// liveTailRetryWait shortens the SDK's 30s RetryWait for the live tail: galexie uploads about every 5s, so 30s sawtooths ingest lag.
// 500ms is a cheap local MinIO bucket LIST for the real-time latency budget. See ledgerstream.Config.LiveRetryWait.
const liveTailRetryWait = 500 * time.Millisecond

// liveTailRetryBudget is how long the live tail retries a datastore FAULT before giving up (a not-yet-written tip is retried forever, free).
// The SDK's RetryLimit=5 with 500ms RetryWait alone tolerates only 2.5s, so a MinIO restart would park the indexer in `failed`; 5m covers one. See ledgerstream.Config.LiveRetryBudget.
const liveTailRetryBudget = 5 * time.Minute

// LedgerstreamConfig builds a ledgerstream.Config for one galexie bucket: S3BucketArchive for history, S3BucketLive for the live tail.
// Only S3/MinIO is wired. With cold tiering enabled (ADR-0027), the archive bucket alone also gets a ColdDataStore; the live bucket never does.
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
		// Trailing-edge tolerance: bounded backfills race the live tip (Galexie writes lazily), hitting "object missing" at the edge.
		// Applies to every consumer of this helper; unbounded live streams wait via RetryWait instead. See ledgerstream.Config for the delivery caveat.
		TolerateTrailingMissing: true,
	}

	// Live bucket is read as an unbounded stream: shorten RetryWait and state the fault-tolerance budget explicitly. Only galexie-live gets either.
	if bucket == cfg.Storage.S3BucketLive {
		out.LiveRetryWait = liveTailRetryWait
		out.LiveRetryBudget = liveTailRetryBudget
	}

	// Attach a ColdDataStore only when cold fields are populated AND reading the archive bucket; the live tail is locally authoritative.
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
		// The CLIENT must not come from the SDK's datastore.NewDataStore: it resolves the ambient AWS chain, which on r1 holds local MinIO keys
		// and would sign every cold request to real AWS with them (InvalidAccessKeyId). See pipeline.NewColdDataStore.
		out.ColdDataStoreFactory = func(ctx context.Context) (datastore.DataStore, error) {
			return NewColdDataStore(ctx, cfg.Storage)
		}
	}

	return out
}
