package clickhouse

import (
	"math/big"
	"testing"
	"time"
)

func TestFanOutAccountMovement_TwoParticipants(t *testing.T) {
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	m := AccountMovement{
		MovementKind:    "payment",
		Provenance:      "classic_derived",
		Ledger:          100,
		LedgerCloseTime: closeTime,
		TxHash:          "txhash",
		OpIndex:         1,
		LegIndex:        0,
		Asset:           "native",
		Amount:          big.NewInt(500),
		FromAddress:     "GALICE",
		ToAddress:       "GBOB",
	}
	rows := FanOutAccountMovement(m)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}

	var sent, received *AccountMovementRow
	for i := range rows {
		switch rows[i].Direction {
		case AccountMovementSent:
			sent = &rows[i]
		case AccountMovementReceived:
			received = &rows[i]
		case AccountMovementSelf:
			t.Fatalf("unexpected 'self' direction for a two-participant movement: %+v", rows[i])
		}
	}
	if sent == nil || received == nil {
		t.Fatalf("expected one sent + one received row, got %+v", rows)
	}
	if sent.Address != "GALICE" || sent.Counterparty != "GBOB" {
		t.Errorf("sent row = %+v, want address=GALICE counterparty=GBOB", sent)
	}
	if received.Address != "GBOB" || received.Counterparty != "GALICE" {
		t.Errorf("received row = %+v, want address=GBOB counterparty=GALICE", received)
	}
	// Shared movement identity fields must be preserved on both fan-out rows.
	for _, r := range rows {
		if r.Ledger != 100 || r.TxHash != "txhash" || r.OpIndex != 1 || r.LegIndex != 0 {
			t.Errorf("row %+v lost shared movement identity", r)
		}
		if r.MovementKind != "payment" || r.Asset != "native" || r.Amount.Cmp(big.NewInt(500)) != 0 {
			t.Errorf("row %+v lost shared movement data", r)
		}
	}
}

func TestFanOutAccountMovement_SelfPayment(t *testing.T) {
	m := AccountMovement{
		MovementKind: "payment",
		Ledger:       100,
		TxHash:       "txhash",
		Asset:        "native",
		Amount:       big.NewInt(1),
		FromAddress:  "GSELF",
		ToAddress:    "GSELF",
	}
	rows := FanOutAccountMovement(m)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 (self-payment must not double-count)", len(rows))
	}
	if rows[0].Direction != AccountMovementSelf {
		t.Errorf("direction = %q, want %q", rows[0].Direction, AccountMovementSelf)
	}
	if rows[0].Address != "GSELF" {
		t.Errorf("address = %q, want GSELF", rows[0].Address)
	}
	if rows[0].Counterparty != "" {
		t.Errorf("counterparty = %q, want empty for a self row", rows[0].Counterparty)
	}
}

func TestFanOutAccountMovement_SingleParticipant_ClaimableBalanceCreate(t *testing.T) {
	// claimable_balance_create: creator known, no claimant resolved yet.
	m := AccountMovement{
		MovementKind: "claimable_balance_create",
		Ledger:       100,
		TxHash:       "txhash",
		Asset:        "native",
		Amount:       big.NewInt(10),
		FromAddress:  "GCREATOR",
		ToAddress:    "",
	}
	rows := FanOutAccountMovement(m)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Address != "GCREATOR" || rows[0].Direction != AccountMovementSent || rows[0].Counterparty != "" {
		t.Errorf("row = %+v, want address=GCREATOR direction=sent counterparty=''", rows[0])
	}
}

func TestFanOutAccountMovement_SingleParticipant_ClaimableBalanceClaim(t *testing.T) {
	// claimable_balance_claim: escrow side unknown (not a G-account), claimant known.
	m := AccountMovement{
		MovementKind: "claimable_balance_claim",
		Ledger:       100,
		TxHash:       "txhash",
		Asset:        "native",
		Amount:       big.NewInt(10),
		FromAddress:  "",
		ToAddress:    "GCLAIMANT",
	}
	rows := FanOutAccountMovement(m)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Address != "GCLAIMANT" || rows[0].Direction != AccountMovementReceived || rows[0].Counterparty != "" {
		t.Errorf("row = %+v, want address=GCLAIMANT direction=received counterparty=''", rows[0])
	}
}

func TestFanOutAccountMovement_LiquidityPoolLegs(t *testing.T) {
	// Deposit leg: depositor known ("sent" into the pool), pool side unknown.
	deposit := AccountMovement{
		MovementKind: "liquidity_pool_deposit",
		Ledger:       100,
		TxHash:       "txhash",
		LegIndex:     0,
		Asset:        "native",
		Amount:       big.NewInt(10),
		FromAddress:  "GDEPOSITOR",
		ToAddress:    "",
	}
	depositRows := FanOutAccountMovement(deposit)
	if len(depositRows) != 1 || depositRows[0].Direction != AccountMovementSent {
		t.Fatalf("deposit leg rows = %+v, want 1 sent row", depositRows)
	}

	// Withdraw leg: withdrawer known ("received" from the pool), pool side unknown.
	withdraw := AccountMovement{
		MovementKind: "liquidity_pool_withdraw",
		Ledger:       100,
		TxHash:       "txhash",
		LegIndex:     1,
		Asset:        "native",
		Amount:       big.NewInt(10),
		FromAddress:  "",
		ToAddress:    "GWITHDRAWER",
	}
	withdrawRows := FanOutAccountMovement(withdraw)
	if len(withdrawRows) != 1 || withdrawRows[0].Direction != AccountMovementReceived {
		t.Fatalf("withdraw leg rows = %+v, want 1 received row", withdrawRows)
	}
}

func TestFanOutAccountMovement_NoParticipants(t *testing.T) {
	m := AccountMovement{MovementKind: "payment", Ledger: 1, TxHash: "tx", Asset: "native", Amount: big.NewInt(1)}
	rows := FanOutAccountMovement(m)
	if rows != nil {
		t.Errorf("rows = %+v, want nil for a movement with no known participant", rows)
	}
}

func TestFanOutAccountMovement_NilAmountPreserved(t *testing.T) {
	// Amount is passed through as-is by the fan-out (nil-guarding happens at
	// insert time, not fan-out time) — assert the fan-out doesn't panic or
	// silently substitute a value.
	m := AccountMovement{
		MovementKind: "payment",
		Ledger:       1,
		TxHash:       "tx",
		Asset:        "native",
		FromAddress:  "GALICE",
		ToAddress:    "GBOB",
	}
	rows := FanOutAccountMovement(m)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Amount != nil {
			t.Errorf("row.Amount = %v, want nil (unset) preserved from input", r.Amount)
		}
	}
}

func TestMarshalAccountMovementAttributes(t *testing.T) {
	got, err := marshalAccountMovementAttributes(nil)
	if err != nil || got != "{}" {
		t.Errorf("marshalAccountMovementAttributes(nil) = %q, %v; want '{}', nil", got, err)
	}
	got, err = marshalAccountMovementAttributes(map[string]any{"balance_id": "abc"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got != `{"balance_id":"abc"}` {
		t.Errorf("marshal = %q, want {\"balance_id\":\"abc\"}", got)
	}
}

func TestSortAccountMovementRows(t *testing.T) {
	rows := []AccountMovementRow{
		{Address: "GB", Ledger: 2, TxHash: "z"},
		{Address: "GA", Ledger: 5, TxHash: "a"},
		{Address: "GA", Ledger: 1, TxHash: "b"},
	}
	sortAccountMovementRows(rows)
	if rows[0].Address != "GA" || rows[0].Ledger != 1 {
		t.Errorf("rows[0] = %+v, want address=GA ledger=1", rows[0])
	}
	if rows[1].Address != "GA" || rows[1].Ledger != 5 {
		t.Errorf("rows[1] = %+v, want address=GA ledger=5", rows[1])
	}
	if rows[2].Address != "GB" {
		t.Errorf("rows[2] = %+v, want address=GB", rows[2])
	}
}
