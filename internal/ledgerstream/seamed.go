package ledgerstream

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// StreamArchiveThenLive chains a bounded archive read over [from, seam-1] with an unbounded live read from seam. seam <= 1 or from >= seam skips the archive (seam==1 would hit Stream's to==0 unbounded sentinel). The cursor is written after each batch, so a crash resumes from cursor+1; nil logger disables phase logging.
func StreamArchiveThenLive(
	ctx context.Context,
	archive, live Config,
	from, seam uint32,
	logger *slog.Logger,
	callback func(xdr.LedgerCloseMeta) error,
) error {
	// seam <= 1 has no valid archive phase: [from, 0] is Stream's unbounded sentinel, so the
	// archive read would never hand off to live. Guarded for any future caller passing from==0.
	if seam <= 1 || from >= seam {
		if logger != nil {
			logger.Info("ledgerstream: live-only", "from", from, "seam", seam)
		}
		return Stream(ctx, live, from, 0 /*unbounded*/, callback)
	}

	if logger != nil {
		logger.Info("ledgerstream: archive phase", "from", from, "to", seam-1)
	}
	// Track how far the archive phase got. TolerateTrailingMissing is set for both buckets (ops
	// backfills legitimately race the live tip), so a trailing hole returns success; without this
	// counter the handoff jumps to the fixed seam and permanently skips every ledger the archive
	// never delivered, because the cursor advances past the gap.
	//
	// The check lives here, not in the shared config: ops backfill reuses that config against the
	// archive bucket and must keep tolerating a race with the live tip.
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
