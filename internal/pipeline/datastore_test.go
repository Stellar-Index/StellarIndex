package pipeline

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// TestLedgerstreamConfig_NoColdTier verifies the legacy single-
// source path is unchanged when the operator hasn't opted in to
// the cold tier (ADR-0027). ColdDataStore must be zero-valued so
// ledgerstream.Stream takes the SDK's ApplyLedgerMetadata path.
func TestLedgerstreamConfig_NoColdTier(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Stellar: config.StellarConfig{Network: "pubnet"},
		Storage: config.StorageConfig{
			S3Endpoint:      "http://127.0.0.1:9000",
			S3Region:        "r1",
			S3BucketArchive: "galexie-archive",
			S3BucketLive:    "galexie-live",
			// S3Cold* fields intentionally left zero.
		},
	}
	got := LedgerstreamConfig(cfg, cfg.Storage.S3BucketArchive)
	if got.ColdDataStore.Type != "" {
		t.Errorf("ColdDataStore must be zero when cold tiering disabled; got Type=%q", got.ColdDataStore.Type)
	}
	if got.ColdDataStoreFactory != nil {
		t.Error("ColdDataStoreFactory must be nil when cold tiering is disabled")
	}
	if got.DataStore.Type != "S3" {
		t.Errorf("hot DataStore.Type = %q, want S3", got.DataStore.Type)
	}
	if got.DataStore.Params["destination_bucket_path"] != "galexie-archive" {
		t.Errorf("hot bucket = %q, want galexie-archive", got.DataStore.Params["destination_bucket_path"])
	}
}

// TestLedgerstreamConfig_ColdTierArchive verifies the cold-tier
// branch fires for the archive bucket when the operator has
// populated the S3Cold* fields.
func TestLedgerstreamConfig_ColdTierArchive(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Stellar: config.StellarConfig{Network: "pubnet"},
		Storage: config.StorageConfig{
			S3Endpoint:          "http://127.0.0.1:9000",
			S3Region:            "r1",
			S3BucketArchive:     "galexie-archive",
			S3BucketLive:        "galexie-live",
			S3ColdEndpoint:      "https://s3.us-east-2.amazonaws.com",
			S3ColdRegion:        "us-east-2",
			S3ColdBucketArchive: "aws-public-blockchain/v1.1/stellar/ledgers/pubnet",
		},
	}
	got := LedgerstreamConfig(cfg, cfg.Storage.S3BucketArchive)
	if got.ColdDataStore.Type != "S3" {
		t.Fatalf("ColdDataStore.Type = %q, want S3 (cold tier should have wired)", got.ColdDataStore.Type)
	}
	if got.ColdDataStore.Params["destination_bucket_path"] != "aws-public-blockchain/v1.1/stellar/ledgers/pubnet" {
		t.Errorf("cold bucket = %q", got.ColdDataStore.Params["destination_bucket_path"])
	}
	if got.ColdDataStore.Params["endpoint_url"] != "https://s3.us-east-2.amazonaws.com" {
		t.Errorf("cold endpoint = %q", got.ColdDataStore.Params["endpoint_url"])
	}
	// The cold CLIENT must come from pipeline.NewColdDataStore, not the
	// SDK's ambient-credential path — see TestColdDataStoreFactory_
	// SendsUnsignedRequests for the wire-level proof.
	if got.ColdDataStoreFactory == nil {
		t.Error("ColdDataStoreFactory not wired; ledgerstream would open cold via datastore.NewDataStore " +
			"and sign requests with the hot tier's ambient MinIO credentials (2026-07-25 incident)")
	}
}

// TestLedgerstreamConfig_ColdTierSkippedForLiveBucket: the cold tier must not attach to the live bucket, whose local near-tip data has no separate cold source of truth.
func TestLedgerstreamConfig_ColdTierSkippedForLiveBucket(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Stellar: config.StellarConfig{Network: "pubnet"},
		Storage: config.StorageConfig{
			S3Endpoint:          "http://127.0.0.1:9000",
			S3Region:            "r1",
			S3BucketArchive:     "galexie-archive",
			S3BucketLive:        "galexie-live",
			S3ColdEndpoint:      "https://s3.us-east-2.amazonaws.com",
			S3ColdRegion:        "us-east-2",
			S3ColdBucketArchive: "aws-public-blockchain/v1.1/stellar/ledgers/pubnet",
		},
	}
	got := LedgerstreamConfig(cfg, cfg.Storage.S3BucketLive)
	if got.ColdDataStore.Type != "" {
		t.Errorf("ColdDataStore must be zero for the live bucket; got Type=%q", got.ColdDataStore.Type)
	}
	if got.ColdDataStoreFactory != nil {
		t.Error("ColdDataStoreFactory must be nil for the live bucket")
	}
}

// TestLedgerstreamConfig_LiveTailRetryPolicy pins both halves of the live-tail retry policy: the NotFound fast path (LiveRetryWait, no attempt consumed) keeps lag small,
// and the fault budget (LiveRetryBudget) keeps a minutes-long MinIO restart from exiting the process. The archive bucket is a bounded read, so neither applies there.
func TestLedgerstreamConfig_LiveTailRetryPolicy(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Stellar: config.StellarConfig{Network: "pubnet"},
		Storage: config.StorageConfig{
			S3Endpoint:      "http://127.0.0.1:9000",
			S3Region:        "r1",
			S3BucketArchive: "galexie-archive",
			S3BucketLive:    "galexie-live",
		},
	}

	live := LedgerstreamConfig(cfg, cfg.Storage.S3BucketLive)
	if live.LiveRetryWait != liveTailRetryWait {
		t.Errorf("live LiveRetryWait = %v, want %v (tip-latency fast path)", live.LiveRetryWait, liveTailRetryWait)
	}
	if live.LiveRetryBudget != liveTailRetryBudget {
		t.Errorf("live LiveRetryBudget = %v, want %v", live.LiveRetryBudget, liveTailRetryBudget)
	}
	// The property that matters operationally: the tolerated outage has
	// to outlast a MinIO restart (2-3 min observed, including a config
	// apply), or the indexer exits and systemd's StartLimit parks it.
	if live.LiveRetryBudget < 3*time.Minute {
		t.Errorf("live-tail fault tolerance = %v; a MinIO restart takes 2-3 min, so this must be comfortably longer",
			live.LiveRetryBudget)
	}

	archive := LedgerstreamConfig(cfg, cfg.Storage.S3BucketArchive)
	if archive.LiveRetryWait != 0 || archive.LiveRetryBudget != 0 {
		t.Errorf("archive bucket got live-tail overrides (wait=%v budget=%v), want both zero - bounded reads keep strict semantics",
			archive.LiveRetryWait, archive.LiveRetryBudget)
	}
}

func TestStorageConfig_ColdTieringEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		s    config.StorageConfig
		want bool
	}{
		{"all empty", config.StorageConfig{}, false},
		{"endpoint only", config.StorageConfig{S3ColdEndpoint: "https://s3.us-east-2.amazonaws.com"}, false},
		{"bucket only", config.StorageConfig{S3ColdBucketArchive: "aws-public-blockchain/v1.1/stellar/ledgers/pubnet"}, true},
		{"all set", config.StorageConfig{S3ColdEndpoint: "x", S3ColdRegion: "y", S3ColdBucketArchive: "z"}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.s.ColdTieringEnabled(); got != c.want {
				t.Errorf("ColdTieringEnabled() = %v, want %v", got, c.want)
			}
		})
	}
}
