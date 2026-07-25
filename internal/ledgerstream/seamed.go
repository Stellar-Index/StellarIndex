package ledgerstream

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// StreamArchiveThenLive reads ledgers in [from, ∞) by chaining two
// Stream calls: a bounded read from `archive` covering [from, seam-1],
// then an unbounded read from `live` starting at seam.
//
// Behaviour:
//   - seam == 0 → archive bucket is unused; the call degrades to a
//     plain unbounded Stream(live, from, 0, ...). This matches the
//     pre-2026-04-26 indexer behaviour.
//   - from >= seam → all wanted data lives in the live bucket; same
//     degradation as above.
//   - from < seam → bounded archive read then unbounded live read.
//
// Restart safety: each Stream call writes the cursor inside the
// callback after every successful batch. A crash mid-archive-phase
// resumes from cursor+1 (which is still < seam) on next start, so
// the same archive→live progression replays. A crash after the
// archive phase exits but before live starts is benign — the cursor
// at that moment is seam-1; restart computes from = seam, takes the
// live-only branch, and proceeds.
//
// logger is used to mark the phase boundaries in the journal; if
// nil, the function emits no log lines.
func StreamArchiveThenLive(
	ctx context.Context,
	archive, live Config,
	from, seam uint32,
	logger *slog.Logger,
	callback func(xdr.LedgerCloseMeta) error,
) error {
	if seam == 0 || from >= seam {
		if logger != nil {
			logger.Info("ledgerstream: live-only", "from", from, "seam", seam)
		}
		return Stream(ctx, live, from, 0 /*unbounded*/, callback)
	}

	if logger != nil {
		logger.Info("ledgerstream: archive phase", "from", from, "to", seam-1)
	}
	// Track how far the archive phase actually got. Config carries
	// TolerateTrailingMissing (set for BOTH buckets by
	// pipeline.LedgerstreamConfig, because ops backfills legitimately race
	// the live tip), which makes a trailing hole return SUCCESS rather than
	// an error. Without this counter the handoff below then jumps to the
	// fixed `seam` regardless, silently skipping every ledger the archive
	// never delivered — and skipping it permanently, because the cursor
	// advances past the gap.
	//
	// Deliberately NOT fixed by disabling TolerateTrailingMissing for the
	// archive bucket in the shared helper: internal/ops/ingest/backfill.go
	// reuses that same helper against the archive bucket for fills that must
	// survive racing the live tip (the 2026-05-26 soroban-events walk failed
	// exactly that way). Narrowing the shared config would regress a
	// legitimate, battle-tested ops path to fix an indexer-only bug. The
	// check belongs here, at the one call site that has a hard seam.
	var lastArchive uint32
	archiveCB := func(lcm xdr.LedgerCloseMeta) error {
		if seq := lcm.LedgerSequence(); seq > lastArchive {
			lastArchive = seq
		}
		return callback(lcm)
	}
	if err := Stream(ctx, archive, from, seam-1, archiveCB); err != nil {
		return fmt.Errorf("archive phase [%d,%d]: %w", from, seam-1, err)
	}
	if lastArchive < seam-1 {
		// Fail LOUD rather than hand off. Continuing would advance the
		// cursor past ledgers that were never ingested, and nothing
		// downstream re-reads them: the completeness verdict is the only
		// thing that would ever notice, long after the fact.
		return fmt.Errorf(
			"archive phase [%d,%d] ended at ledger %d — %d trailing ledger(s) were never "+
				"delivered; refusing to hand off to live at %d because that would skip them "+
				"permanently (the cursor would advance past the gap). The archive bucket is "+
				"missing data, or TolerateTrailingMissing swallowed a real hole",
			from, seam-1, lastArchive, seam-1-lastArchive, seam)
	}

	if logger != nil {
		logger.Info("ledgerstream: archive phase complete; handing off to live", "seam", seam)
	}
	return Stream(ctx, live, seam, 0 /*unbounded*/, callback)
}
