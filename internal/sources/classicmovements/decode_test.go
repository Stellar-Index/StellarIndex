package classicmovements

import (
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
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
	closedAt := time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC)

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Ledger: 40_000_000, ClosedAt: closedAt, TxHash: "deadbeef", TxSource: fromAddr, OpIndex: 2,
		Op: mkPaymentOp(t, 0x02, asset, 500_0000000), OpResult: mkPaymentSuccessResult(),
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
	if m.FromAddress != fromAddr || m.ToAddress != destAddr {
		t.Errorf("From/To = %q/%q, want %q/%q", m.FromAddress, m.ToAddress, fromAddr, destAddr)
	}
	if ev.Source() != SourceName {
		t.Errorf("Source() = %q, want %q", ev.Source(), SourceName)
	}
}

func TestDecoder_nativeKinds(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x10)
	destAddr, _ := mkAccount(t, 0x21)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	cases := []struct {
		name       string
		ctx        dispatcher.OpContext
		wantKind   Kind
		wantAmount string
		checkTo    bool
	}{
		{"payment", dispatcher.OpContext{Op: mkPaymentOp(t, 0x11, native, 10), OpResult: mkPaymentSuccessResult()}, KindPayment, "10", false},
		{"createAccount", dispatcher.OpContext{Op: mkCreateAccountOp(t, 0x21, 2_732_091_143), OpResult: mkCreateAccountSuccessResult()}, KindCreateAccount, "2732091143", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.ctx.TxSource, tc.ctx.TxHash, tc.ctx.Ledger = fromAddr, "tx1", 40_000_000
			m := decodeOne(t, NewDecoder(), tc.ctx)
			if m.Kind != tc.wantKind || m.Asset != "native" || m.Amount.String() != tc.wantAmount {
				t.Errorf("got %q %s %s, want %q native %s", m.Kind, m.Amount.String(), m.Asset, tc.wantKind, tc.wantAmount)
			}
			if m.FromAddress != fromAddr || (tc.checkTo && m.ToAddress != destAddr) {
				t.Errorf("From/To = %q/%q, want %q/%q", m.FromAddress, m.ToAddress, fromAddr, destAddr)
			}
		})
	}
}

// Failed ops emit nothing: either the op never reached its own result union
// (bare code, as in real_bytes_test.go's payment_failed_source_no_account) or
// the union's own code is a failure (inner).
func TestDecoder_failedOps_emitNothing(t *testing.T) {
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	bare := xdr.OperationResult{Code: xdr.OperationResultCodeOpNoAccount}
	cases := []struct {
		name   string
		op     xdr.Operation
		result xdr.OperationResult
	}{
		{"payment_bareCode", mkPaymentOp(t, 0x30, native, 1), bare},
		{"payment_innerFailure", mkPaymentOp(t, 0x31, native, 1), xdr.OperationResult{
			Code: xdr.OperationResultCodeOpInner,
			Tr: &xdr.OperationResultTr{
				Type:          xdr.OperationTypePayment,
				PaymentResult: &xdr.PaymentResult{Code: xdr.PaymentResultCodePaymentUnderfunded},
			},
		}},
		{"pathPayment_bareCode", mkPathPaymentStrictReceiveOp(t, native, 100, 0x69, native, 100), bare},
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

// A "successful" op with a non-positive amount never occurs on chain; Decode
// must fail loudly rather than emit a row that breaks the `amount >= 0` CHECK.
func TestDecoder_malformedAmount_errorsLoudly(t *testing.T) {
	_, err := NewDecoder().Decode(dispatcher.OpContext{
		Op:       mkPaymentOp(t, 0x40, xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, 0),
		OpResult: mkPaymentSuccessResult(), TxSource: "GTEST",
	})
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

func eventMovements(outs []consumer.Event) []Movement {
	ms := make([]Movement, 0, len(outs))
	for _, ev := range outs {
		ms = append(ms, ev.(MovementEvent).Movement)
	}
	return ms
}

// pathPaymentLegs asserts ms is exactly one path payment's two legs
// and returns them as (source leg 0, destination leg 1).
func pathPaymentLegs(t *testing.T, ms []Movement) (src, dst Movement) {
	t.Helper()
	if len(ms) != 2 {
		t.Fatalf("got %d movements, want 2 path_payment legs", len(ms))
	}
	src, dst = ms[0], ms[1]
	if src.Kind != KindPathPayment || dst.Kind != KindPathPayment || src.LegIndex != 0 || dst.LegIndex != 1 {
		t.Fatalf("legs = {%q leg %d, %q leg %d}, want {%q leg 0, %q leg 1}",
			src.Kind, src.LegIndex, dst.Kind, dst.LegIndex, KindPathPayment, KindPathPayment)
	}
	return src, dst
}

// A path payment's sender leg carries the asset and amount that LEFT the
// sender; only the destination leg carries the delivered asset.
func TestDecoder_pathPayment_senderLegCarriesSendAsset(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x6A)
	destAddr, _ := mkAccount(t, 0x6B)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x6C)
	usdcID := "USDC-" + usdc.MustAlphaNum4().Issuer.Address()
	offers := []xdr.ClaimAtom{mkOrderBookClaimAtom(t, 0x6D, usdc, 2_500_0000000, native, 10_000_0000000)}

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Ledger: 40_000_000, TxHash: "txpp-1062", TxSource: fromAddr,
		Op:       mkPathPaymentStrictSendOp(t, native, 10_000_0000000, 0x6B, usdc, 2_400_0000000),
		OpResult: mkPathPaymentStrictSendSuccessResult(t, 0x6B, usdc, 2_500_0000000, offers),
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	src, dst := pathPaymentLegs(t, eventMovements(outs))
	if src.FromAddress != fromAddr || src.ToAddress != "" || src.Asset != "native" || src.Amount.String() != "100000000000" {
		t.Errorf("source leg = %s %s %q->%q, want 100000000000 native %q->\"\"",
			src.Amount.String(), src.Asset, src.FromAddress, src.ToAddress, fromAddr)
	}
	if dst.FromAddress != "" || dst.ToAddress != destAddr || dst.Asset != usdcID || dst.Amount.String() != "25000000000" {
		t.Errorf("destination leg = %s %s %q->%q, want 25000000000 %s \"\"->%q",
			dst.Amount.String(), dst.Asset, dst.FromAddress, dst.ToAddress, usdcID, destAddr)
	}
	want := map[string]any{
		"send_asset": "native", "send_amount": "100000000000",
		"dest_asset": usdcID, "dest_amount": "25000000000",
		"from": fromAddr, "to": destAddr,
	}
	for _, m := range []Movement{src, dst} {
		if !reflect.DeepEqual(m.Attributes, want) {
			t.Errorf("leg %d Attributes = %+v, want %+v", m.LegIndex, m.Attributes, want)
		}
	}
}

// StrictReceive derives the source amount from the contiguous prefix of
// offers whose AssetBought is SendAsset (the SendAsset==DestAsset case with
// no offers is exactly the delivered amount).
func TestDecoder_pathPaymentStrictReceive_sourceAmountDerivation(t *testing.T) {
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	sony := mkAlphanum4Asset(t, "SONY", 0x53)
	shib := mkAlphanum4Asset(t, "SHIB", 0x57)
	usdc := mkAlphanum4Asset(t, "USDC", 0x5C)
	cases := []struct {
		name       string
		destAsset  xdr.Asset
		destAmount int64
		offers     []xdr.ClaimAtom
		wantDest   string
		wantSrc    string
	}{
		{"direct_noOffers", native, 100, nil, "100", "100"},
		{"singleHop", sony, 900000000000, []xdr.ClaimAtom{
			mkOrderBookClaimAtom(t, 0x54, sony, 900000000000, native, 12_000000),
		}, "900000000000", "12000000"},
		// Two-hop native->SHIB->native: only hop0 counts toward the source.
		{"multiHop", native, 83586584, []xdr.ClaimAtom{
			mkOrderBookClaimAtom(t, 0x58, shib, 602078450074, native, 83568489),
			mkOrderBookClaimAtom(t, 0x59, native, 83586584, shib, 602078450074),
		}, "83586584", "83568489"},
		{"multiOfferSameHop", usdc, 500_0000000, []xdr.ClaimAtom{
			mkOrderBookClaimAtom(t, 0x5D, usdc, 400_0000000, native, 40_000000),
			mkOrderBookClaimAtom(t, 0x5E, usdc, 100_0000000, native, 10_000000),
		}, "5000000000", "50000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fromAddr, _ := mkAccount(t, 0x50)
			destAddr, _ := mkAccount(t, 0x51)
			outs, err := NewDecoder().Decode(dispatcher.OpContext{
				TxSource: fromAddr, TxHash: "txpp", Ledger: 40_000_000,
				Op:       mkPathPaymentStrictReceiveOp(t, native, 1<<40, 0x51, tc.destAsset, tc.destAmount),
				OpResult: mkPathPaymentStrictReceiveSuccessResult(t, 0x51, tc.destAsset, tc.destAmount, tc.offers),
			})
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			src, dst := pathPaymentLegs(t, eventMovements(outs))
			if dst.Amount.String() != tc.wantDest || src.Amount.String() != tc.wantSrc {
				t.Errorf("dest/source amount = %s/%s, want %s/%s", dst.Amount.String(), src.Amount.String(), tc.wantDest, tc.wantSrc)
			}
			if src.Asset != "native" {
				t.Errorf("source asset = %q, want native", src.Asset)
			}
			if src.FromAddress != fromAddr || dst.ToAddress != destAddr {
				t.Errorf("From/To = %q/%q, want %q/%q", src.FromAddress, dst.ToAddress, fromAddr, destAddr)
			}
		})
	}
}

// If the first offer's AssetBought is not SendAsset, the hop-order assumption
// is violated: fail loudly rather than derive a wrong amount.
func TestDecoder_pathPaymentStrictReceive_hopOrderViolation_errorsLoudly(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x60)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x61)
	shib := mkAlphanum4Asset(t, "SHIB", 0x62)
	offers := []xdr.ClaimAtom{mkOrderBookClaimAtom(t, 0x63, shib, 1000, usdc, 500)}

	_, err := NewDecoder().Decode(dispatcher.OpContext{
		Op:       mkPathPaymentStrictReceiveOp(t, native, 1000, 0x64, shib, 1000),
		OpResult: mkPathPaymentStrictReceiveSuccessResult(t, 0x64, shib, 1000, offers),
		TxSource: fromAddr, TxHash: "txpp5",
	})
	if !errors.Is(err, ErrMalformedMovement) {
		t.Errorf("err = %v, want errors.Is(err, ErrMalformedMovement)", err)
	}
}

// StrictSend takes SendAmount exactly from the body, no offers derivation.
func TestDecoder_pathPaymentStrictSend_success(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x65)
	destAddr, _ := mkAccount(t, 0x66)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	aqua := mkAlphanum4Asset(t, "AQUA", 0x67)
	offers := []xdr.ClaimAtom{mkOrderBookClaimAtom(t, 0x68, aqua, 63545, native, 1100)}

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Op:       mkPathPaymentStrictSendOp(t, native, 1100, 0x66, aqua, 60000),
		OpResult: mkPathPaymentStrictSendSuccessResult(t, 0x66, aqua, 63545, offers),
		TxSource: fromAddr, TxHash: "txpp6",
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	src, m := pathPaymentLegs(t, eventMovements(outs))
	if m.Asset != "AQUA-"+aqua.MustAlphaNum4().Issuer.Address() || m.Amount.String() != "63545" {
		t.Errorf("dest leg = %s %s", m.Amount.String(), m.Asset)
	}
	if src.Asset != "native" || src.Amount.String() != "1100" {
		t.Errorf("source leg = %s %s, want native 1100", src.Amount.String(), src.Asset)
	}
	if m.ToAddress != destAddr {
		t.Errorf("ToAddress = %q, want %q", m.ToAddress, destAddr)
	}
}

// One path payment is both SDEX trades (its claim atoms) and movements (its
// legs); whatever the registration order, the dispatcher must hand the op to
// both decoders and count it against both.
func TestDispatcher_pathPayment_reachesBothSDEXAndMovements(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x65)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	aqua := mkAlphanum4Asset(t, "AQUA", 0x67)
	offers := []xdr.ClaimAtom{mkOrderBookClaimAtom(t, 0x68, aqua, 63545, native, 1100)}
	ctx := dispatcher.OpContext{
		Ledger:   61000000,
		ClosedAt: time.Unix(1_760_000_000, 0).UTC(),
		TxHash:   "txpp-both",
		TxSource: fromAddr,
		Op:       mkPathPaymentStrictSendOp(t, native, 1100, 0x66, aqua, 60000),
		OpResult: mkPathPaymentStrictSendSuccessResult(t, 0x66, aqua, 63545, offers),
	}
	orders := map[string][]dispatcher.OpDecoder{
		"sdex-first":      {sdex.NewDecoder(), NewDecoder()},
		"movements-first": {NewDecoder(), sdex.NewDecoder()},
	}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			disp := dispatcher.New()
			for _, od := range order {
				disp.AddOpDecoder(od)
			}
			outs, err := disp.RouteOp(ctx)
			if err != nil {
				t.Fatalf("RouteOp: %v", err)
			}
			bySource := map[string]int{}
			for _, ev := range outs {
				bySource[ev.Source()]++
			}
			if bySource[sdex.SourceName] != 1 || bySource[SourceName] != 2 {
				t.Errorf("outputs by source = %v, want one %s trade and two %s legs",
					bySource, sdex.SourceName, SourceName)
			}
			seen := disp.Stats().EventsSeen
			if seen[sdex.SourceName] != 1 || seen[SourceName] != 1 {
				t.Errorf("EventsSeen = %v, want 1 for both %s and %s", seen, sdex.SourceName, SourceName)
			}
		})
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

// decodeOne decodes ctx on d and returns the single movement it must emit.
func decodeOne(t *testing.T, d *Decoder, ctx dispatcher.OpContext) Movement {
	t.Helper()
	outs, err := d.Decode(ctx)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	return outs[0].(MovementEvent).Movement
}

func TestDecoder_createClaimableBalance_roundTrip(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x70)
	claimantAddr, _ := mkAccount(t, 0x71)
	usdc := mkAlphanum4Asset(t, "USDC", 0x72)
	m := decodeOne(t, NewDecoder(), dispatcher.OpContext{
		Op:       mkCreateClaimableBalanceOp(t, usdc, 100_0000000, mkClaimant(t, 0x71)),
		OpResult: mkCreateClaimableBalanceSuccessResult(t, mkBalanceID(0x73)),
		TxSource: fromAddr, TxHash: "txcb1", Ledger: 40_000_000,
	})
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

// A create decoded earlier on the same Decoder resolves a later claim or
// clawback immediately, with no pending ref.
func TestDecoder_balanceFollowUp_resolvedFromSameRunIndex(t *testing.T) {
	creatorAddr, _ := mkAccount(t, 0x74)
	actorAddr, _ := mkAccount(t, 0x75)
	bid := mkBalanceID(0x77)
	cases := []struct {
		name       string
		asset      xdr.Asset
		amount     int64
		op         xdr.Operation
		result     xdr.OperationResult
		wantKind   Kind
		wantAmount string
		wantIssuer bool
	}{
		{"claim", mkAlphanum4Asset(t, "USDC", 0x76), 500_0000000, mkClaimClaimableBalanceOp(bid), mkClaimClaimableBalanceSuccessResult(), KindClaimableBalanceClaim, "5000000000", true},
		{"clawback", mkAlphanum4Asset(t, "EURC", 0x7C), 42_0000000, mkClawbackClaimableBalanceOp(bid), mkClawbackClaimableBalanceSuccessResult(), KindClaimableBalanceClawback, "420000000", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDecoder()
			decodeOne(t, d, dispatcher.OpContext{
				Op:       mkCreateClaimableBalanceOp(t, tc.asset, tc.amount, mkClaimant(t, 0x75)),
				OpResult: mkCreateClaimableBalanceSuccessResult(t, bid),
				TxSource: creatorAddr, TxHash: "txcb2", Ledger: 40_000_000,
			})
			m := decodeOne(t, d, dispatcher.OpContext{
				Op: tc.op, OpResult: tc.result, TxSource: actorAddr, TxHash: "txcb3", Ledger: 40_000_005,
			})
			if m.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", m.Kind, tc.wantKind)
			}
			if m.Amount.String() != tc.wantAmount {
				t.Errorf("Amount = %q, want %s", m.Amount.String(), tc.wantAmount)
			}
			if tc.wantIssuer && m.Asset != "USDC-"+tc.asset.MustAlphaNum4().Issuer.Address() {
				t.Errorf("Asset = %q, want the create's USDC", m.Asset)
			}
			if m.FromAddress != "" || m.ToAddress != actorAddr {
				t.Errorf("From/To = %q/%q, want \"\"/%q", m.FromAddress, m.ToAddress, actorAddr)
			}
			if tc.wantKind == KindClaimableBalanceClaim && m.Attributes["created_by"] != creatorAddr {
				t.Errorf("created_by = %v, want %q", m.Attributes["created_by"], creatorAddr)
			}
			if pending := d.TakePendingClaimableBalances(); len(pending) != 0 {
				t.Errorf("got %d pending refs, want 0 (resolved from in-run index)", len(pending))
			}
		})
	}
}

// A claim whose create was not seen emits nothing and records a pending ref
// (never a guessed amount); the create decoded later in the same window makes
// ResolveBalance succeed without a ClickHouse round trip.
func TestDecoder_claimClaimableBalance_unresolvedThenResolved(t *testing.T) {
	creatorAddr, _ := mkAccount(t, 0x84)
	claimerAddr, _ := mkAccount(t, 0x85)
	usdc := mkAlphanum4Asset(t, "USDC", 0x86)
	bid := mkBalanceID(0x87)
	d := NewDecoder()

	outs, err := d.Decode(dispatcher.OpContext{
		Op: mkClaimClaimableBalanceOp(bid), OpResult: mkClaimClaimableBalanceSuccessResult(),
		TxSource: claimerAddr, TxHash: "a_claim_tx", Ledger: 40_000_000,
	})
	if err != nil {
		t.Fatalf("Decode(claim): %v", err)
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
	if again := d.TakePendingClaimableBalances(); len(again) != 0 {
		t.Errorf("second TakePendingClaimableBalances() = %d, want 0 (already drained)", len(again))
	}

	decodeOne(t, d, dispatcher.OpContext{
		Op:       mkCreateClaimableBalanceOp(t, usdc, 250_0000000, mkClaimant(t, 0x85)),
		OpResult: mkCreateClaimableBalanceSuccessResult(t, bid),
		TxSource: creatorAddr, TxHash: "z_create_tx", Ledger: 40_000_000,
	})
	asset, amount, createdBy, ok := d.ResolveBalance(pending[0].BalanceIDHex)
	if !ok {
		t.Fatal("ResolveBalance = false, want true (create was indexed later in the same window)")
	}
	if asset != "USDC-"+usdc.MustAlphaNum4().Issuer.Address() || amount.String() != "2500000000" || createdBy != creatorAddr {
		t.Errorf("got asset=%s amount=%s createdBy=%s", asset, amount.String(), createdBy)
	}
}

func TestDecoder_clawback_roundTrip(t *testing.T) {
	issuerAddr, _ := mkAccount(t, 0x7E)
	holderAddr, _ := mkAccount(t, 0x7F)
	m := decodeOne(t, NewDecoder(), dispatcher.OpContext{
		Op:       mkClawbackOp(t, mkAlphanum4Asset(t, "GALA", 0x80), 0x7F, 387000),
		OpResult: mkClawbackSuccessResult(), TxSource: issuerAddr, TxHash: "txclaw1", Ledger: 40_000_000,
	})
	if m.Kind != KindClawback {
		t.Errorf("Kind = %q, want %q", m.Kind, KindClawback)
	}
	if m.Amount.String() != "387000" {
		t.Errorf("Amount = %q, want 387000", m.Amount.String())
	}
	// From is the HOLDER (body.From) and To the issuer (TxSource): the reverse of every other kind.
	if m.FromAddress != holderAddr || m.ToAddress != issuerAddr {
		t.Errorf("From/To = %q/%q, want holder %q / issuer %q", m.FromAddress, m.ToAddress, holderAddr, issuerAddr)
	}
}

// The in-run create index is bounded with FIFO eviction (OOM guard); the cap
// is shrunk so the test needs no millions of entries.
func TestDecoder_claimableBalanceIndex_FIFOEviction(t *testing.T) {
	orig := maxCBIndexEntries
	maxCBIndexEntries = 3
	t.Cleanup(func() { maxCBIndexEntries = orig })

	d := NewDecoder()
	fromAddr, _ := mkAccount(t, 0x01)
	mkCreateMovement := func(id string) Movement {
		return Movement{
			Kind:        KindClaimableBalanceCreate,
			Asset:       "native",
			Amount:      canonical.NewAmount(big.NewInt(1)),
			FromAddress: fromAddr,
			Attributes:  map[string]any{"balance_id": id},
		}
	}

	// Cap 3, 5 inserts: the two oldest are evicted, the three newest stay.
	const inserted = 5
	ids := make([]string, inserted)
	for i := 0; i < inserted; i++ {
		ids[i] = fmt.Sprintf("bid%d", i)
		d.indexClaimableBalanceCreate(mkCreateMovement(ids[i]))
	}
	if got := len(d.balances); got != maxCBIndexEntries {
		t.Fatalf("len(d.balances) = %d, want %d (capped)", got, maxCBIndexEntries)
	}
	for _, id := range ids[:inserted-maxCBIndexEntries] {
		if _, _, _, found := d.ResolveBalance(id); found {
			t.Errorf("ResolveBalance(%q) found = true, want false (should have been FIFO-evicted)", id)
		}
	}
	for _, id := range ids[inserted-maxCBIndexEntries:] {
		asset, amount, createdBy, found := d.ResolveBalance(id)
		if !found {
			t.Errorf("ResolveBalance(%q) found = false, want true (within the cap, should still be indexed)", id)
			continue
		}
		if asset != "native" || amount.String() != "1" || createdBy != fromAddr {
			t.Errorf("ResolveBalance(%q) = asset=%s amount=%s createdBy=%s, want native/1/%s", id, asset, amount.String(), createdBy, fromAddr)
		}
	}

	// A retried window re-decoding an indexed id must not take a new slot.
	d.indexClaimableBalanceCreate(mkCreateMovement(ids[inserted-1]))
	if got := len(d.balances); got != maxCBIndexEntries {
		t.Fatalf("after re-insert, len(d.balances) = %d, want %d (unchanged)", got, maxCBIndexEntries)
	}
	for _, id := range ids[inserted-maxCBIndexEntries:] {
		if _, _, _, found := d.ResolveBalance(id); !found {
			t.Errorf("after re-insert, ResolveBalance(%q) found = false, want true", id)
		}
	}
}

func TestDecoder_claimableBalance_failedOps_emitNothing(t *testing.T) {
	bid := mkBalanceID(0x81)
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	noAccount := xdr.OperationResult{Code: xdr.OperationResultCodeOpNoAccount}
	cases := []struct {
		name string
		op   xdr.Operation
	}{
		{"create_bareCode", mkCreateClaimableBalanceOp(t, native, 100, mkClaimant(t, 0x82))},
		{"claim_bareCode", mkClaimClaimableBalanceOp(bid)},
		{"clawback_bareCode", mkClawbackClaimableBalanceOp(bid)},
		{"clawbackOp_bareCode", mkClawbackOp(t, native, 0x83, 100)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outs, err := NewDecoder().Decode(dispatcher.OpContext{Op: tc.op, OpResult: noAccount, TxSource: "GTEST"})
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

// CAP-33: a zero StartingBalance is a REAL sponsored creation, not a malformed op.
func TestDecoder_createAccount_zeroBalance_sponsored(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x20)
	m := decodeOne(t, NewDecoder(), dispatcher.OpContext{
		Ledger: 37_124_896, TxHash: "txsponsored", TxSource: fromAddr, OpIndex: 1,
		Op: mkCreateAccountOp(t, 0x21, 0), OpResult: mkCreateAccountSuccessResult(),
	})
	if m.Kind != KindCreateAccount || m.Amount.Sign() != 0 {
		t.Fatalf("want zero-amount create_account, got kind=%q amount=%s", m.Kind, m.Amount)
	}
}

func mkAccountMergeOp(t *testing.T, destSeed byte) xdr.Operation {
	t.Helper()
	_, dest := mkAccount(t, destSeed)
	muxedDest := xdr.MuxedAccount{Type: xdr.CryptoKeyTypeKeyTypeEd25519, Ed25519: dest.Ed25519}
	return xdr.Operation{
		Body: xdr.OperationBody{
			Type:        xdr.OperationTypeAccountMerge,
			Destination: &muxedDest,
		},
	}
}

func mkAccountMergeSuccessResult(bal int64) xdr.OperationResult {
	b := xdr.Int64(bal)
	return xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeAccountMerge,
			AccountMergeResult: &xdr.AccountMergeResult{
				Code:                 xdr.AccountMergeResultCodeAccountMergeSuccess,
				SourceAccountBalance: &b,
			},
		},
	}
}

// CAP-33 mirrored onto AccountMerge: a zero balance is a REAL merge; a
// negative one is never legal and must error loudly.
func TestDecoder_accountMerge_balanceEdges(t *testing.T) {
	fromAddr, _ := mkAccount(t, 0x50)
	destAddr, _ := mkAccount(t, 0x51)

	m := decodeOne(t, NewDecoder(), dispatcher.OpContext{
		Ledger: 37_124_900, TxHash: "txmergesponsored", TxSource: fromAddr,
		Op: mkAccountMergeOp(t, 0x51), OpResult: mkAccountMergeSuccessResult(0),
	})
	if m.Kind != KindAccountMerge || m.Amount.Sign() != 0 {
		t.Fatalf("want zero-amount account_merge, got kind=%q amount=%s", m.Kind, m.Amount)
	}
	if m.FromAddress != fromAddr || m.ToAddress != destAddr {
		t.Errorf("From/To = %q/%q, want %q/%q", m.FromAddress, m.ToAddress, fromAddr, destAddr)
	}

	_, err := NewDecoder().Decode(dispatcher.OpContext{
		Ledger: 37_124_901, TxHash: "txmergenegative", TxSource: fromAddr,
		Op: mkAccountMergeOp(t, 0x53), OpResult: mkAccountMergeSuccessResult(-1),
	})
	if !errors.Is(err, ErrMalformedMovement) {
		t.Errorf("err = %v, want errors.Is(err, ErrMalformedMovement)", err)
	}
}
