package classicmovements

import (
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// ─── Phase 4 (entry-changes half): synthetic-fixture unit tests ───

func mkPoolID(seed byte) xdr.PoolId {
	var id xdr.PoolId
	id[0] = seed
	return id
}

func mkLiquidityPoolEntry(assetA, assetB xdr.Asset, reserveA, reserveB, totalShares int64) *xdr.LedgerEntry {
	return &xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeLiquidityPool,
			LiquidityPool: &xdr.LiquidityPoolEntry{
				LiquidityPoolId: mkPoolID(0x01),
				Body: xdr.LiquidityPoolEntryBody{
					Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct,
					ConstantProduct: &xdr.LiquidityPoolEntryConstantProduct{
						Params:                   xdr.LiquidityPoolConstantProductParameters{AssetA: assetA, AssetB: assetB, Fee: 30},
						ReserveA:                 xdr.Int64(reserveA),
						ReserveB:                 xdr.Int64(reserveB),
						TotalPoolShares:          xdr.Int64(totalShares),
						PoolSharesTrustLineCount: 1,
					},
				},
			},
		},
	}
}

func mkLiquidityPoolDepositOp(assetsHint xdr.PoolId, maxA, maxB int64) xdr.Operation {
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeLiquidityPoolDeposit,
			LiquidityPoolDepositOp: &xdr.LiquidityPoolDepositOp{
				LiquidityPoolId: assetsHint,
				MaxAmountA:      xdr.Int64(maxA),
				MaxAmountB:      xdr.Int64(maxB),
				MinPrice:        xdr.Price{N: 1, D: 1},
				MaxPrice:        xdr.Price{N: 1, D: 1},
			},
		},
	}
}

func mkLiquidityPoolDepositSuccessResult() xdr.OperationResult {
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:                       xdr.OperationTypeLiquidityPoolDeposit,
			LiquidityPoolDepositResult: &xdr.LiquidityPoolDepositResult{Code: xdr.LiquidityPoolDepositResultCodeLiquidityPoolDepositSuccess},
		},
	}
}

func mkLiquidityPoolWithdrawOp(poolID xdr.PoolId, shares, minA, minB int64) xdr.Operation {
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeLiquidityPoolWithdraw,
			LiquidityPoolWithdrawOp: &xdr.LiquidityPoolWithdrawOp{
				LiquidityPoolId: poolID,
				Amount:          xdr.Int64(shares),
				MinAmountA:      xdr.Int64(minA),
				MinAmountB:      xdr.Int64(minB),
			},
		},
	}
}

func mkLiquidityPoolWithdrawSuccessResult() xdr.OperationResult {
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:                        xdr.OperationTypeLiquidityPoolWithdraw,
			LiquidityPoolWithdrawResult: &xdr.LiquidityPoolWithdrawResult{Code: xdr.LiquidityPoolWithdrawResultCodeLiquidityPoolWithdrawSuccess},
		},
	}
}

func TestDecodeLiquidityPoolOp_deposit_success(t *testing.T) {
	depositorAddr, _ := mkAccount(t, 0x90)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x91)
	op := mkLiquidityPoolDepositOp(mkPoolID(0x01), 1000, 5000)
	result := mkLiquidityPoolDepositSuccessResult()
	changes := []EntryChangeXDR{
		{ChangeType: "state", Entry: mkLiquidityPoolEntry(native, usdc, 10_000, 50_000, 1000)},
		{ChangeType: "updated", Entry: mkLiquidityPoolEntry(native, usdc, 10_800, 54_000, 1080)},
	}

	movements, err := DecodeLiquidityPoolOp(40_000_000, time.Time{}, "tx1", 0, depositorAddr, op, result, changes)
	if err != nil {
		t.Fatalf("DecodeLiquidityPoolOp: %v", err)
	}
	if len(movements) != 2 {
		t.Fatalf("got %d movements, want 2", len(movements))
	}
	if movements[0].Kind != KindLiquidityPoolDeposit || movements[1].Kind != KindLiquidityPoolDeposit {
		t.Errorf("Kind = %q/%q, want %q", movements[0].Kind, movements[1].Kind, KindLiquidityPoolDeposit)
	}
	if movements[0].LegIndex != 0 || movements[1].LegIndex != 1 {
		t.Errorf("LegIndex = %d/%d, want 0/1", movements[0].LegIndex, movements[1].LegIndex)
	}
	if movements[0].Asset != "native" || movements[0].Amount.String() != "800" {
		t.Errorf("leg0 = %s %s, want native 800", movements[0].Amount.String(), movements[0].Asset)
	}
	if movements[1].Amount.String() != "4000" {
		t.Errorf("leg1 amount = %s, want 4000", movements[1].Amount.String())
	}
	if movements[0].FromAddress != depositorAddr || movements[0].ToAddress != "" {
		t.Errorf("From/To = %q/%q, want %q/\"\"", movements[0].FromAddress, movements[0].ToAddress, depositorAddr)
	}
	if movements[0].Attributes["pool_id"] == "" {
		t.Error("pool_id attribute is empty")
	}
}

// TestDecodeLiquidityPoolOp_deposit_newPool covers the "no preceding
// state" case: a deposit into a pool that didn't exist before this
// op — before is implicitly zero-reserve, not a fidelity gap.
func TestDecodeLiquidityPoolOp_deposit_newPool(t *testing.T) {
	depositorAddr, _ := mkAccount(t, 0x92)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	aqua := mkAlphanum4Asset(t, "AQUA", 0x93)
	op := mkLiquidityPoolDepositOp(mkPoolID(0x01), 1000, 5000)
	result := mkLiquidityPoolDepositSuccessResult()
	changes := []EntryChangeXDR{
		{ChangeType: "created", Entry: mkLiquidityPoolEntry(native, aqua, 1000, 5000, 1000)},
	}

	movements, err := DecodeLiquidityPoolOp(40_000_000, time.Time{}, "tx2", 0, depositorAddr, op, result, changes)
	if err != nil {
		t.Fatalf("DecodeLiquidityPoolOp: %v", err)
	}
	if len(movements) != 2 {
		t.Fatalf("got %d movements, want 2", len(movements))
	}
	if movements[0].Amount.String() != "1000" || movements[1].Amount.String() != "5000" {
		t.Errorf("amounts = %s/%s, want 1000/5000 (full reserve, since before is implicitly zero)",
			movements[0].Amount.String(), movements[1].Amount.String())
	}
}

func TestDecodeLiquidityPoolOp_deposit_noEntryChanges_unavailable(t *testing.T) {
	op := mkLiquidityPoolDepositOp(mkPoolID(0x01), 1000, 5000)
	result := mkLiquidityPoolDepositSuccessResult()

	_, err := DecodeLiquidityPoolOp(40_000_000, time.Time{}, "tx3", 0, "GTEST", op, result, nil)
	if !errors.Is(err, ErrEntryChangesUnavailable) {
		t.Errorf("err = %v, want errors.Is(err, ErrEntryChangesUnavailable)", err)
	}
}

func TestDecodeLiquidityPoolOp_withdraw_success(t *testing.T) {
	withdrawerAddr, _ := mkAccount(t, 0x94)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x95)
	poolID := mkPoolID(0x02)
	op := mkLiquidityPoolWithdrawOp(poolID, 108, 700, 3500)
	result := mkLiquidityPoolWithdrawSuccessResult()
	changes := []EntryChangeXDR{
		{ChangeType: "state", Entry: mkLiquidityPoolEntry(native, usdc, 10_800, 54_000, 1080)},
		{ChangeType: "updated", Entry: mkLiquidityPoolEntry(native, usdc, 10_000, 50_000, 1000)},
	}

	movements, err := DecodeLiquidityPoolOp(40_000_000, time.Time{}, "tx4", 0, withdrawerAddr, op, result, changes)
	if err != nil {
		t.Fatalf("DecodeLiquidityPoolOp: %v", err)
	}
	if len(movements) != 2 {
		t.Fatalf("got %d movements, want 2", len(movements))
	}
	if movements[0].Kind != KindLiquidityPoolWithdraw {
		t.Errorf("Kind = %q, want %q", movements[0].Kind, KindLiquidityPoolWithdraw)
	}
	if movements[0].Amount.String() != "800" || movements[1].Amount.String() != "4000" {
		t.Errorf("amounts = %s/%s, want 800/4000", movements[0].Amount.String(), movements[1].Amount.String())
	}
	if movements[0].FromAddress != "" || movements[0].ToAddress != withdrawerAddr {
		t.Errorf("From/To = %q/%q, want \"\"/%q", movements[0].FromAddress, movements[0].ToAddress, withdrawerAddr)
	}
}

// TestDecodeLiquidityPoolOp_withdraw_missingBefore_unavailable covers
// the withdraw-specific rule: unlike deposit, a withdraw ALWAYS acts
// on a pre-existing pool, so a missing 'state' row (only 'updated'
// present) is itself a fidelity gap, not a valid "new pool" case.
func TestDecodeLiquidityPoolOp_withdraw_missingBefore_unavailable(t *testing.T) {
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x96)
	op := mkLiquidityPoolWithdrawOp(mkPoolID(0x02), 108, 700, 3500)
	result := mkLiquidityPoolWithdrawSuccessResult()
	changes := []EntryChangeXDR{
		{ChangeType: "updated", Entry: mkLiquidityPoolEntry(native, usdc, 10_000, 50_000, 1000)},
	}

	_, err := DecodeLiquidityPoolOp(40_000_000, time.Time{}, "tx5", 0, "GTEST", op, result, changes)
	if !errors.Is(err, ErrEntryChangesUnavailable) {
		t.Errorf("err = %v, want errors.Is(err, ErrEntryChangesUnavailable)", err)
	}
}

func TestDecodeLiquidityPoolOp_failedOp_emitsNothing(t *testing.T) {
	op := mkLiquidityPoolDepositOp(mkPoolID(0x01), 1000, 5000)
	result := xdr.OperationResult{Code: xdr.OperationResultCodeOpNoAccount}

	movements, err := DecodeLiquidityPoolOp(40_000_000, time.Time{}, "tx6", 0, "GTEST", op, result, nil)
	if err != nil {
		t.Fatalf("DecodeLiquidityPoolOp: %v", err)
	}
	if len(movements) != 0 {
		t.Errorf("got %d movements from a failed op, want 0", len(movements))
	}
}

// ─── CAP-0038 revocation edge case ─────────────────────────────────

func mkAllowTrustOp(t *testing.T, trustorSeed byte, code string, authorize uint32) xdr.Operation {
	t.Helper()
	_, trustor := mkAccount(t, trustorSeed)
	var codeArr xdr.AssetCode4
	copy(codeArr[:], code)
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeAllowTrust,
			AllowTrustOp: &xdr.AllowTrustOp{
				Trustor:   trustor,
				Asset:     xdr.AssetCode{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AssetCode4: &codeArr},
				Authorize: xdr.Uint32(authorize),
			},
		},
	}
}

func mkAllowTrustSuccessResult() xdr.OperationResult {
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:             xdr.OperationTypeAllowTrust,
			AllowTrustResult: &xdr.AllowTrustResult{Code: xdr.AllowTrustResultCodeAllowTrustSuccess},
		},
	}
}

func mkSetTrustLineFlagsOp(t *testing.T, trustorSeed byte, asset xdr.Asset, clearFlags uint32) xdr.Operation {
	t.Helper()
	_, trustor := mkAccount(t, trustorSeed)
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeSetTrustLineFlags,
			SetTrustLineFlagsOp: &xdr.SetTrustLineFlagsOp{
				Trustor:    trustor,
				Asset:      asset,
				ClearFlags: xdr.Uint32(clearFlags),
			},
		},
	}
}

func mkSetTrustLineFlagsSuccessResult() xdr.OperationResult {
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:                    xdr.OperationTypeSetTrustLineFlags,
			SetTrustLineFlagsResult: &xdr.SetTrustLineFlagsResult{Code: xdr.SetTrustLineFlagsResultCodeSetTrustLineFlagsSuccess},
		},
	}
}

func mkClaimableBalanceCreatedChange(t *testing.T, balanceIDSeed byte, asset xdr.Asset, amount int64) EntryChangeXDR {
	t.Helper()
	var h xdr.Hash
	h[0] = balanceIDSeed
	return EntryChangeXDR{
		ChangeType: "created",
		Entry: &xdr.LedgerEntry{
			Data: xdr.LedgerEntryData{
				Type: xdr.LedgerEntryTypeClaimableBalance,
				ClaimableBalance: &xdr.ClaimableBalanceEntry{
					BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
					Asset:     asset,
					Amount:    xdr.Int64(amount),
				},
			},
		},
	}
}

// TestDecodeCAP0038Revocation_noLiquidation_emitsNothing is the
// overwhelmingly common case: a successful AllowTrust with no
// correlated claimable_balance creation — not an error, not
// ErrEntryChangesUnavailable (that sentinel is LP-deposit/withdraw
// specific; this decode path uses zero movements to mean "nothing
// happened here" the SAME way an ordinary flag change reads).
func TestDecodeCAP0038Revocation_noLiquidation_emitsNothing(t *testing.T) {
	op := mkAllowTrustOp(t, 0x97, "USDC", 0)
	result := mkAllowTrustSuccessResult()

	movements, err := DecodeCAP0038Revocation(40_000_000, time.Time{}, "tx7", 0, op, result, nil)
	if err != nil {
		t.Fatalf("DecodeCAP0038Revocation: %v", err)
	}
	if len(movements) != 0 {
		t.Errorf("got %d movements, want 0 (no liquidation)", len(movements))
	}
}

func TestDecodeCAP0038Revocation_liquidation_emitsTwoLegs(t *testing.T) {
	trustorAddr, _ := mkAccount(t, 0x98)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x99)
	op := mkAllowTrustOp(t, 0x98, "USDC", 0)
	result := mkAllowTrustSuccessResult()
	changes := []EntryChangeXDR{
		mkClaimableBalanceCreatedChange(t, 0x9A, native, 800),
		mkClaimableBalanceCreatedChange(t, 0x9B, usdc, 4000),
	}

	movements, err := DecodeCAP0038Revocation(40_000_000, time.Time{}, "tx8", 0, op, result, changes)
	if err != nil {
		t.Fatalf("DecodeCAP0038Revocation: %v", err)
	}
	if len(movements) != 2 {
		t.Fatalf("got %d movements, want 2", len(movements))
	}
	for i, m := range movements {
		if m.Kind != KindLiquidityPoolWithdraw {
			t.Errorf("movements[%d].Kind = %q, want %q", i, m.Kind, KindLiquidityPoolWithdraw)
		}
		if m.FromAddress != trustorAddr || m.ToAddress != "" {
			t.Errorf("movements[%d] From/To = %q/%q, want %q/\"\"", i, m.FromAddress, m.ToAddress, trustorAddr)
		}
		if m.Attributes["revocation"] != true {
			t.Errorf("movements[%d].Attributes[revocation] = %v, want true", i, m.Attributes["revocation"])
		}
		if m.Attributes["trigger_op_type"] != "allow_trust" {
			t.Errorf("movements[%d].Attributes[trigger_op_type] = %v, want allow_trust", i, m.Attributes["trigger_op_type"])
		}
		if m.LegIndex != uint32(i) { //nolint:gosec // i is a tiny loop index in a test.
			t.Errorf("movements[%d].LegIndex = %d, want %d", i, m.LegIndex, i)
		}
	}
	if movements[0].Amount.String() != "800" || movements[1].Amount.String() != "4000" {
		t.Errorf("amounts = %s/%s, want 800/4000", movements[0].Amount.String(), movements[1].Amount.String())
	}
}

func TestDecodeCAP0038Revocation_setTrustLineFlags_triggerType(t *testing.T) {
	trustorAddr, _ := mkAccount(t, 0x9C)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x9D)
	op := mkSetTrustLineFlagsOp(t, 0x9C, usdc, 1)
	result := mkSetTrustLineFlagsSuccessResult()
	changes := []EntryChangeXDR{
		mkClaimableBalanceCreatedChange(t, 0x9E, native, 100),
		mkClaimableBalanceCreatedChange(t, 0x9F, usdc, 200),
	}

	movements, err := DecodeCAP0038Revocation(40_000_000, time.Time{}, "tx9", 0, op, result, changes)
	if err != nil {
		t.Fatalf("DecodeCAP0038Revocation: %v", err)
	}
	if len(movements) != 2 {
		t.Fatalf("got %d movements, want 2", len(movements))
	}
	if movements[0].Attributes["trigger_op_type"] != "set_trustline_flags" {
		t.Errorf("trigger_op_type = %v, want set_trustline_flags", movements[0].Attributes["trigger_op_type"])
	}
	if movements[0].FromAddress != trustorAddr {
		t.Errorf("FromAddress = %q, want %q", movements[0].FromAddress, trustorAddr)
	}
}

func TestDecodeCAP0038Revocation_failedOp_emitsNothing(t *testing.T) {
	op := mkAllowTrustOp(t, 0x9C, "USDC", 0)
	result := xdr.OperationResult{Code: xdr.OperationResultCodeOpNoAccount}

	movements, err := DecodeCAP0038Revocation(40_000_000, time.Time{}, "tx10", 0, op, result, nil)
	if err != nil {
		t.Fatalf("DecodeCAP0038Revocation: %v", err)
	}
	if len(movements) != 0 {
		t.Errorf("got %d movements from a failed op, want 0", len(movements))
	}
}
