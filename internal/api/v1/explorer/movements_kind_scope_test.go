package explorer

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// From P23 on, the cap67 archive carries transfer movements through its
// watermark and mint, burn and clawback over its recorded supply range only,
// the Postgres tail above it `transfer` only, and no epoch carries fees or
// order-book fills. The coverage note
// must say so rather than let a partial feed — or a ?kind= filter that
// silently stops at P23 — read as complete.

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

// TestAccountMovements_SupplyKindNoteNamesArchiveRange: with the supply range
// recorded from P23, the note names P23..watermark rather than claiming the
// kind stops at P23 — and with no archive it says no post-P23 row of that
// kind is served.
func TestAccountMovements_SupplyKindNoteNamesArchiveRange(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	wm := base + 1000
	for _, kind := range []string{"mint", "burn", "clawback"} {
		reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm, supplyFrom: base, supplyThru: wm}
		view := callMovementsQuery(t, reader, &stubSEP41Tail{}, accountMovementsDefaultLimit, url.Values{"kind": {kind}})
		for _, want := range []string{fmt.Sprintf("%q", kind), fmt.Sprintf("from ledger %d through ledger %d", base, wm)} {
			if !strings.Contains(view.CoverageNote, want) {
				t.Errorf("?kind=%s note %q does not contain %q", kind, view.CoverageNote, want)
			}
		}
		if strings.Contains(view.CoverageNote, "served only before") {
			t.Errorf("?kind=%s note %q claims the kind stops at P23", kind, view.CoverageNote)
		}

		empty := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}}
		view = callMovementsQuery(t, empty, &stubSEP41Tail{}, accountMovementsDefaultLimit, url.Values{"kind": {kind}})
		if !strings.Contains(view.CoverageNote, "has not derived this kind yet") {
			t.Errorf("?kind=%s with no archive: note %q does not say the archive is not derived", kind, view.CoverageNote)
		}
	}
}

// TestMovementsKindNote_ClawbackSpansBothEpochs: classic clawback operations
// fill the pre-P23 archive and CAP-67 clawback events the post-P23 one.
func TestMovementsKindNote_ClawbackSpansBothEpochs(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	note := movementsKindNote("clawback", supplyRange{from: base, thru: base + 5})
	for _, want := range []string{fmt.Sprintf("before ledger %d", base), fmt.Sprintf("through ledger %d", base+5)} {
		if !strings.Contains(note, want) {
			t.Errorf("clawback note %q does not contain %q", note, want)
		}
	}
}

// TestMovementsCoverageNote_SupplyKindsBoundedByWatermark: the unfiltered
// note names the ledger through which the supply kinds are served.
func TestMovementsCoverageNote_SupplyKindsBoundedByWatermark(t *testing.T) {
	note := movementsCoverageNote(64_400_000, "", supplyRange{from: timescale.SEP41MovementsFloorLedger, thru: 64_400_000})
	if !strings.Contains(note, "through ledger 64400000 the archive also carries mint, burn and clawback") {
		t.Errorf("note %q does not bound the supply kinds by the archive watermark", note)
	}
}

// TestAccountMovements_SupplyNoteStopsAtTheRecordedRange: a deployment whose
// watermark was advanced before the supply kinds joined the derive has them
// only from the recorded supply floor. Neither note may claim P23 coverage
// for them, and neither may run past the page's watermark.
func TestAccountMovements_SupplyNoteStopsAtTheRecordedRange(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	wm, supplyFrom := base+1000, base+600
	reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm, supplyFrom: supplyFrom, supplyThru: wm + 500}

	view := callMovementsQuery(t, reader, &stubSEP41Tail{}, accountMovementsDefaultLimit, url.Values{})
	if strings.Contains(view.CoverageNote, "(P23) through ledger "+fmt.Sprint(wm)) {
		t.Errorf("note %q claims the supply kinds from P23 through the watermark", view.CoverageNote)
	}
	for _, want := range []string{
		fmt.Sprintf("from 2025-09-03 (P23) through ledger %d this feed carries transfer movements only", supplyFrom-1),
		fmt.Sprintf("from ledger %d through ledger %d the archive also carries mint, burn and clawback", supplyFrom, wm),
	} {
		if !strings.Contains(view.CoverageNote, want) {
			t.Errorf("note %q does not contain %q", view.CoverageNote, want)
		}
	}

	for _, kind := range []string{"mint", "burn", "clawback"} {
		view = callMovementsQuery(t, reader, &stubSEP41Tail{}, accountMovementsDefaultLimit, url.Values{"kind": {kind}})
		if strings.Contains(view.CoverageNote, fmt.Sprintf("from ledger %d through", base)) {
			t.Errorf("?kind=%s note %q claims the kind from P23", kind, view.CoverageNote)
		}
		for _, want := range []string{
			fmt.Sprintf("from ledger %d through ledger %d", supplyFrom, wm),
			fmt.Sprintf("ledgers %d through %d (from P23) are still being backfilled", base, supplyFrom-1),
		} {
			if !strings.Contains(view.CoverageNote, want) {
				t.Errorf("?kind=%s note %q does not contain %q", kind, view.CoverageNote, want)
			}
		}
	}
}

// TestAccountMovements_NoSupplyRangeClaimsNoSupplyKinds: a watermark with no
// recorded supply range (the derive has not yet run a supply window) serves
// no post-P23 mint, burn or clawback, whatever the watermark.
func TestAccountMovements_NoSupplyRangeClaimsNoSupplyKinds(t *testing.T) {
	reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: timescale.SEP41MovementsFloorLedger + 1000}
	view := callMovementsQuery(t, reader, &stubSEP41Tail{}, accountMovementsDefaultLimit, url.Values{})
	if strings.Contains(view.CoverageNote, "also carries mint") || !strings.Contains(view.CoverageNote, "are not served yet") {
		t.Errorf("note %q claims supply-kind coverage with no recorded supply range", view.CoverageNote)
	}
	view = callMovementsQuery(t, reader, &stubSEP41Tail{}, accountMovementsDefaultLimit, url.Values{"kind": {"burn"}})
	if !strings.Contains(view.CoverageNote, "has not derived this kind yet") {
		t.Errorf("?kind=burn note %q does not say the kind is not derived", view.CoverageNote)
	}
}

func TestMovementsCoverage_StructuredKinds(t *testing.T) {
	floor := timescale.SEP41MovementsFloorLedger
	byKind := func(c []MovementKindCoverage) map[string]MovementKindCoverage {
		m := map[string]MovementKindCoverage{}
		for _, k := range c {
			m[k.Kind] = k
		}
		return m
	}
	cases := []struct {
		name         string
		wm           uint32
		tail         string
		supply       supplyRange
		wantTransfer string
		wantSupply   string
	}{
		{"no archive", 0, "", supplyRange{}, "partial", "not_served"},
		{"full", floor + 100, "", supplyRange{from: floor, thru: floor + 100}, "served", "served"},
		{"supply backfilling", floor + 100, "", supplyRange{from: floor + 40, thru: floor + 100}, "served", "partial"},
		{"tail errored", floor + 100, "tail down", supplyRange{from: floor, thru: floor + 100}, "partial", "served"},
	}
	for _, tc := range cases {
		got := byKind(movementsCoverage(tc.wm, tc.tail, tc.supply))
		if got["transfer"].Status != tc.wantTransfer || got["mint_burn_clawback"].Status != tc.wantSupply {
			t.Errorf("%s: transfer=%q supply=%q, want %q/%q", tc.name, got["transfer"].Status, got["mint_burn_clawback"].Status, tc.wantTransfer, tc.wantSupply)
		}
		if got["fee"].Status != "not_served" || got["fill"].Status != "not_served" {
			t.Errorf("%s: fee/fill must stay not_served: %+v", tc.name, got)
		}
	}
	if s := byKind(movementsCoverage(floor+100, "", supplyRange{from: floor, thru: floor + 100}))["mint_burn_clawback"]; s.ThroughLedger != floor+100 {
		t.Errorf("supply through = %d, want %d", s.ThroughLedger, floor+100)
	}
}
