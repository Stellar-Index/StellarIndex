package explorer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
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

// TestAccountMovements_StaleWhenArchiveTrailsLake pins this: the feed's
// all-assets boundary is the cap67 derive watermark, so a derive that
// stopped (or is parked at a lake hole) while the lake moved on must raise
// flags.stale — it was hardcoded false.
func TestAccountMovements_StaleWhenArchiveTrailsLake(t *testing.T) {
	wm := timescale.SEP41MovementsFloorLedger + 10_000
	if _, stale := movementsEnvelope(t, wm, wm+10*cap67MovementsStaleLedgers); !stale {
		t.Error("flags.stale = false with the archive 600 ledgers behind the lake tip, want true")
	}
	if _, stale := movementsEnvelope(t, wm, wm+5); stale {
		t.Error("flags.stale = true with the archive 5 ledgers behind the lake tip (a live follow daemon)")
	}
}

// TestAccountMovements_PinAboveLiveWatermarkRejected: a pin above the live
// boundary would ceiling CH at the pin (rows in (live, pin] not derived yet)
// and floor PG above it, dropping that range from BOTH arms.
func TestAccountMovements_PinAboveLiveWatermarkRejected(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	liveWM := base + 1000
	reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: liveWM}
	cursor := fmt.Sprintf("%d.%s.0.0.%d", base+5000, validTestTxHash, base+4000)

	code, _ := callMovementsStatus(t, reader, &stubSEP41Tail{}, cursor)
	if code != http.StatusBadRequest {
		t.Fatalf("pinned watermark %d above live %d: status %d, want 400 invalid-cursor", base+4000, liveWM, code)
	}
}

// TestAccountMovements_MaxUint32PinDoesNotDoubleList: wm=MaxUint32 wrapped
// wm+1 to 0 so the PG floor fell back to P23 while CH was ceilinged at
// MaxUint32 — every post-P23 transfer served by both arms.
func TestAccountMovements_MaxUint32PinDoesNotDoubleList(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	postP23 := base + 100
	when := time.Unix(1_700_000_000, 0).UTC()
	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        base + 50, // live archive boundary is below the row
		chRows:    []clickhouse.AccountMovementRow{pinTestCHRow(postP23)},
	}
	tail := &stubSEP41Tail{rows: []timescale.SEP41TransferRow{{
		ContractID: validTestContract, Ledger: postP23, TxHash: validTestTxHash,
		ObservedAt: when, ToAddr: validTestAccount, Amount: big.NewInt(1000),
	}}}
	cursor := fmt.Sprintf("%d.%s.0.0.%d", base+9000, validTestTxHash, uint32(math.MaxUint32))

	code, view := callMovementsStatus(t, reader, tail, cursor)
	if code == http.StatusOK && len(view.Movements) > 1 {
		t.Fatalf("pinned wm=MaxUint32 served ledger %d from both arms (%d rows) — wm+1 overflowed the PG floor to 0",
			postP23, len(view.Movements))
	}
	if code != http.StatusBadRequest {
		t.Fatalf("pinned wm=MaxUint32 above live %d: status %d, want 400 invalid-cursor", base+50, code)
	}
}

// TestAccountMovements_PinBelowFloorDoesNotHidePreP23Archive: a pin below
// the P23 boundary must not lower the CH ceiling beneath the classic
// archive's range — PG is floored at P23 regardless, so every ledger in
// (pin, P23) would be served by neither arm.
func TestAccountMovements_PinBelowFloorDoesNotHidePreP23Archive(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	preP23 := base - 10
	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        base + 1000,
		chRows:    []clickhouse.AccountMovementRow{pinTestCHRow(preP23)},
	}
	cursor := fmt.Sprintf("%d.%s.0.0.%d", base, validTestTxHash, 1)

	code, view := callMovementsStatus(t, reader, &stubSEP41Tail{}, cursor)
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if len(view.Movements) != 1 || view.Movements[0].Ledger != preP23 {
		t.Fatalf("pre-P23 archive row at ledger %d hidden by a pin of 1 (got %d rows) — CH ceiling dropped below the P23 floor",
			preP23, len(view.Movements))
	}
	if strings.Contains(view.CoverageNote, "through ledger 1 ") {
		t.Fatalf("coverage note %q claims all-assets coverage through a pre-P23 pin", view.CoverageNote)
	}
}

// TestAccountMovements_ValidPinStillHonoured: a pin at or below the live
// boundary is the legitimate case and keeps working.
func TestAccountMovements_ValidPinStillHonoured(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: base + 1300}
	cursor := fmt.Sprintf("%d.%s.0.0.%d", base+2000, validTestTxHash, base+1000)

	code, _ := callMovementsStatus(t, reader, &stubSEP41Tail{}, cursor)
	if code != http.StatusOK {
		t.Fatalf("valid pin below live watermark: status %d, want 200", code)
	}
	if !reader.gotFilter.HasMaxLedger || reader.gotFilter.MaxLedger != base+1000 {
		t.Fatalf("CH ceiling = %d (set=%v), want the pinned %d", reader.gotFilter.MaxLedger, reader.gotFilter.HasMaxLedger, base+1000)
	}
}

// TestAccountMovements_ServesPerAssetDecimals: the recent tail spans every
// SEP-41 contract, so each row must carry its own asset's scale. An
// 18-decimal token's 1.0 is 10^18 base units; rendered at a feed-wide 7 it
// reads as 100,000,000,000. A failed decimals read must omit the scale
// rather than publish a guessed 7.
func TestAccountMovements_ServesPerAssetDecimals(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC", validTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	usdcSAC, err := usdc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	const unreadableToken = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	floor := timescale.SEP41MovementsFloorLedger
	when := time.Unix(1_700_000_000, 0).UTC()
	oneUnit18, _ := new(big.Int).SetString("1000000000000000000", 10)
	tail := &scopedSEP41Tail{rows: []timescale.SEP41TransferRow{
		{ContractID: validTestContract, Ledger: floor + 3, TxHash: validTestTxHash, ObservedAt: when, FromAddr: validTestAccount, ToAddr: unreadableToken, Amount: oneUnit18},
		{ContractID: usdcSAC, Ledger: floor + 2, TxHash: validTestTxHash, ObservedAt: when, FromAddr: validTestAccount, ToAddr: validTestContract, Amount: big.NewInt(10_000_000)},
		{ContractID: unreadableToken, Ledger: floor + 1, TxHash: validTestTxHash, ObservedAt: when, FromAddr: validTestAccount, ToAddr: validTestContract, Amount: big.NewInt(5)},
	}}
	reader := &sacNamingReader{movementsArmReader: &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}}, sac: usdcSAC, name: "USDC:" + validTestAccount}

	var captured AccountMovementsView
	h := newProbeHandler(reader, nil)
	h.SEP41Movements = tail
	h.TokenDecimals = func(_ context.Context, contractID string) (int, bool) {
		if contractID == validTestContract {
			return 18, true
		}
		return 0, false
	}
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
		captured, _ = v.(AccountMovementsView)
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/movements", nil)
	r.SetPathValue("g_strkey", validTestAccount)
	h.AccountMovements(httptest.NewRecorder(), r)

	want := map[string]*int{validTestContract: intPtr(18), usdc.String(): intPtr(7), unreadableToken: nil}
	if len(captured.Movements) != len(want) {
		t.Fatalf("got %d movements, want %d: %+v", len(captured.Movements), len(want), captured.Movements)
	}
	for _, m := range captured.Movements {
		w, ok := want[m.Asset]
		if !ok {
			t.Fatalf("unexpected asset %q", m.Asset)
		}
		switch {
		case w == nil && m.Decimals != nil:
			t.Errorf("%s: decimals = %d, want omitted (read failed)", m.Asset, *m.Decimals)
		case w != nil && (m.Decimals == nil || *m.Decimals != *w):
			t.Errorf("%s: decimals = %v, want %d", m.Asset, m.Decimals, *w)
		}
	}
}

// TestAccountMovements_AssetFilterScopesPostgresTailBeforeLimit: the
// account's newest Postgres transfers are all a non-matching token and its
// USDC transfers sit below them. ?asset=USDC must return a full page of USDC
// with a cursor that reaches the rest; a filter applied after the LIMIT
// returned an empty page, no cursor, and stranded every USDC row.
func TestAccountMovements_AssetFilterScopesPostgresTailBeforeLimit(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC", validTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	sac, err := usdc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	floor := timescale.SEP41MovementsFloorLedger
	when := time.Unix(1_700_000_000, 0).UTC()
	row := func(ledger uint32, contract string) timescale.SEP41TransferRow {
		return timescale.SEP41TransferRow{
			ContractID: contract, Ledger: ledger, TxHash: validTestTxHash, ObservedAt: when,
			FromAddr: validTestAccount, ToAddr: validTestContract, Amount: big.NewInt(int64(ledger)),
		}
	}
	var rows []timescale.SEP41TransferRow
	for l := floor + 104; l >= floor+100; l-- { // newest: another token
		rows = append(rows, row(l, validTestContract))
	}
	for l := floor + 13; l >= floor+10; l-- { // older: the filtered asset
		rows = append(rows, row(l, sac))
	}
	reader := &sacNamingReader{
		movementsArmReader: &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}},
		sac:                sac,
		name:               "USDC:" + validTestAccount,
	}
	tail := &scopedSEP41Tail{rows: rows}
	q := url.Values{"asset": {usdc.String()}}

	page1 := callMovementsQuery(t, reader, tail, 3, q)
	if got := ledgersOf(page1); !slices.Equal(got, []uint32{floor + 13, floor + 12, floor + 11}) {
		t.Fatalf("page 1 ledgers = %v, want the three newest USDC transfers %v", got, []uint32{floor + 13, floor + 12, floor + 11})
	}
	for _, m := range page1.Movements {
		if m.Asset != usdc.String() {
			t.Errorf("page 1 row at ledger %d has asset %q, want %q", m.Ledger, m.Asset, usdc.String())
		}
	}
	if page1.NextCursor == "" {
		t.Fatal("page 1 is full but next_cursor is empty — the USDC transfer at the bottom is unreachable")
	}

	q.Set("cursor", page1.NextCursor)
	page2 := callMovementsQuery(t, reader, tail, 3, q)
	if got := ledgersOf(page2); !slices.Equal(got, []uint32{floor + 10}) {
		t.Fatalf("page 2 ledgers = %v, want the remaining USDC transfer [%d]", got, floor+10)
	}
	if page2.NextCursor != "" {
		t.Errorf("page 2 next_cursor = %q, want empty (history exhausted)", page2.NextCursor)
	}
}

// TestAccountMovements_WatermarkReadError_DoesNotDoubleServe pins
// that on a cap67 watermark read error with a POPULATED archive,
// the CH arm must be clamped to the static P23 boundary so its post-P23
// cap67_derived rows are NOT served alongside the Postgres tail's identical
// watched-token rows. Against the un-fixed code (wm=0 disables the CH trim)
// the same transfer is emitted twice.
func TestAccountMovements_WatermarkReadError_DoesNotDoubleServe(t *testing.T) {
	floor := timescale.SEP41MovementsFloorLedger
	postP23 := floor + 100 // a real cap67_derived movement above the P23 boundary
	when := time.Unix(1_700_000_000, 0).UTC()

	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        0,
		wmErr:     errors.New("clickhouse: cap67 watermark: connection reset"),
		chRows: []clickhouse.AccountMovementRow{{
			Address:         validTestAccount,
			Ledger:          postP23,
			LedgerCloseTime: when,
			TxHash:          validTestTxHash,
			OpIndex:         0,
			LegIndex:        0,
			Direction:       clickhouse.AccountMovementReceived,
			MovementKind:    "transfer",
			Provenance:      "cap67_derived", // the ClickHouse arm's provenance
			Asset:           "USDC-" + validTestAccount,
			Amount:          big.NewInt(1000),
		}},
	}
	tail := &stubSEP41Tail{rows: []timescale.SEP41TransferRow{{
		ContractID: validTestContract,
		Ledger:     postP23, // the SAME transfer, served by the watched-token tail
		TxHash:     validTestTxHash,
		OpIndex:    0,
		EventIndex: 0,
		ObservedAt: when,
		ToAddr:     validTestAccount,
		Amount:     big.NewInt(1000),
	}}}

	view := callMovements(t, reader, tail, "")

	for _, m := range view.Movements {
		if m.Provenance == "cap67_derived" && m.Ledger >= floor {
			t.Fatalf("post-P23 cap67_derived (ClickHouse) row at ledger %d served during a watermark-read error — "+
				"double-listed with the Postgres watched-token tail (W1-chrollup-1)", m.Ledger)
		}
	}
	if len(view.Movements) != 1 {
		t.Fatalf("expected exactly 1 movement (the Postgres watched-token row); got %d — "+
			"the CH arm was not clamped to the static P23 boundary on a watermark read error (W1-chrollup-1)",
			len(view.Movements))
	}
}

// TestAccountMovements_PaginationPinsWatermark pins the behaviour: a
// continuation page must reuse the watermark pinned into the cursor by
// page 1, NOT the live (advanced) watermark. Against the un-fixed code the
// handler re-reads the live watermark, moving the CH ceiling up and making
// the coverage note claim completeness through the higher boundary.
func TestAccountMovements_PaginationPinsWatermark(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	pinnedWM := base + 1000     // the watermark page 1 committed to
	liveWM := base + 1300       // the derive has since advanced
	nativeLedger := base + 1250 // a native row in the sliver (pinned < it < live)
	cursorLedger := base + 2000 // keyset above nativeLedger, so only the ceiling can exclude it
	when := time.Unix(1_700_000_000, 0).UTC()

	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        liveWM, // what the live read would return (used by un-fixed code)
		chRows: []clickhouse.AccountMovementRow{{
			Address:         validTestAccount,
			Ledger:          nativeLedger,
			LedgerCloseTime: when,
			TxHash:          validTestTxHash,
			OpIndex:         0,
			LegIndex:        0,
			Direction:       clickhouse.AccountMovementReceived,
			MovementKind:    "transfer",
			Provenance:      "cap67_derived",
			Asset:           "native",
			Amount:          big.NewInt(1000),
		}},
	}

	cursor := fmt.Sprintf("%d.%s.0.0.%d", cursorLedger, validTestTxHash, pinnedWM)
	view := callMovements(t, reader, &stubSEP41Tail{}, cursor)

	for _, m := range view.Movements {
		if m.Ledger == nativeLedger {
			t.Fatalf("row at ledger %d (above the PINNED page-1 watermark %d) served on a continuation page — "+
				"the CH ceiling followed the LIVE watermark %d instead of the pinned one (W1-chrollup-2)",
				nativeLedger, pinnedWM, liveWM)
		}
	}
	wantNote := fmt.Sprintf("ledger %d", pinnedWM)
	if !strings.Contains(view.CoverageNote, wantNote) {
		t.Fatalf("coverage note %q does not report the pinned watermark %d — a mid-scroll derive advance "+
			"moved the disclosed completeness boundary (W1-chrollup-2)", view.CoverageNote, pinnedWM)
	}
}

// TestAccountMovements_CeilingIsAppliedBeforeTheLimit is the
// regression: with a cap67 watermark below the account's newest rows, a
// first page must come back FULL of rows at-or-below the watermark and
// carry a next_cursor, instead of being emptied by a post-read trim.
func TestAccountMovements_CeilingIsAppliedBeforeTheLimit(t *testing.T) {
	const limit = accountMovementsDefaultLimit // what the probe handler's ParseLimit returns
	base := timescale.SEP41MovementsFloorLedger
	// The cap67 archive's watermark: the CH arm's ceiling. Post-P23, as the
	// derive only ever records one there; a sub-floor value covers no
	// post-P23 ledger and leaves the ceiling at the P23 boundary.
	wm := base + 5_000
	when := time.Unix(1_700_000_000, 0).UTC()

	// The archive holds, newest first: 40 rows ABOVE the watermark (the
	// derive is mid-window — they are not servable yet) and 30 rows below
	// it (servable, and what the reader must page through).
	var archive []clickhouse.AccountMovementRow
	row := func(ledger uint32) clickhouse.AccountMovementRow {
		return clickhouse.AccountMovementRow{
			Address:         validTestAccount,
			Ledger:          ledger,
			LedgerCloseTime: when,
			TxHash:          validTestTxHash,
			OpIndex:         0,
			LegIndex:        0,
			Direction:       clickhouse.AccountMovementReceived,
			MovementKind:    "payment",
			Provenance:      "classic_derived",
			Asset:           "native",
			Amount:          big.NewInt(1000),
		}
	}
	for i := uint32(0); i < 40; i++ {
		archive = append(archive, row(wm+40-i)) // ledgers wm+40 … wm+1
	}
	for i := uint32(0); i < 30; i++ {
		archive = append(archive, row(wm-i)) // ledgers wm … wm-29
	}

	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        wm,
		chRows:    archive,
	}

	view := callMovements(t, reader, &stubSEP41Tail{}, "")

	if len(view.Movements) != limit {
		t.Fatalf("served %d movements, want a FULL page of %d — the ceiling was applied to the page "+
			"AFTER the SQL LIMIT, so rows above the watermark ate every slot (F055)", len(view.Movements), limit)
	}
	for _, m := range view.Movements {
		if m.Ledger > wm {
			t.Fatalf("movement at ledger %d served above the cap67 watermark %d (F055)", m.Ledger, wm)
		}
	}
	if view.Movements[0].Ledger != wm {
		t.Fatalf("newest served movement is ledger %d, want %d (the highest servable row)", view.Movements[0].Ledger, wm)
	}
	if view.NextCursor == "" {
		t.Fatalf("next_cursor is empty on a full page — the account's pre-watermark history is unreachable (F055)")
	}
}

// TestAccountMovements_GenesisFloorFailsClosed pins the ceiling's
// set-signal: a deployment that installs the movements floor at genesis
// (testnet/futurenet, timescale.InstallMovementsFloor(1)) computes a
// ceiling of floor-1 == 0 whenever the cap67 watermark is absent or
// unreadable, and must then serve NOTHING from the ClickHouse arm — the
// Postgres tail covers the whole net. Carrying the clamp on a
// `MaxLedger > 0` sentinel instead of an explicit HasMaxLedger drops the
// predicate at exactly that ceiling and double-lists every transfer the
// tail already serves.
func TestAccountMovements_GenesisFloorFailsClosed(t *testing.T) {
	timescale.InstallMovementsFloor(1)
	t.Cleanup(func() { timescale.InstallMovementsFloor(0) })

	const ledger = 12_345 // a real testnet ledger: above the genesis floor, so the tail owns it
	when := time.Unix(1_700_000_000, 0).UTC()

	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        0,
		wmErr:     errors.New("clickhouse: cap67 watermark: connection reset"),
		chRows: []clickhouse.AccountMovementRow{{
			Address:         validTestAccount,
			Ledger:          ledger,
			LedgerCloseTime: when,
			TxHash:          validTestTxHash,
			Direction:       clickhouse.AccountMovementReceived,
			MovementKind:    "transfer",
			Provenance:      "cap67_derived",
			Asset:           "USDC-" + validTestAccount,
			Amount:          big.NewInt(1000),
		}},
	}
	tail := &stubSEP41Tail{rows: []timescale.SEP41TransferRow{{
		ContractID: validTestContract,
		Ledger:     ledger, // the SAME transfer, served by the watched-token tail
		TxHash:     validTestTxHash,
		ObservedAt: when,
		ToAddr:     validTestAccount,
		Amount:     big.NewInt(1000),
	}}}

	view := callMovements(t, reader, tail, "")

	if !reader.gotFilter.HasMaxLedger || reader.gotFilter.MaxLedger != 0 {
		t.Fatalf("CH arm read with filter {MaxLedger:%d HasMaxLedger:%t}, want a SET ceiling of 0 — "+
			"at an installed genesis floor the ceiling is floor-1 == 0 and must still be sent",
			reader.gotFilter.MaxLedger, reader.gotFilter.HasMaxLedger)
	}
	if len(view.Movements) != 1 {
		t.Fatalf("served %d movements, want exactly 1 (the Postgres tail row) — a ceiling of 0 must serve "+
			"nothing from the ClickHouse arm at an installed genesis floor", len(view.Movements))
	}
	if got := view.Movements[0].Provenance; got != "cap67_event" {
		t.Fatalf("served provenance %q, want cap67_event (the Postgres tail row); the ClickHouse arm was "+
			"served unclamped at ceiling 0", got)
	}
}

// TestMovementsCoverageNote_DoesNotAssertUnverifiedCompleteness pins the
// prose half of that: account_movements has no completeness verdict, so
// the note may name the archive boundary but must not call it complete.
func TestMovementsCoverageNote_DoesNotAssertUnverifiedCompleteness(t *testing.T) {
	note := movementsCoverageNote(64_400_000, "", supplyRange{})
	if strings.Contains(note, "complete for") {
		t.Errorf("note %q asserts completeness for an archive no reconcile verifies", note)
	}
	if !strings.Contains(note, "ledger 64400000") || !strings.Contains(note, "not a verified completeness verdict") {
		t.Errorf("note %q must name the boundary ledger and say it is not a verdict", note)
	}
}
