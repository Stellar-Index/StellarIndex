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

// ─── Phase 2: PathPaymentStrictReceive / PathPaymentStrictSend ────

func mkOrderBookClaimAtom(t *testing.T, sellerSeed byte, soldAsset xdr.Asset, soldAmount int64, boughtAsset xdr.Asset, boughtAmount int64) xdr.ClaimAtom {
	t.Helper()
	_, seller := mkAccount(t, sellerSeed)
	return xdr.ClaimAtom{
		Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook,
		OrderBook: &xdr.ClaimOfferAtom{
			SellerId:     seller,
			OfferId:      1,
			AssetSold:    soldAsset,
			AmountSold:   xdr.Int64(soldAmount),
			AssetBought:  boughtAsset,
			AmountBought: xdr.Int64(boughtAmount),
		},
	}
}

func mkPathPaymentStrictReceiveOp(t *testing.T, sendAsset xdr.Asset, sendMax int64, destSeed byte, destAsset xdr.Asset, destAmount int64) xdr.Operation {
	t.Helper()
	_, dest := mkAccount(t, destSeed)
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypePathPaymentStrictReceive,
			PathPaymentStrictReceiveOp: &xdr.PathPaymentStrictReceiveOp{
				SendAsset:   sendAsset,
				SendMax:     xdr.Int64(sendMax),
				Destination: xdr.MuxedAccount{Type: xdr.CryptoKeyTypeKeyTypeEd25519, Ed25519: dest.Ed25519},
				DestAsset:   destAsset,
				DestAmount:  xdr.Int64(destAmount),
			},
		},
	}
}

func mkPathPaymentStrictReceiveSuccessResult(t *testing.T, destSeed byte, destAsset xdr.Asset, destAmount int64, offers []xdr.ClaimAtom) xdr.OperationResult {
	t.Helper()
	_, dest := mkAccount(t, destSeed)
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypePathPaymentStrictReceive,
			PathPaymentStrictReceiveResult: &xdr.PathPaymentStrictReceiveResult{
				Code: xdr.PathPaymentStrictReceiveResultCodePathPaymentStrictReceiveSuccess,
				Success: &xdr.PathPaymentStrictReceiveResultSuccess{
					Offers: offers,
					Last: xdr.SimplePaymentResult{
						Destination: dest,
						Asset:       destAsset,
						Amount:      xdr.Int64(destAmount),
					},
				},
			},
		},
	}
}

func mkPathPaymentStrictSendOp(t *testing.T, sendAsset xdr.Asset, sendAmount int64, destSeed byte, destAsset xdr.Asset, destMin int64) xdr.Operation {
	t.Helper()
	_, dest := mkAccount(t, destSeed)
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypePathPaymentStrictSend,
			PathPaymentStrictSendOp: &xdr.PathPaymentStrictSendOp{
				SendAsset:   sendAsset,
				SendAmount:  xdr.Int64(sendAmount),
				Destination: xdr.MuxedAccount{Type: xdr.CryptoKeyTypeKeyTypeEd25519, Ed25519: dest.Ed25519},
				DestAsset:   destAsset,
				DestMin:     xdr.Int64(destMin),
			},
		},
	}
}

func mkPathPaymentStrictSendSuccessResult(t *testing.T, destSeed byte, destAsset xdr.Asset, destAmount int64, offers []xdr.ClaimAtom) xdr.OperationResult {
	t.Helper()
	_, dest := mkAccount(t, destSeed)
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypePathPaymentStrictSend,
			PathPaymentStrictSendResult: &xdr.PathPaymentStrictSendResult{
				Code: xdr.PathPaymentStrictSendResultCodePathPaymentStrictSendSuccess,
				Success: &xdr.PathPaymentStrictSendResultSuccess{
					Offers: offers,
					Last: xdr.SimplePaymentResult{
						Destination: dest,
						Asset:       destAsset,
						Amount:      xdr.Int64(destAmount),
					},
				},
			},
		},
	}
}

// TestDecoder_pathPaymentStrictReceive_direct_noOffers covers the
// degenerate SendAsset==DestAsset case: no order book / pool
// crossed, so the source amount consumed equals exactly what was
// delivered (research §2 path (b), the len(Offers)==0 branch).
func TestDecoder_pathPaymentStrictReceive_direct_noOffers(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x50)
	destAddr, _ := mkAccount(t, 0x51)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	op := mkPathPaymentStrictReceiveOp(t, native, 100, 0x51, native, 100)
	result := mkPathPaymentStrictReceiveSuccessResult(t, 0x51, native, 100, nil)

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Ledger: 40_000_000, TxHash: "txpp1", TxSource: fromAddr, OpIndex: 0,
		Op: op, OpResult: result,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	m := outs[0].(MovementEvent).Movement
	if m.Kind != KindPathPayment {
		t.Errorf("Kind = %q, want %q", m.Kind, KindPathPayment)
	}
	if m.Asset != "native" || m.Amount.String() != "100" {
		t.Errorf("dest leg = %s %s, want native 100", m.Amount.String(), m.Asset)
	}
	if m.FromAddress != fromAddr || m.ToAddress != destAddr {
		t.Errorf("From/To = %q/%q, want %q/%q", m.FromAddress, m.ToAddress, fromAddr, destAddr)
	}
	if m.Attributes["send_asset"] != "native" || m.Attributes["send_amount"] != "100" {
		t.Errorf("Attributes = %+v, want send_asset=native send_amount=100", m.Attributes)
	}
}

// TestDecoder_pathPaymentStrictReceive_singleHop mirrors real mainnet
// shape (real_bytes_test.go's pp1/pp3/pp4): one offer, AssetBought ==
// SendAsset — source amount = that offer's AmountBought.
func TestDecoder_pathPaymentStrictReceive_singleHop(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x52)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	sony := mkAlphanum4Asset(t, "SONY", 0x53)
	offers := []xdr.ClaimAtom{
		mkOrderBookClaimAtom(t, 0x54, sony, 900000000000, native, 12_000000),
	}
	op := mkPathPaymentStrictReceiveOp(t, native, 12_120000, 0x55, sony, 900000000000)
	result := mkPathPaymentStrictReceiveSuccessResult(t, 0x55, sony, 900000000000, offers)

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Op: op, OpResult: result, TxSource: fromAddr, TxHash: "txpp2",
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	m := outs[0].(MovementEvent).Movement
	if m.Amount.String() != "900000000000" {
		t.Errorf("dest amount = %s, want 900000000000", m.Amount.String())
	}
	if m.Attributes["send_amount"] != "12000000" {
		t.Errorf("send_amount = %v, want 12000000", m.Attributes["send_amount"])
	}
}

// TestDecoder_pathPaymentStrictReceive_multiHop is the synthetic
// version of real_bytes_test.go's two-hop native→SHIB→native fixture
// — Offers[0] converts SendAsset (native) to the intermediate asset,
// Offers[1] converts the intermediate back to native (a distinct
// asset pair). The derivation must sum only the contiguous prefix
// matching SendAsset (i.e. just Offers[0]), not all offers.
func TestDecoder_pathPaymentStrictReceive_multiHop(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x56)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	shib := mkAlphanum4Asset(t, "SHIB", 0x57)
	offers := []xdr.ClaimAtom{
		mkOrderBookClaimAtom(t, 0x58, shib, 602078450074, native, 83568489), // hop0: pay native, get SHIB
		mkOrderBookClaimAtom(t, 0x59, native, 83586584, shib, 602078450074), // hop1: pay SHIB, get native
	}
	op := mkPathPaymentStrictReceiveOp(t, native, 83584774, 0x5A, native, 83586584)
	result := mkPathPaymentStrictReceiveSuccessResult(t, 0x5A, native, 83586584, offers)

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Op: op, OpResult: result, TxSource: fromAddr, TxHash: "txpp3",
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	m := outs[0].(MovementEvent).Movement
	if m.Asset != "native" || m.Amount.String() != "83586584" {
		t.Errorf("dest leg = %s %s, want native 83586584", m.Amount.String(), m.Asset)
	}
	if m.Attributes["send_asset"] != "native" || m.Attributes["send_amount"] != "83568489" {
		t.Errorf("Attributes = %+v, want send_asset=native send_amount=83568489 (hop0 only, not the 83586584 hop1 leg)", m.Attributes)
	}
}

// TestDecoder_pathPaymentStrictReceive_multiOfferSameHop covers order
// book depth: the first hop fills across TWO offers before the path
// moves to the next asset — both must be summed since both have
// AssetBought == SendAsset.
func TestDecoder_pathPaymentStrictReceive_multiOfferSameHop(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x5B)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x5C)
	offers := []xdr.ClaimAtom{
		mkOrderBookClaimAtom(t, 0x5D, usdc, 400_0000000, native, 40_000000),
		mkOrderBookClaimAtom(t, 0x5E, usdc, 100_0000000, native, 10_000000),
	}
	op := mkPathPaymentStrictReceiveOp(t, native, 50_000000, 0x5F, usdc, 500_0000000)
	result := mkPathPaymentStrictReceiveSuccessResult(t, 0x5F, usdc, 500_0000000, offers)

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Op: op, OpResult: result, TxSource: fromAddr, TxHash: "txpp4",
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	m := outs[0].(MovementEvent).Movement
	if m.Attributes["send_amount"] != "50000000" {
		t.Errorf("send_amount = %v, want 50000000 (sum of both offers)", m.Attributes["send_amount"])
	}
}

// TestDecoder_pathPaymentStrictReceive_hopOrderViolation_errorsLoudly
// pins the defensive path: if the FIRST offer's AssetBought doesn't
// match SendAsset, the hop-order assumption this package's derivation
// relies on is violated — fail loudly rather than silently deriving
// a wrong amount.
func TestDecoder_pathPaymentStrictReceive_hopOrderViolation_errorsLoudly(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x60)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x61)
	shib := mkAlphanum4Asset(t, "SHIB", 0x62)
	// First offer's AssetBought is usdc, but SendAsset is native — violates the hop-order assumption.
	offers := []xdr.ClaimAtom{
		mkOrderBookClaimAtom(t, 0x63, shib, 1000, usdc, 500),
	}
	op := mkPathPaymentStrictReceiveOp(t, native, 1000, 0x64, shib, 1000)
	result := mkPathPaymentStrictReceiveSuccessResult(t, 0x64, shib, 1000, offers)

	_, err := NewDecoder().Decode(dispatcher.OpContext{Op: op, OpResult: result, TxSource: fromAddr, TxHash: "txpp5"})
	if !errors.Is(err, ErrMalformedMovement) {
		t.Errorf("err = %v, want errors.Is(err, ErrMalformedMovement)", err)
	}
}

// TestDecoder_pathPaymentStrictSend_success pins StrictSend's simpler
// path: SendAmount is exact in the body, no Offers derivation.
func TestDecoder_pathPaymentStrictSend_success(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x65)
	destAddr, _ := mkAccount(t, 0x66)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	aqua := mkAlphanum4Asset(t, "AQUA", 0x67)
	offers := []xdr.ClaimAtom{
		mkOrderBookClaimAtom(t, 0x68, aqua, 63545, native, 1100),
	}
	op := mkPathPaymentStrictSendOp(t, native, 1100, 0x66, aqua, 60000)
	result := mkPathPaymentStrictSendSuccessResult(t, 0x66, aqua, 63545, offers)

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Op: op, OpResult: result, TxSource: fromAddr, TxHash: "txpp6",
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	m := outs[0].(MovementEvent).Movement
	if m.Kind != KindPathPayment {
		t.Errorf("Kind = %q, want %q", m.Kind, KindPathPayment)
	}
	if m.Asset != "AQUA-"+aqua.MustAlphaNum4().Issuer.Address() || m.Amount.String() != "63545" {
		t.Errorf("dest leg = %s %s", m.Amount.String(), m.Asset)
	}
	if m.Attributes["send_asset"] != "native" || m.Attributes["send_amount"] != "1100" {
		t.Errorf("Attributes = %+v, want send_asset=native send_amount=1100", m.Attributes)
	}
	if m.ToAddress != destAddr {
		t.Errorf("ToAddress = %q, want %q", m.ToAddress, destAddr)
	}
}

// TestDecoder_pathPayment_failedOp_emitsNothing covers both op types'
// failure path (bare outer code, e.g. PATH_PAYMENT_STRICT_RECEIVE
// never reaching its own result union).
func TestDecoder_pathPayment_failedOp_emitsNothing(t *testing.T) {
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	op := mkPathPaymentStrictReceiveOp(t, native, 100, 0x69, native, 100)
	result := xdr.OperationResult{Code: xdr.OperationResultCodeOpNoAccount}

	outs, err := NewDecoder().Decode(dispatcher.OpContext{Op: op, OpResult: result, TxSource: "GTEST"})
	if err != nil {
		t.Fatalf("Decode on failed op: %v", err)
	}
	if len(outs) != 0 {
		t.Errorf("got %d outputs from a failed op, want 0", len(outs))
	}
}

// ─── Phase 3: ClaimableBalance create/claim/clawback + Clawback ───

func mkClaimant(t *testing.T, destSeed byte) xdr.Claimant {
	t.Helper()
	_, dest := mkAccount(t, destSeed)
	return xdr.Claimant{
		Type: xdr.ClaimantTypeClaimantTypeV0,
		V0:   &xdr.ClaimantV0{Destination: dest, Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional}},
	}
}

func mkCreateClaimableBalanceOp(t *testing.T, asset xdr.Asset, amount int64, claimants ...xdr.Claimant) xdr.Operation {
	t.Helper()
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeCreateClaimableBalance,
			CreateClaimableBalanceOp: &xdr.CreateClaimableBalanceOp{
				Asset: asset, Amount: xdr.Int64(amount), Claimants: claimants,
			},
		},
	}
}

func mkBalanceID(seed byte) xdr.ClaimableBalanceId {
	var h xdr.Hash
	h[0] = seed
	return xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h}
}

func mkCreateClaimableBalanceSuccessResult(t *testing.T, bid xdr.ClaimableBalanceId) xdr.OperationResult {
	t.Helper()
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeCreateClaimableBalance,
			CreateClaimableBalanceResult: &xdr.CreateClaimableBalanceResult{
				Code:      xdr.CreateClaimableBalanceResultCodeCreateClaimableBalanceSuccess,
				BalanceId: &bid,
			},
		},
	}
}

func mkClaimClaimableBalanceOp(bid xdr.ClaimableBalanceId) xdr.Operation {
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type:                    xdr.OperationTypeClaimClaimableBalance,
			ClaimClaimableBalanceOp: &xdr.ClaimClaimableBalanceOp{BalanceId: bid},
		},
	}
}

func mkClaimClaimableBalanceSuccessResult() xdr.OperationResult {
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:                        xdr.OperationTypeClaimClaimableBalance,
			ClaimClaimableBalanceResult: &xdr.ClaimClaimableBalanceResult{Code: xdr.ClaimClaimableBalanceResultCodeClaimClaimableBalanceSuccess},
		},
	}
}

func mkClawbackClaimableBalanceOp(bid xdr.ClaimableBalanceId) xdr.Operation {
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type:                       xdr.OperationTypeClawbackClaimableBalance,
			ClawbackClaimableBalanceOp: &xdr.ClawbackClaimableBalanceOp{BalanceId: bid},
		},
	}
}

func mkClawbackClaimableBalanceSuccessResult() xdr.OperationResult {
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeClawbackClaimableBalance,
			ClawbackClaimableBalanceResult: &xdr.ClawbackClaimableBalanceResult{
				Code: xdr.ClawbackClaimableBalanceResultCodeClawbackClaimableBalanceSuccess,
			},
		},
	}
}

func mkClawbackOp(t *testing.T, asset xdr.Asset, holderSeed byte, amount int64) xdr.Operation {
	t.Helper()
	_, holder := mkAccount(t, holderSeed)
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeClawback,
			ClawbackOp: &xdr.ClawbackOp{
				Asset:  asset,
				From:   xdr.MuxedAccount{Type: xdr.CryptoKeyTypeKeyTypeEd25519, Ed25519: holder.Ed25519},
				Amount: xdr.Int64(amount),
			},
		},
	}
}

func mkClawbackSuccessResult() xdr.OperationResult {
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type:           xdr.OperationTypeClawback,
			ClawbackResult: &xdr.ClawbackResult{Code: xdr.ClawbackResultCodeClawbackSuccess},
		},
	}
}

func TestDecoder_createClaimableBalance_roundTrip(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x70)
	claimantAddr, _ := mkAccount(t, 0x71)
	usdc := mkAlphanum4Asset(t, "USDC", 0x72)
	bid := mkBalanceID(0x73)
	op := mkCreateClaimableBalanceOp(t, usdc, 100_0000000, mkClaimant(t, 0x71))
	result := mkCreateClaimableBalanceSuccessResult(t, bid)

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Op: op, OpResult: result, TxSource: fromAddr, TxHash: "txcb1", Ledger: 40_000_000,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	m := outs[0].(MovementEvent).Movement
	if m.Kind != KindClaimableBalanceCreate {
		t.Errorf("Kind = %q, want %q", m.Kind, KindClaimableBalanceCreate)
	}
	if m.Amount.String() != "1000000000" {
		t.Errorf("Amount = %q, want 1000000000", m.Amount.String())
	}
	if m.FromAddress != fromAddr || m.ToAddress != "" {
		t.Errorf("From/To = %q/%q, want %q/\"\"", m.FromAddress, m.ToAddress, fromAddr)
	}
	wantID := "7300000000000000000000000000000000000000000000000000000000000000"
	if m.Attributes["balance_id"] != wantID {
		t.Errorf("balance_id = %v, want %v", m.Attributes["balance_id"], wantID)
	}
	claimants, ok := m.Attributes["claimants"].([]string)
	if !ok || len(claimants) != 1 || claimants[0] != claimantAddr {
		t.Errorf("claimants = %v, want [%s]", m.Attributes["claimants"], claimantAddr)
	}
}

// TestDecoder_claimClaimableBalance_resolvedFromSameRunIndex proves
// the in-run correlation: a create decoded through the SAME Decoder
// instance earlier makes the later claim resolve immediately, with
// no pending entry recorded.
func TestDecoder_claimClaimableBalance_resolvedFromSameRunIndex(t *testing.T) {
	creatorAddr, _ := mkAccount(t, 0x74)
	claimerAddr, _ := mkAccount(t, 0x75)
	usdc := mkAlphanum4Asset(t, "USDC", 0x76)
	bid := mkBalanceID(0x77)
	d := NewDecoder()

	_, err := d.Decode(dispatcher.OpContext{
		Op:       mkCreateClaimableBalanceOp(t, usdc, 500_0000000, mkClaimant(t, 0x75)),
		OpResult: mkCreateClaimableBalanceSuccessResult(t, bid),
		TxSource: creatorAddr, TxHash: "txcb2", Ledger: 40_000_000,
	})
	if err != nil {
		t.Fatalf("Decode(create): %v", err)
	}

	outs, err := d.Decode(dispatcher.OpContext{
		Op:       mkClaimClaimableBalanceOp(bid),
		OpResult: mkClaimClaimableBalanceSuccessResult(),
		TxSource: claimerAddr, TxHash: "txcb3", Ledger: 40_000_005,
	})
	if err != nil {
		t.Fatalf("Decode(claim): %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	m := outs[0].(MovementEvent).Movement
	if m.Kind != KindClaimableBalanceClaim {
		t.Errorf("Kind = %q, want %q", m.Kind, KindClaimableBalanceClaim)
	}
	if m.Asset != "USDC-"+usdc.MustAlphaNum4().Issuer.Address() || m.Amount.String() != "5000000000" {
		t.Errorf("asset/amount = %s %s, want the create's USDC 5000000000", m.Amount.String(), m.Asset)
	}
	if m.FromAddress != "" || m.ToAddress != claimerAddr {
		t.Errorf("From/To = %q/%q, want \"\"/%q", m.FromAddress, m.ToAddress, claimerAddr)
	}
	if m.Attributes["created_by"] != creatorAddr {
		t.Errorf("created_by = %v, want %q", m.Attributes["created_by"], creatorAddr)
	}
	if pending := d.TakePendingClaimableBalances(); len(pending) != 0 {
		t.Errorf("got %d pending refs, want 0 (resolved from in-run index)", len(pending))
	}
}

// TestDecoder_claimClaimableBalance_unresolved_recordsPending covers
// the "create wasn't seen in this run" path: zero movements emitted,
// a PendingClaimableBalanceRef recorded for the caller's second pass
// — never a guessed amount.
func TestDecoder_claimClaimableBalance_unresolved_recordsPending(t *testing.T) {
	claimerAddr, _ := mkAccount(t, 0x78)
	bid := mkBalanceID(0x79)
	d := NewDecoder()

	outs, err := d.Decode(dispatcher.OpContext{
		Op:       mkClaimClaimableBalanceOp(bid),
		OpResult: mkClaimClaimableBalanceSuccessResult(),
		TxSource: claimerAddr, TxHash: "txcb4", Ledger: 40_000_000,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 0 {
		t.Fatalf("got %d outputs, want 0 (unresolved)", len(outs))
	}
	pending := d.TakePendingClaimableBalances()
	if len(pending) != 1 {
		t.Fatalf("got %d pending refs, want 1", len(pending))
	}
	if pending[0].Kind != KindClaimableBalanceClaim || pending[0].ToAddress != claimerAddr {
		t.Errorf("pending[0] = %+v", pending[0])
	}
	// TakePendingClaimableBalances drains the buffer.
	if again := d.TakePendingClaimableBalances(); len(again) != 0 {
		t.Errorf("second TakePendingClaimableBalances() = %d, want 0 (already drained)", len(again))
	}
}

func TestDecoder_clawbackClaimableBalance_resolvedFromSameRunIndex(t *testing.T) {
	creatorAddr, _ := mkAccount(t, 0x7A)
	issuerAddr, _ := mkAccount(t, 0x7B)
	eurc := mkAlphanum4Asset(t, "EURC", 0x7C)
	bid := mkBalanceID(0x7D)
	d := NewDecoder()

	_, err := d.Decode(dispatcher.OpContext{
		Op:       mkCreateClaimableBalanceOp(t, eurc, 42_0000000, mkClaimant(t, 0x7B)),
		OpResult: mkCreateClaimableBalanceSuccessResult(t, bid),
		TxSource: creatorAddr, TxHash: "txcb5", Ledger: 40_000_000,
	})
	if err != nil {
		t.Fatalf("Decode(create): %v", err)
	}

	outs, err := d.Decode(dispatcher.OpContext{
		Op:       mkClawbackClaimableBalanceOp(bid),
		OpResult: mkClawbackClaimableBalanceSuccessResult(),
		TxSource: issuerAddr, TxHash: "txcb6", Ledger: 40_000_010,
	})
	if err != nil {
		t.Fatalf("Decode(clawback): %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	m := outs[0].(MovementEvent).Movement
	if m.Kind != KindClaimableBalanceClawback {
		t.Errorf("Kind = %q, want %q", m.Kind, KindClaimableBalanceClawback)
	}
	if m.Amount.String() != "420000000" {
		t.Errorf("Amount = %q, want 420000000", m.Amount.String())
	}
	if m.FromAddress != "" || m.ToAddress != issuerAddr {
		t.Errorf("From/To = %q/%q, want \"\"/%q", m.FromAddress, m.ToAddress, issuerAddr)
	}
}

func TestDecoder_clawback_roundTrip(t *testing.T) {
	issuerAddr, _ := mkAccount(t, 0x7E)
	holderAddr, _ := mkAccount(t, 0x7F)
	gala := mkAlphanum4Asset(t, "GALA", 0x80)
	op := mkClawbackOp(t, gala, 0x7F, 387000)
	result := mkClawbackSuccessResult()

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Op: op, OpResult: result, TxSource: issuerAddr, TxHash: "txclaw1", Ledger: 40_000_000,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	m := outs[0].(MovementEvent).Movement
	if m.Kind != KindClawback {
		t.Errorf("Kind = %q, want %q", m.Kind, KindClawback)
	}
	if m.Amount.String() != "387000" {
		t.Errorf("Amount = %q, want 387000", m.Amount.String())
	}
	// FromAddress is the HOLDER (body.From), NOT ctx.TxSource; ToAddress
	// is the issuer (ctx.TxSource) — the reverse of every other kind.
	if m.FromAddress != holderAddr {
		t.Errorf("FromAddress = %q, want holder %q", m.FromAddress, holderAddr)
	}
	if m.ToAddress != issuerAddr {
		t.Errorf("ToAddress = %q, want issuer %q", m.ToAddress, issuerAddr)
	}
}

// TestDecoder_ResolveBalance_sameWindowOutOfOrder proves the
// re-check path chops uses after draining a window's pending list:
// even though the claim was decoded BEFORE its create (simulating
// StreamClassicOps' tx_hash-lexicographic order putting the claim's
// tx ahead of the create's tx within the same window), ResolveBalance
// finds it once the create HAS been indexed, no ClickHouse round trip needed.
func TestDecoder_ResolveBalance_sameWindowOutOfOrder(t *testing.T) {
	creatorAddr, _ := mkAccount(t, 0x84)
	claimerAddr, _ := mkAccount(t, 0x85)
	usdc := mkAlphanum4Asset(t, "USDC", 0x86)
	bid := mkBalanceID(0x87)
	d := NewDecoder()

	// Claim decoded FIRST (its tx_hash sorts earlier) — unresolved at
	// this point, since the create hasn't been seen yet.
	outs, err := d.Decode(dispatcher.OpContext{
		Op: mkClaimClaimableBalanceOp(bid), OpResult: mkClaimClaimableBalanceSuccessResult(),
		TxSource: claimerAddr, TxHash: "a_claim_tx", Ledger: 40_000_000,
	})
	if err != nil {
		t.Fatalf("Decode(claim): %v", err)
	}
	if len(outs) != 0 {
		t.Fatalf("got %d outputs, want 0 (not yet resolved)", len(outs))
	}
	pending := d.TakePendingClaimableBalances()
	if len(pending) != 1 {
		t.Fatalf("got %d pending refs, want 1", len(pending))
	}

	// Create decoded SECOND, same window — indexes the balance.
	_, err = d.Decode(dispatcher.OpContext{
		Op:       mkCreateClaimableBalanceOp(t, usdc, 250_0000000, mkClaimant(t, 0x85)),
		OpResult: mkCreateClaimableBalanceSuccessResult(t, bid),
		TxSource: creatorAddr, TxHash: "z_create_tx", Ledger: 40_000_000,
	})
	if err != nil {
		t.Fatalf("Decode(create): %v", err)
	}

	// The caller (chops) re-checks the pending ref via ResolveBalance
	// AFTER the whole window has been decoded — this must now succeed
	// without touching ClickHouse.
	asset, amount, createdBy, ok := d.ResolveBalance(pending[0].BalanceIDHex)
	if !ok {
		t.Fatal("ResolveBalance = false, want true (create was indexed later in the same window)")
	}
	if asset != "USDC-"+usdc.MustAlphaNum4().Issuer.Address() || amount.String() != "2500000000" || createdBy != creatorAddr {
		t.Errorf("got asset=%s amount=%s createdBy=%s", asset, amount.String(), createdBy)
	}
}

func TestDecoder_claimableBalance_failedOps_emitNothing(t *testing.T) {
	bid := mkBalanceID(0x81)
	cases := []struct {
		name   string
		op     xdr.Operation
		result xdr.OperationResult
	}{
		{
			"create_bareCode", mkCreateClaimableBalanceOp(t, xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, 100, mkClaimant(t, 0x82)),
			xdr.OperationResult{Code: xdr.OperationResultCodeOpNoAccount},
		},
		{"claim_bareCode", mkClaimClaimableBalanceOp(bid), xdr.OperationResult{Code: xdr.OperationResultCodeOpNoAccount}},
		{"clawback_bareCode", mkClawbackClaimableBalanceOp(bid), xdr.OperationResult{Code: xdr.OperationResultCodeOpNoAccount}},
		{"clawbackOp_bareCode", mkClawbackOp(t, xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, 0x83, 100), xdr.OperationResult{Code: xdr.OperationResultCodeOpNoAccount}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outs, err := NewDecoder().Decode(dispatcher.OpContext{Op: tc.op, OpResult: tc.result, TxSource: "GTEST"})
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(outs) != 0 {
				t.Errorf("got %d outputs, want 0", len(outs))
			}
		})
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

// TestDecoder_createAccount_zeroBalance_sponsored pins the CAP-33 rule:
// StartingBalance == 0 (sponsored reserves, Protocol 15+) is a REAL
// account creation, not a malformed op — the 2026-07-12 backfill was
// silently dropping every sponsored creation before this.
func TestDecoder_createAccount_zeroBalance_sponsored(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x20)
	op := mkCreateAccountOp(t, 0x21, 0)
	result := mkCreateAccountSuccessResult()

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Ledger: 37_124_896, TxHash: "txsponsored", TxSource: fromAddr, OpIndex: 1,
		Op: op, OpResult: result,
	})
	if err != nil {
		t.Fatalf("zero StartingBalance must decode (CAP-33 sponsored creation), got: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	m := outs[0].(MovementEvent).Movement
	if m.Kind != KindCreateAccount || m.Amount.Sign() != 0 {
		t.Fatalf("want zero-amount create_account, got kind=%q amount=%s", m.Kind, m.Amount)
	}
}
