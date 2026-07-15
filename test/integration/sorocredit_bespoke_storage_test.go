//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Reused public strkeys (contract / account ids — public identifiers, not
// secrets) for the sorocredit fixtures.
const (
	credCollateralA = "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE"
	credCollateralB = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
	credOwnerA      = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	credOwnerB      = "GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	credSettler     = "GBGQNZAZ54NZWZA7KGOTOZYCXEYIQGOUJK7L6EM7EJD7AQRBKO7VSXJP"
	credDebtAsset   = "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG"
)

// TestCreditWindowAnalyticsAndBespoke exercises the READ side of the
// sorocredit TVL/analytics surface (CreditWindowAnalytics) + the bespoke
// block (bespokeCredit) end-to-end through real TimescaleDB.
//
// Proves: empty-safe (nil reader + nil block on empty tables — the r1 state
// until the sorocredit projector-replay runs), i128/NUMERIC preservation of
// statement + settlement volumes (never int64), the window-scoped
// open-position proxy (an opened-and-withdrawn position is not counted
// open), the SCHEDULED-settlement labelling (NOT distress), and that the
// KPIs + recent-settlements table surface once data exists.
func TestCreditWindowAnalyticsAndBespoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty-safe: reader returns (nil, nil); bespoke block is nil (not an
	// error) so /v1/protocols/sorocredit degrades to generic analytics.
	if a, err := store.CreditWindowAnalytics(ctx, 90); err != nil {
		t.Fatalf("CreditWindowAnalytics (empty): %v", err)
	} else if a != nil {
		t.Fatalf("CreditWindowAnalytics (empty) = %+v, want nil", a)
	}
	if blk, err := store.BuildProtocolBespoke(ctx, "sorocredit", "lending", 90); err != nil {
		t.Fatalf("BuildProtocolBespoke sorocredit (empty): %v", err)
	} else if blk != nil {
		t.Fatalf("BuildProtocolBespoke sorocredit (empty) = %+v, want nil", blk)
	}

	base := time.Now().UTC().Add(-24 * time.Hour)

	// Huge i128 volumes to prove NUMERIC round-trips exactly (> 2^63).
	stmtAmt1, _ := new(big.Int).SetString("12345678901234567890", 10)
	stmtAmt2 := big.NewInt(1_000_000)
	setAmt1, _ := new(big.Int).SetString("98765432109876543210987654321", 10)
	setAmt2 := big.NewInt(5_000_000)
	wdAmt := big.NewInt(250_000)
	wantStmtVol := new(big.Int).Add(stmtAmt1, stmtAmt2)
	wantSetVol := new(big.Int).Add(setAmt1, setAmt2)

	// Position A — opened, never withdrawn (stays "open" in the window).
	// Position B — opened AND cashed out via a Withdrawal (not "open").
	for _, p := range []timescale.CreditPosition{
		{CollateralContract: credCollateralA, PositionUUID: "uuid-A", PositionName: "Collateral-uuid-A", Owner: credOwnerA, Ledger: 61_700_000, LedgerCloseTime: base, TxHash: pad64("a", 0), OpIndex: 0, EventIndex: 0},
		{CollateralContract: credCollateralB, PositionUUID: "uuid-B", PositionName: "Collateral-uuid-B", Owner: credOwnerB, Ledger: 61_700_001, LedgerCloseTime: base.Add(time.Minute), TxHash: pad64("a", 1), OpIndex: 0, EventIndex: 0},
	} {
		if err := store.InsertCreditPosition(ctx, p); err != nil {
			t.Fatalf("InsertCreditPosition %s: %v", p.CollateralContract, err)
		}
	}

	for _, st := range []timescale.CreditStatement{
		{StatementUUID: "stmt-1", PositionUUID: "uuid-A", CollateralContract: credCollateralA, Amount: stmtAmt1.String(), StatementTime: base, Ledger: 61_700_010, LedgerCloseTime: base.Add(2 * time.Minute), TxHash: pad64("b", 0), OpIndex: 0, EventIndex: 0},
		{StatementUUID: "stmt-2", PositionUUID: "uuid-B", CollateralContract: credCollateralB, Amount: stmtAmt2.String(), StatementTime: base, Ledger: 61_700_011, LedgerCloseTime: base.Add(3 * time.Minute), TxHash: pad64("b", 1), OpIndex: 0, EventIndex: 0},
	} {
		if err := store.InsertCreditStatement(ctx, st); err != nil {
			t.Fatalf("InsertCreditStatement %s: %v", st.StatementUUID, err)
		}
	}

	for _, se := range []timescale.CreditSettlement{
		{CollateralContract: credCollateralA, PositionUUID: "uuid-A", StatementUUID: "stmt-1", SettlerAccount: credSettler, DebtAsset: credDebtAsset, SettledAmount: setAmt1.String(), Ledger: 61_700_020, LedgerCloseTime: base.Add(4 * time.Minute), TxHash: pad64("c", 0), OpIndex: 0, EventIndex: 0},
		{CollateralContract: credCollateralB, PositionUUID: "uuid-B", StatementUUID: "stmt-2", SettlerAccount: credSettler, DebtAsset: credDebtAsset, SettledAmount: setAmt2.String(), Ledger: 61_700_021, LedgerCloseTime: base.Add(5 * time.Minute), TxHash: pad64("c", 1), OpIndex: 0, EventIndex: 0},
	} {
		if err := store.InsertCreditSettlement(ctx, se); err != nil {
			t.Fatalf("InsertCreditSettlement %s: %v", se.PositionUUID, err)
		}
	}

	// Withdrawal on position B → B is no longer "open" in the window.
	if err := store.InsertCreditEvent(ctx, timescale.CreditEvent{
		EventType: "withdrawal", CollateralContract: credCollateralB, Asset: credDebtAsset, Account: credOwnerB,
		Amount: wdAmt.String(), Ledger: 61_700_030, LedgerCloseTime: base.Add(6 * time.Minute), TxHash: pad64("d", 0), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditEvent withdrawal: %v", err)
	}

	a, err := store.CreditWindowAnalytics(ctx, 90)
	if err != nil {
		t.Fatalf("CreditWindowAnalytics: %v", err)
	}
	if a == nil {
		t.Fatal("CreditWindowAnalytics = nil, want summary")
	}
	if a.PositionsOpened != 2 {
		t.Errorf("PositionsOpened = %d, want 2", a.PositionsOpened)
	}
	if a.OpenPositions != 1 {
		t.Errorf("OpenPositions = %d, want 1 (B was withdrawn in the window)", a.OpenPositions)
	}
	if a.UniqueUsers != 2 {
		t.Errorf("UniqueUsers = %d, want 2", a.UniqueUsers)
	}
	if a.Statements != 2 {
		t.Errorf("Statements = %d, want 2", a.Statements)
	}
	if a.StatementVolume.BigInt().Cmp(wantStmtVol) != 0 {
		t.Errorf("StatementVolume = %s, want %s — i128/NUMERIC lost precision", a.StatementVolume, wantStmtVol)
	}
	if a.Settlements != 2 {
		t.Errorf("Settlements = %d, want 2", a.Settlements)
	}
	if a.SettlementVolume.BigInt().Cmp(wantSetVol) != 0 {
		t.Errorf("SettlementVolume = %s, want %s — i128/NUMERIC lost precision", a.SettlementVolume, wantSetVol)
	}
	if a.Withdrawals != 1 {
		t.Errorf("Withdrawals = %d, want 1", a.Withdrawals)
	}
	if a.WithdrawalVolume.BigInt().Cmp(wdAmt) != 0 {
		t.Errorf("WithdrawalVolume = %s, want %s", a.WithdrawalVolume, wdAmt)
	}

	blk, err := store.BuildProtocolBespoke(ctx, "sorocredit", "lending", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke sorocredit: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke sorocredit = nil, want block")
	}
	kpis := map[string]string{}
	for _, k := range blk.KPIs {
		// key on the label prefix before the window suffix
		kpis[k.Label] = k.Value
	}
	assertKPI(t, kpis, "Positions opened (90d)", "2")
	assertKPI(t, kpis, "Open positions (90d)", "1")
	assertKPI(t, kpis, "Scheduled settlements (90d)", "2")
	assertKPI(t, kpis, "Settlement volume (90d)", wantSetVol.String())

	var hasSettleTable bool
	for _, tb := range blk.Tables {
		if tb.Title == "Recent scheduled settlements" {
			hasSettleTable = true
			if len(tb.Rows) != 2 {
				t.Errorf("recent-settlements rows = %d, want 2", len(tb.Rows))
			}
		}
	}
	if !hasSettleTable {
		t.Errorf("bespoke block missing 'Recent scheduled settlements' table; Tables=%+v", blk.Tables)
	}

	// The SCHEDULED-settlement honesty note must be present — never surface
	// these as distressed liquidations.
	var hasNote bool
	for _, n := range blk.Notes {
		if strings.Contains(n, "NOT distressed liquidations") {
			hasNote = true
		}
	}
	if !hasNote {
		t.Errorf("bespoke block missing the 'NOT distressed liquidations' settlement note; Notes=%+v", blk.Notes)
	}
}

// assertKPI fails the test if the KPI label is absent or its value differs.
func assertKPI(t *testing.T, kpis map[string]string, label, want string) {
	t.Helper()
	got, ok := kpis[label]
	if !ok {
		t.Errorf("KPI %q absent", label)
		return
	}
	if got != want {
		t.Errorf("KPI %q = %q, want %q", label, got, want)
	}
}
