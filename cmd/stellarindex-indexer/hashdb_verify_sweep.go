package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"

	"github.com/Stellar-Index/StellarIndex/internal/hashdb"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
)

// hashDBVerifyBufferLedgers caps the sweep's prefetch. The SDK default for 1-ledger files is 10,000
// ledgers, which held ~5 GB at the top of every hourly sweep and pushed the indexer past MemoryHigh.
const hashDBVerifyBufferLedgers = 256

// hashDBVerifyStreamConfig is the sweep's read config: strict on missing objects and with a bounded prefetch.
func hashDBVerifyStreamConfig(lsCfg ledgerstream.Config) ledgerstream.Config {
	lsCfg.TolerateTrailingMissing = false
	lsCfg.Buffered = &ledgerbackend.BufferedStorageBackendConfig{
		BufferSize: hashDBVerifyBufferLedgers,
		NumWorkers: 4,
		RetryLimit: 5,
		RetryWait:  30 * time.Second,
	}
	return lsCfg
}

// hashDBWindowRecent is the stellarindex_hashdb_verify_runs_total "window" label value.
const hashDBWindowRecent = "recent"

// hashDBVerifySweep runs one verify pass and records its outcome, split from startHashDBVerifier so ticker plumbing and work read
// independently.
func hashDBVerifySweep(
	ctx context.Context,
	logger *slog.Logger,
	verifyDB *hashdb.DB,
	lsCfg ledgerstream.Config,
	lastAppended *atomic.Uint32,
	window uint32,
	seenDrifted map[uint32]struct{},
) {
	tip := lastAppended.Load()
	if tip <= hashDBVerifySafetyMargin {
		// Fresh region bring-up, or restart hasn't accumulated enough
		// new appends yet — nothing durable to check.
		return
	}
	to := tip - hashDBVerifySafetyMargin

	from := verifyDB.StartLedger()
	if to > window && to-window+1 > from {
		from = to - window + 1
	}
	if from > to {
		return
	}

	hashDBVerifyPass(ctx, logger, verifyDB, lsCfg, from, to, seenDrifted, hashDBWindowRecent)
}
