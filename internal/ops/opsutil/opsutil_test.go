// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package opsutil

import (
	"testing"

	"github.com/StellarIndex/stellar-index/internal/config"
)

func testConfig() config.Config {
	cfg := config.Config{}
	cfg.Stellar.Network = "pubnet"
	cfg.Storage.S3Endpoint = "http://127.0.0.1:9000"
	cfg.Storage.S3Region = "r1"
	cfg.Storage.S3BucketArchive = "galexie-archive"
	cfg.Storage.S3BucketLive = "galexie-live"
	return cfg
}

// TestNewBoundedLedgerStreamConfig_TolerateTrailingMissingDefault is the
// critical regression: the helper exists specifically so ops
// subcommands cannot forget to opt into TolerateTrailingMissing. If
// this assertion breaks, the trailing-edge missing-file failure
// (project_62_diagnosis_2026_05_25) is back in scope for
// verify-decoders and scan-soroban-events.
func TestNewBoundedLedgerStreamConfig_TolerateTrailingMissingDefault(t *testing.T) {
	t.Parallel()
	cfg := config.Config{}
	cfg.Stellar.Network = "pubnet"
	cfg.Storage.S3Endpoint = "http://127.0.0.1:9000"
	cfg.Storage.S3Region = "r1"
	cfg.Storage.S3BucketArchive = "galexie-archive"
	cfg.Storage.S3BucketLive = "galexie-live"

	got := NewBoundedLedgerStreamConfig(cfg, cfg.Storage.S3BucketArchive, 1)
	if !got.TolerateTrailingMissing {
		t.Fatalf("TolerateTrailingMissing = false, want true")
	}
}

// TestNewBoundedLedgerStreamConfig_BucketPassThrough confirms the
// helper threads the caller's bucket into destination_bucket_path
// unmodified — verify-archive uses bucket-by-parameter while
// verify-decoders uses cfg.Storage.S3BucketLive, so the helper
// cannot pick the bucket itself.
func TestNewBoundedLedgerStreamConfig_BucketPassThrough(t *testing.T) {
	t.Parallel()
	cfg := config.Config{}
	cfg.Stellar.Network = "pubnet"
	cfg.Storage.S3Endpoint = "http://127.0.0.1:9000"
	cfg.Storage.S3Region = "r1"
	cfg.Storage.S3BucketArchive = "galexie-archive"
	cfg.Storage.S3BucketLive = "galexie-live"

	for _, want := range []string{"galexie-archive", "galexie-live", "custom-override"} {
		got := NewBoundedLedgerStreamConfig(cfg, want, 1)
		if got.DataStore.Params["destination_bucket_path"] != want {
			t.Errorf("bucket=%q -> destination_bucket_path=%q, want %q",
				want, got.DataStore.Params["destination_bucket_path"], want)
		}
		if got.DataStore.Params["region"] != cfg.Storage.S3Region {
			t.Errorf("region = %q, want %q",
				got.DataStore.Params["region"], cfg.Storage.S3Region)
		}
		if got.DataStore.Params["endpoint_url"] != cfg.Storage.S3Endpoint {
			t.Errorf("endpoint_url = %q, want %q",
				got.DataStore.Params["endpoint_url"], cfg.Storage.S3Endpoint)
		}
		if got.DataStore.Compression != "zstd" {
			t.Errorf("Compression = %q, want zstd", got.DataStore.Compression)
		}
		if got.DataStore.NetworkPassphrase != cfg.Stellar.Passphrase() {
			t.Errorf("NetworkPassphrase = %q, want %q",
				got.DataStore.NetworkPassphrase, cfg.Stellar.Passphrase())
		}
	}
}

// TestNewBoundedLedgerStreamConfig_BoundedBuffer is the regression for
// the 2026-07-15 ch-backfill -parallel OOM: the helper must return an
// explicit, small Buffered override rather than leaving Buffered nil
// (which would fall through to the SDK's
// ingest.DefaultBufferedStorageBackendConfig — BufferSize=10000,
// NumWorkers=10 for Galexie's 1-ledger-per-file schema). A nil
// Buffered here is exactly the regression: N concurrent
// ledgerstream.Stream walkers each get an independent, un-bounded
// prefetch queue and multiply memory by N.
func TestNewBoundedLedgerStreamConfig_BoundedBuffer(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	got := NewBoundedLedgerStreamConfig(cfg, cfg.Storage.S3BucketArchive, 1)
	if got.Buffered == nil {
		t.Fatalf("Buffered = nil, want an explicit bounded override (SDK default is 10000/10 — the 2026-07-15 OOM)")
	}
	if got.Buffered.BufferSize >= 10000 {
		t.Errorf("BufferSize = %d, want well below the SDK default of 10000", got.Buffered.BufferSize)
	}
	if got.Buffered.NumWorkers == 0 || got.Buffered.NumWorkers > got.Buffered.BufferSize {
		t.Errorf("NumWorkers = %d, BufferSize = %d — NewBufferedStorageBackend requires NumWorkers <= BufferSize",
			got.Buffered.NumWorkers, got.Buffered.BufferSize)
	}
}

// TestNewBoundedLedgerStreamConfig_BufferShrinksWithParallelism asserts
// the per-walker BufferSize scales DOWN as -parallel grows, so total
// buffer memory across N concurrent walkers (ch-backfill, wasm-history,
// verify-archive all split a bounded range and walk chunks
// concurrently) stays roughly constant instead of multiplying by N —
// the direct fix for the -parallel 2 / -parallel 4 OOMs.
func TestNewBoundedLedgerStreamConfig_BufferShrinksWithParallelism(t *testing.T) {
	t.Parallel()
	cfg := testConfig()

	one := NewBoundedLedgerStreamConfig(cfg, cfg.Storage.S3BucketArchive, 1)
	two := NewBoundedLedgerStreamConfig(cfg, cfg.Storage.S3BucketArchive, 2)
	four := NewBoundedLedgerStreamConfig(cfg, cfg.Storage.S3BucketArchive, 4)

	if !(one.Buffered.BufferSize > two.Buffered.BufferSize) {
		t.Errorf("BufferSize did not shrink from parallel=1 (%d) to parallel=2 (%d)",
			one.Buffered.BufferSize, two.Buffered.BufferSize)
	}
	if !(two.Buffered.BufferSize >= four.Buffered.BufferSize) {
		t.Errorf("BufferSize grew from parallel=2 (%d) to parallel=4 (%d)",
			two.Buffered.BufferSize, four.Buffered.BufferSize)
	}

	// parallel=1..4 must all stay well clear of the SDK's 10000
	// default, and NumWorkers <= BufferSize must hold at every N —
	// including a high N where BufferSize floors out.
	eight := NewBoundedLedgerStreamConfig(cfg, cfg.Storage.S3BucketArchive, 8)
	for n, got := range map[int]ledgerstreamBufferedConfigLike{
		1: {one.Buffered.BufferSize, one.Buffered.NumWorkers},
		2: {two.Buffered.BufferSize, two.Buffered.NumWorkers},
		4: {four.Buffered.BufferSize, four.Buffered.NumWorkers},
		8: {eight.Buffered.BufferSize, eight.Buffered.NumWorkers},
	} {
		if got.bufferSize >= 10000 {
			t.Errorf("parallel=%d: BufferSize = %d, want well below the SDK default of 10000", n, got.bufferSize)
		}
		if got.numWorkers == 0 || got.numWorkers > got.bufferSize {
			t.Errorf("parallel=%d: NumWorkers = %d, BufferSize = %d — violates NumWorkers <= BufferSize",
				n, got.numWorkers, got.bufferSize)
		}
	}
}

// ledgerstreamBufferedConfigLike is a tiny local tuple so the table
// test above can compare BufferSize/NumWorkers without importing the
// SDK's ledgerbackend package just for a struct literal.
type ledgerstreamBufferedConfigLike struct {
	bufferSize uint32
	numWorkers uint32
}
