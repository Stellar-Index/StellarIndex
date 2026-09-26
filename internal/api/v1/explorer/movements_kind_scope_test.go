package explorer

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// From P23 on, both the cap67 archive and the Postgres tail carry only
// `transfer` movements, and no epoch carries fees or order-book fills
// (GH-1061). The coverage note must say so rather than let a partial feed —
// or a ?kind= filter that silently stops at P23 — read as complete.

// TestAccountMovements_CoverageNoteDisclosesKindGap: an unfiltered feed's
// note must name the kinds that are absent, at both archive states.
func TestAccountMovements_CoverageNoteDisclosesKindGap(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	for _, wm := range []uint32{0, base + 1000} {
		reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}
		view := callMovementsQuery(t, reader, &stubSEP41Tail{}, accountMovementsDefaultLimit, url.Values{})
		for _, want := range []string{"transfer movements only", "mint", "burn", "clawback", "fees", "order-book fills"} {
			if !strings.Contains(view.CoverageNote, want) {
				t.Errorf("wm=%d: coverage note %q does not disclose %q", wm, view.CoverageNote, want)
			}
		}
	}
}

// TestAccountMovements_NonTransferKindNoteNamesP23Cutoff: ?kind=payment
// can only return pre-P23 rows; the note must say the results stop there
// instead of the generic "all assets through ledger N" claim.
func TestAccountMovements_NonTransferKindNoteNamesP23Cutoff(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: base + 1000}
	view := callMovementsQuery(t, reader, &stubSEP41Tail{}, accountMovementsDefaultLimit, url.Values{"kind": {"payment"}})

	if strings.Contains(view.CoverageNote, "all assets through ledger") {
		t.Errorf("?kind=payment note %q claims all-assets coverage through the post-P23 watermark", view.CoverageNote)
	}
	for _, want := range []string{`"payment"`, fmt.Sprintf("before ledger %d", base)} {
		if !strings.Contains(view.CoverageNote, want) {
			t.Errorf("?kind=payment note %q does not contain %q", view.CoverageNote, want)
		}
	}
}

// TestAccountMovements_TransferKindKeepsArchiveNote: ?kind=transfer is
// served at every epoch, so it keeps the archive-boundary note.
func TestAccountMovements_TransferKindKeepsArchiveNote(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: base + 1000}
	view := callMovementsQuery(t, reader, &stubSEP41Tail{}, accountMovementsDefaultLimit, url.Values{"kind": {"transfer"}})
	if !strings.Contains(view.CoverageNote, fmt.Sprintf("ledger %d", base+1000)) {
		t.Errorf("?kind=transfer note %q lost the archive boundary", view.CoverageNote)
	}
}
