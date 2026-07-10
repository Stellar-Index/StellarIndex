package classicmovements

import (
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/dispatcher"
)

// mkAccount returns a valid G-strkey + corresponding xdr.AccountId
// from a seed byte. Mirrors internal/sources/sdex/decode_test.go's
// helper of the same name.
func mkAccount(t *testing.T, seed byte) (string, xdr.AccountId) {
	t.Helper()
	var pub xdr.Uint256
	pub[0] = seed
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
	s, err := strkey.Encode(strkey.VersionByteAccountID, pub[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s, aid
}

func mkAlphanum4Asset(t *testing.T, code string, issuerSeed byte) xdr.Asset {
	t.Helper()
	_, issuer := mkAccount(t, issuerSeed)
	var codeArr [4]byte
	copy(codeArr[:], code)
	return xdr.Asset{
		Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{AssetCode: codeArr, Issuer: issuer},
	}
}

func mkPaymentOp(t *testing.T, destSeed byte, asset xdr.Asset, amount int64) xdr.Operation {
	t.Helper()
	_, dest := mkAccount(t, destSeed)
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypePayment,
			PaymentOp: &xdr.PaymentOp{
				Destination: xdr.MuxedAccount{Type: xdr.CryptoKeyTypeKeyTypeEd25519, Ed25519: dest.Ed25519},
				Asset:       asset,
				Amount:      xdr.Int64(amount),
			},
		},
	}
}

func mkPaymentSuccessResult() xdr.OperationResult {
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:          xdr.OperationTypePayment,
			PaymentResult: &xdr.PaymentResult{Code: xdr.PaymentResultCodePaymentSuccess},
		},
	}
}

func mkCreateAccountOp(t *testing.T, destSeed byte, startingBalance int64) xdr.Operation {
	t.Helper()
	_, dest := mkAccount(t, destSeed)
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeCreateAccount,
			CreateAccountOp: &xdr.CreateAccountOp{
				Destination:     dest,
				StartingBalance: xdr.Int64(startingBalance),
			},
		},
	}
}

func mkCreateAccountSuccessResult() xdr.OperationResult {
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:                xdr.OperationTypeCreateAccount,
			CreateAccountResult: &xdr.CreateAccountResult{Code: xdr.CreateAccountResultCodeCreateAccountSuccess},
		},
	}
}

func TestDecoder_payment_roundTrip(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x01)
	destAddr, _ := mkAccount(t, 0x02)
	asset := mkAlphanum4Asset(t, "USDC", 0x03)
	op := mkPaymentOp(t, 0x02, asset, 500_0000000)
	result := mkPaymentSuccessResult()
	closedAt := time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC)

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Ledger:   40_000_000,
		ClosedAt: closedAt,
		TxHash:   "deadbeef",
		TxSource: fromAddr,
		OpIndex:  2,
		Op:       op,
		OpResult: result,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	ev, ok := outs[0].(MovementEvent)
	if !ok {
		t.Fatalf("output is %T, want MovementEvent", outs[0])
	}
	m := ev.Movement
	if m.Kind != KindPayment {
		t.Errorf("Kind = %q, want %q", m.Kind, KindPayment)
	}
	if m.Provenance != ProvenanceClassicDerived {
		t.Errorf("Provenance = %q, want %q", m.Provenance, ProvenanceClassicDerived)
	}
	if m.Ledger != 40_000_000 || m.TxHash != "deadbeef" || m.OpIndex != 2 || m.LegIndex != 0 {
		t.Errorf("identity fields wrong: %+v", m)
	}
	if !m.LedgerCloseTime.Equal(closedAt) {
		t.Errorf("LedgerCloseTime = %v, want %v", m.LedgerCloseTime, closedAt)
	}
	if m.Asset != "USDC-"+asset.MustAlphaNum4().Issuer.Address() {
		t.Errorf("Asset = %q", m.Asset)
	}
	if m.Amount.String() != "5000000000" {
		t.Errorf("Amount = %q, want 5000000000", m.Amount.String())
	}
	if m.FromAddress != fromAddr {
		t.Errorf("FromAddress = %q, want %q", m.FromAddress, fromAddr)
	}
	if m.ToAddress != destAddr {
		t.Errorf("ToAddress = %q, want %q", m.ToAddress, destAddr)
	}
	if ev.Source() != SourceName {
		t.Errorf("Source() = %q, want %q", ev.Source(), SourceName)
	}
}

func TestDecoder_payment_nativeAsset(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x10)
	op := mkPaymentOp(t, 0x11, xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, 10)
	result := mkPaymentSuccessResult()

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Op: op, OpResult: result, TxSource: fromAddr, TxHash: "tx1",
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	m := outs[0].(MovementEvent).Movement
	if m.Asset != "native" {
		t.Errorf("Asset = %q, want native", m.Asset)
	}
	if m.Amount.String() != "10" {
		t.Errorf("Amount = %q, want 10", m.Amount.String())
	}
}

func TestDecoder_createAccount_roundTrip(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x20)
	destAddr, _ := mkAccount(t, 0x21)
	op := mkCreateAccountOp(t, 0x21, 2_732_091_143)
	result := mkCreateAccountSuccessResult()

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Ledger: 40_000_000, TxHash: "tx2", TxSource: fromAddr, OpIndex: 0,
		Op: op, OpResult: result,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	m := outs[0].(MovementEvent).Movement
	if m.Kind != KindCreateAccount {
		t.Errorf("Kind = %q, want %q", m.Kind, KindCreateAccount)
	}
	if m.Asset != "native" {
		t.Errorf("Asset = %q, want native", m.Asset)
	}
	if m.Amount.String() != "2732091143" {
		t.Errorf("Amount = %q, want 2732091143", m.Amount.String())
	}
	if m.FromAddress != fromAddr || m.ToAddress != destAddr {
		t.Errorf("From/To = %q/%q, want %q/%q", m.FromAddress, m.ToAddress, fromAddr, destAddr)
	}
}

// TestDecoder_failedOp_bareCode_emitsNothing covers a tx-validation-
// layer failure (op never reached its own result union) — the
// OperationResultCodeOpNoAccount shape observed in real pre-P23
// data (see real_bytes_test.go's payment_failed_source_no_account
// case for the byte-identical production example this synthesizes).
func TestDecoder_failedOp_bareCode_emitsNothing(t *testing.T) {
	op := mkPaymentOp(t, 0x30, xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, 1)
	result := xdr.OperationResult{Code: xdr.OperationResultCodeOpNoAccount}

	outs, err := NewDecoder().Decode(dispatcher.OpContext{Op: op, OpResult: result, TxSource: "GTEST"})
	if err != nil {
		t.Fatalf("Decode on failed op: %v", err)
	}
	if len(outs) != 0 {
		t.Errorf("got %d outputs from a bare-failure-code op, want 0", len(outs))
	}
}

// TestDecoder_failedOp_innerFailure_emitsNothing covers the OTHER
// failure shape: the op reached its own result union
// (OperationResultCodeOpInner) but that union's own code is a
// failure (e.g. PAYMENT_UNDERFUNDED) — distinct code path from the
// bare-code case above (result.GetTr() succeeds here; the PaymentResult
// success-code check is what rejects it).
func TestDecoder_failedOp_innerFailure_emitsNothing(t *testing.T) {
	op := mkPaymentOp(t, 0x31, xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, 1)
	result := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:          xdr.OperationTypePayment,
			PaymentResult: &xdr.PaymentResult{Code: xdr.PaymentResultCodePaymentUnderfunded},
		},
	}

	outs, err := NewDecoder().Decode(dispatcher.OpContext{Op: op, OpResult: result, TxSource: "GTEST"})
	if err != nil {
		t.Fatalf("Decode on inner-failure op: %v", err)
	}
	if len(outs) != 0 {
		t.Errorf("got %d outputs from an underfunded payment, want 0", len(outs))
	}
}

// TestDecoder_malformedAmount_errorsLoudly pins the defensive
// ErrMalformedMovement path: a "successful" op whose body carries a
// non-positive amount should never happen on real chain data (core
// rejects it at validation), but if it ever does, Decode must fail
// loudly rather than silently emit a zero/negative-amount row that
// would violate migration 0103's `amount >= 0` CHECK... or worse,
// slip through if the check were ever loosened.
func TestDecoder_malformedAmount_errorsLoudly(t *testing.T) {
	op := mkPaymentOp(t, 0x40, xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, 0)
	result := mkPaymentSuccessResult()

	_, err := NewDecoder().Decode(dispatcher.OpContext{Op: op, OpResult: result, TxSource: "GTEST"})
	if !errors.Is(err, ErrMalformedMovement) {
		t.Errorf("err = %v, want errors.Is(err, ErrMalformedMovement)", err)
	}
}

func TestKind_IsValid(t *testing.T) {
	valid := []Kind{
		KindPayment, KindCreateAccount, KindPathPayment, KindAccountMerge,
		KindClawback, KindClaimableBalanceCreate, KindClaimableBalanceClaim,
		KindClaimableBalanceClawback, KindLiquidityPoolDeposit, KindLiquidityPoolWithdraw,
	}
	for _, k := range valid {
		if !k.IsValid() {
			t.Errorf("Kind(%q).IsValid() = false, want true", k)
		}
	}
	if Kind("bogus").IsValid() {
		t.Error(`Kind("bogus").IsValid() = true, want false`)
	}
}

func TestProvenance_IsValid(t *testing.T) {
	if !ProvenanceClassicDerived.IsValid() || !ProvenanceCAP67Event.IsValid() {
		t.Error("both known provenance values must be valid")
	}
	if Provenance("bogus").IsValid() {
		t.Error(`Provenance("bogus").IsValid() = true, want false`)
	}
}
