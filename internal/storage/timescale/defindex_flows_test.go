package timescale

import (
	"context"
	"testing"
	"time"
)

// Harvest must be a valid direction end to end (audit 2026-08-04
// finding 4): the DB CHECK was widened by migration 0138, and this
// Go-side whitelist was the second gate that silently rejected it.
func TestDefindexDirection_HarvestIsValid(t *testing.T) {
	if !DefindexHarvest.IsValid() {
		t.Fatal("DefindexHarvest.IsValid() = false — harvest rows would be rejected at insert despite the widened CHECK")
	}
}

// A vault-layer row with a present-but-empty AmountsVec is the
// decode/insert boundary for decodeVaultFlow's documented degenerate
// case: an on-chain zero-asset deposit/withdraw decodes cleanly
// (TestDecodeVaultFlow_emptyAmountsVec, internal/sources/defindex) and
// the insert guard must accept it rather than reject it as malformed —
// a rejection here is a plain errors.New that classifySinkFault cannot
// place in dispositionSkip, so it becomes an unclassified sink fault:
// held for a full retry budget, then quarantined, on a legitimate event.
//
// s is a zero-value Store (nil db): any code path that reaches
// s.db.ExecContext panics, which is what makes "the guard let it
// through" observable without a real database — the same technique
// TestInsertDefindexFee_guards uses for the rejection side.
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
