package main

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/Stellar-Index/StellarIndex/internal/hashdb"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
)

// hashDBWindowRecent is the stellarindex_hashdb_verify_runs_total "window" label value.
const hashDBWindowRecent = "recent"

// hashDBVerifySweep runs one verify pass and records its outcome.
// Split out of startHashDBVerifier so the ticker-plumbing and the
// actual-work are independently readable (matches the
// RunRoutedViaTagger / sweep() split in internal/pipeline/routedvia.go).
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
