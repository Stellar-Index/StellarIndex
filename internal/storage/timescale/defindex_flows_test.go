package timescale

import (
	"context"
	"testing"
	"time"
)

// Harvest must be a valid direction end to end: the DB CHECK was widened by migration 0138, and this
// Go-side whitelist is the second gate that would silently reject it.
func TestDefindexDirection_HarvestIsValid(t *testing.T) {
	if !DefindexHarvest.IsValid() {
		t.Fatal("DefindexHarvest.IsValid() = false — harvest rows would be rejected at insert despite the widened CHECK")
	}
}

// A vault-layer row with an empty AmountsVec is a legitimate zero-asset deposit/withdraw, so the insert guard
// must accept it; a plain-error rejection would be held for the full retry budget and then quarantined.
// The zero-value Store panics at s.db.ExecContext, making a pass-through observable.
func TestInsertDefindexFlow_vaultEmptyAmountsVecIsAccepted(t *testing.T) {
	row := DefindexFlow{
		Ledger:          60_903_337,
		LedgerCloseTime: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		TxHash:          "vaulttx",
		ContractID:      "CA25XTGHKQ6PUMFJ4SDNRFMUABIFX46U7VAZBFDZKAOX5C3KZXUAR2KQ",
		Layer:           DefindexLayerVault,
		Direction:       DefindexDeposit,
		Actor:           "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		AmountsVec:      []string{}, // present, zero-length — the legal shape
		DfTokens:        "0",
	}
	s := &Store{}

	reachedDB := false
	func() {
		defer func() {
			if recover() != nil {
				reachedDB = true
			}
		}()
		_ = s.InsertDefindexFlow(context.Background(), row)
	}()

	if !reachedDB {
		t.Fatal("InsertDefindexFlow rejected a vault row with a present-but-empty " +
			"AmountsVec before reaching the DB call — this is the legal zero-asset " +
			"deposit/withdraw shape decodeVaultFlow documents as valid, and " +
			"rejecting it here turns a real on-chain event into an unclassified " +
			"sink fault instead of a clean insert")
	}
}

// A zero-value Store has a nil db, so any case that slips past a guard
// panics — which keeps these cases non-vacuous.
func TestInsertDefindexAdminEvent_guards(t *testing.T) {
	valid := DefindexAdminEvent{
		Ledger:          60_903_337,
		LedgerCloseTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		TxHash:          "admintx",
		ContractID:      "CA25XTGHKQ6PUMFJ4SDNRFMUABIFX46U7VAZBFDZKAOX5C3KZXUAR2KQ",
		EventKind:       "nmanager",
		NewAddress:      "GBJB3Z4SZ2E7ZSN6RZR5VTPH5OC6XWMEVQYEBVOMDP2FDGVVW52Q2PYA",
	}
	cases := []struct {
		name   string
		mutate func(*DefindexAdminEvent)
	}{
		{"empty TxHash", func(e *DefindexAdminEvent) { e.TxHash = "" }},
		{"empty ContractID", func(e *DefindexAdminEvent) { e.ContractID = "" }},
		{"zero LedgerCloseTime", func(e *DefindexAdminEvent) { e.LedgerCloseTime = time.Time{} }},
		{"unknown EventKind", func(e *DefindexAdminEvent) { e.EventKind = "rebalance" }},
	}
	s := &Store{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := valid
			tc.mutate(&row)
			if err := s.InsertDefindexAdminEvent(context.Background(), row); err == nil {
				t.Error("InsertDefindexAdminEvent accepted an invalid row, want a guard error")
			}
		})
	}
}

// The defensive pre-DB guards must reject an incomplete dfees row before touching the pool (a zero-value
// Store has a nil db, so a case slipping past a guard panics). Happy-path insert and upsert are integration-
// tested in test/integration.
func TestInsertDefindexFee_guards(t *testing.T) {
	valid := DefindexFee{
		Ledger:          60_903_337,
		LedgerCloseTime: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		TxHash:          "dfeestx",
		ContractID:      "CA25XTGHKQ6PUMFJ4SDNRFMUABIFX46U7VAZBFDZKAOX5C3KZXUAR2KQ",
		Token:           "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		Amount:          "7686",
	}
	cases := []struct {
		name   string
		mutate func(*DefindexFee)
	}{
		{"empty TxHash", func(e *DefindexFee) { e.TxHash = "" }},
		{"empty ContractID", func(e *DefindexFee) { e.ContractID = "" }},
		{"empty Token", func(e *DefindexFee) { e.Token = "" }},
		{"empty Amount", func(e *DefindexFee) { e.Amount = "" }},
		{"zero LedgerCloseTime", func(e *DefindexFee) { e.LedgerCloseTime = time.Time{} }},
	}
	s := &Store{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := valid
			tc.mutate(&row)
			if err := s.InsertDefindexFee(context.Background(), row); err == nil {
				t.Error("InsertDefindexFee accepted an invalid row, want a guard error")
			}
		})
	}
}
