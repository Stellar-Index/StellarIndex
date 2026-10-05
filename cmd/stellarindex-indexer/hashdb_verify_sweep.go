package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"

	"github.com/Stellar-Index/StellarIndex/internal/hashdb"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
)

const (
	// hashDBVerifyHistorySliceLedgers is the size of the random slice of
	// pre-window history each sweep re-verifies: 5% of the default
	// 20000-ledger window, so a sweep's stream cost grows by ~5% plus one
	// seek into the archive bucket. Without it, history older than the
	// window (~28h) is never re-verified after the live pass scrolls past it.
	hashDBVerifyHistorySliceLedgers = uint32(1000)

	// Values of the stellarindex_hashdb_verify_runs_total "window" label.
	hashDBWindowRecent  = "recent"
	hashDBWindowHistory = "history"
)

// pickHashDBHistorySlice chooses a seeded random slice of up to n ledgers
// from [start, recentFrom-1]: never below the table's first ledger, never
// overlapping the recent window beginning at recentFrom. ok is false when
// there is no history older than the recent window.
func pickHashDBHistorySlice(start, recentFrom, n uint32, seed uint64) (from, to uint32, ok bool) {
	if n == 0 || recentFrom <= start {
		return 0, 0, false
	}
	span := recentFrom - start
	if n >= span {
		return start, recentFrom - 1, true
	}
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	from = start + uint32(r.Uint64N(uint64(span-n)+1))
	return from, from + n - 1, true
}

// hashDBVerifySweep runs one verify pass and records its outcome.
// Split out of startHashDBVerifier so the ticker-plumbing and the
// actual-work are independently readable (matches the
// RunRoutedViaTagger / sweep() split in internal/pipeline/routedvia.go).
func hashDBVerifySweep(
	ctx context.Context,
	logger *slog.Logger,
	verifyDB *hashdb.DB,
	lsCfg ledgerstream.Config,
	archiveCfg ledgerstream.Config,
	lastAppended *atomic.Uint32,
	window uint32,
	seenDrifted map[uint32]struct{},
	seed int64,
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

	// History older than the window is no longer in the live bucket, so
	// the slice reads from the archive bucket. The seed and bounds ride on
	// every log line of the pass so a failing slice can be re-run with
	// -verify-hashdb-from/-to.
	if hFrom, hTo, ok := pickHashDBHistorySlice(verifyDB.StartLedger(), from, hashDBVerifyHistorySliceLedgers, uint64(seed)); ok && ctx.Err() == nil {
		hl := logger.With("slice", "history", "seed", seed)
		hl.Info("hashdb verify history slice", "from", hFrom, "to", hTo)
		hashDBVerifyPass(ctx, hl, verifyDB, archiveCfg, hFrom, hTo, seenDrifted, hashDBWindowHistory)
	}
}
