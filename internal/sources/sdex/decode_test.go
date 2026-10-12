package sdex

import (
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// ─── fixture builders ────────────────────────────────────────────

// mkAccount returns a valid G-strkey + corresponding xdr.AccountId
// from a seed byte.
func mkAccount(t *testing.T, seed byte) (string, xdr.AccountId) {
	t.Helper()
	var pub xdr.Uint256
	pub[0] = seed
	aid := xdr.AccountId{
		Type:    xdr.PublicKeyTypePublicKeyTypeEd25519,
		Ed25519: &pub,
	}
	s, err := strkey.Encode(strkey.VersionByteAccountID, pub[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s, aid
}

// mkAlphanum4Asset builds a USDC-shaped classic asset from a seed
// for the issuer.
func mkAlphanum4Asset(t *testing.T, code string, issuerSeed byte) xdr.Asset {
	t.Helper()
	_, issuer := mkAccount(t, issuerSeed)
	var codeArr [4]byte
	copy(codeArr[:], code)
	return xdr.Asset{
		Type: xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{
			AssetCode: codeArr,
			Issuer:    issuer,
		},
	}
}

// mkOrderBookClaim builds an OrderBook ClaimAtom describing one
// filled offer.
func mkOrderBookClaim(t *testing.T,
	sellerSeed byte,
	offerID int64,
	soldAsset, boughtAsset xdr.Asset,
	soldAmount, boughtAmount int64,
) xdr.ClaimAtom {
	t.Helper()
	var pub xdr.Uint256
	pub[0] = sellerSeed
	return xdr.ClaimAtom{
		Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook,
		OrderBook: &xdr.ClaimOfferAtom{
			SellerId:     xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub},
			OfferId:      xdr.Int64(offerID),
			AssetSold:    soldAsset,
			AmountSold:   xdr.Int64(soldAmount),
			AssetBought:  boughtAsset,
			AmountBought: xdr.Int64(boughtAmount),
		},
	}
}

// mkManageSellOfferOp wraps a set of claim atoms in a
// ManageSellOffer operation + success result.
func mkManageSellOfferOp(claims []xdr.ClaimAtom) (xdr.Operation, xdr.OperationResult) {
	op := xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeManageSellOffer,
		},
	}
	result := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeManageSellOffer,
			ManageSellOfferResult: &xdr.ManageSellOfferResult{
				Code: xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
				Success: &xdr.ManageOfferSuccessResult{
					OffersClaimed: claims,
				},
			},
		},
	}
	return op, result
}

// ─── Decoder tests ───────────────────────────────────────────────

func TestDecoder_matchesTradeOps(t *testing.T) {
	cases := []struct {
		name string
		typ  xdr.OperationType
		want bool
	}{
		{"ManageSellOffer", xdr.OperationTypeManageSellOffer, true},
		{"ManageBuyOffer", xdr.OperationTypeManageBuyOffer, true},
		{"CreatePassiveSellOffer", xdr.OperationTypeCreatePassiveSellOffer, true},
		{"PathPaymentStrictReceive", xdr.OperationTypePathPaymentStrictReceive, true},
		{"PathPaymentStrictSend", xdr.OperationTypePathPaymentStrictSend, true},
		{"Payment (never a trade)", xdr.OperationTypePayment, false},
		{"CreateAccount (never)", xdr.OperationTypeCreateAccount, false},
	}
	d := NewDecoder()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := xdr.Operation{Body: xdr.OperationBody{Type: tc.typ}}
			if got := d.Matches(op); got != tc.want {
				t.Errorf("Matches(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestDecoder_orderBookClaim_roundTrip(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)

	// Trade shape: seller sold 100 XLM, bought 12 USDC.
	claim := mkOrderBookClaim(t, 0x20 /*offerID*/, 42,
		xlm, usdc, 1_000_000_000 /*100 XLM*/, 12_000_000 /*12 USDC*/)
	op, result := mkManageSellOfferOp([]xdr.ClaimAtom{claim})

	taker, _ := mkAccount(t, 0x01)
	d := NewDecoder()
	outs, err := d.Decode(dispatcher.OpContext{
		Ledger:   62_000_000,
		ClosedAt: time.Now().UTC().Truncate(time.Second),
		TxHash:   "a1b2c3",
		TxSource: taker,
		OpIndex:  3,
		Op:       op,
		OpResult: result,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	ev, ok := outs[0].(TradeEvent)
	if !ok {
		t.Fatalf("wrong output type %T", outs[0])
	}

	// Base = XLM (native), Quote = USDC classic, amounts preserved.
	if ev.Trade.Pair.Base.Type != c.AssetNative {
		t.Errorf("Base.Type = %q, want native", ev.Trade.Pair.Base.Type)
	}
	if ev.Trade.Pair.Quote.Code != "USDC" {
		t.Errorf("Quote.Code = %q, want USDC", ev.Trade.Pair.Quote.Code)
	}
	if ev.Trade.BaseAmount.BigInt().Int64() != 1_000_000_000 {
		t.Errorf("BaseAmount = %s, want 1_000_000_000", ev.Trade.BaseAmount)
	}
	if ev.Trade.QuoteAmount.BigInt().Int64() != 12_000_000 {
		t.Errorf("QuoteAmount = %s, want 12_000_000", ev.Trade.QuoteAmount)
	}
	if ev.Trade.Taker != taker {
		t.Errorf("Taker = %q, want %q", ev.Trade.Taker, taker)
	}
	seller, _ := mkAccount(t, 0x20)
	if ev.Trade.Maker != seller {
		t.Errorf("Maker = %q, want %q", ev.Trade.Maker, seller)
	}
	if ev.Trade.Source != SourceName {
		t.Errorf("Source = %q", ev.Trade.Source)
	}
	// OpIndex uses the fanout stride: 3*1024 + 0 = 3072.
	if ev.Trade.OpIndex != 3*1024 {
		t.Errorf("OpIndex = %d, want %d", ev.Trade.OpIndex, 3*1024)
	}
}

func TestDecoder_multiClaim_opIndexUnique(t *testing.T) {
	// One ManageSellOffer crossing two resting offers yields two claims and two trades with distinct OpIndex, so the trades
	// primary key cannot collide.
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)

	claims := []xdr.ClaimAtom{
		mkOrderBookClaim(t, 0x21, 1, xlm, usdc, 100_000_000, 1_200_000),
		mkOrderBookClaim(t, 0x22, 2, xlm, usdc, 200_000_000, 2_500_000),
	}
	op, result := mkManageSellOfferOp(claims)

	taker, _ := mkAccount(t, 0x01)
	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Ledger: 1, TxHash: "hash", OpIndex: 5,
		ClosedAt: time.Now(),
		TxSource: taker,
		Op:       op, OpResult: result,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 2 {
		t.Fatalf("got %d outputs, want 2", len(outs))
	}
	seen := map[uint32]bool{}
	for i, o := range outs {
		ev := o.(TradeEvent)
		if seen[ev.Trade.OpIndex] {
			t.Errorf("duplicate OpIndex %d (trade %d)", ev.Trade.OpIndex, i)
		}
		seen[ev.Trade.OpIndex] = true
	}
	if len(seen) != 2 {
		t.Errorf("OpIndex uniqueness violated: %d distinct from 2 trades", len(seen))
	}
}

func TestDecoder_failedOp_emitsNothing(t *testing.T) {
	// An op whose result is anything other than Success produces
	// zero trades — not an error, just no trade signal. stellar-
	// core does let offers fail (insufficient balance, etc.).
	op := xdr.Operation{
		Body: xdr.OperationBody{Type: xdr.OperationTypeManageSellOffer},
	}
	result := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeManageSellOffer,
			ManageSellOfferResult: &xdr.ManageSellOfferResult{
				Code: xdr.ManageSellOfferResultCodeManageSellOfferLowReserve,
			},
		},
	}
	outs, err := NewDecoder().Decode(dispatcher.OpContext{Op: op, OpResult: result})
	if err != nil {
		t.Fatalf("Decode on failed op: %v", err)
	}
	if len(outs) != 0 {
		t.Errorf("got %d outputs from failed op, want 0", len(outs))
	}
}

// TestDecoder_v0ClaimAtom_decodedAsOrderBook pins that pre-CAP-27 V0 claim atoms decode into a trade, deriving the seller
// G-strkey from the raw ed25519 bytes. Without the V0 branch the claim is skipped, leaving a coverage hole in pre-P18 history.
func TestDecoder_v0ClaimAtom_decodedAsOrderBook(t *testing.T) {
	var pub xdr.Uint256
	pub[0] = 0x30
	claim := xdr.ClaimAtom{
		Type: xdr.ClaimAtomTypeClaimAtomTypeV0,
		V0: &xdr.ClaimOfferAtomV0{
			SellerEd25519: pub,
			OfferId:       1,
			AssetSold:     xdr.Asset{Type: xdr.AssetTypeAssetTypeNative},
			AmountSold:    100,
			AssetBought:   mkAlphanum4Asset(t, "USDC", 0x10),
			AmountBought:  12,
		},
	}
	op, result := mkManageSellOfferOp([]xdr.ClaimAtom{claim})
	outs, err := NewDecoder().Decode(dispatcher.OpContext{Op: op, OpResult: result})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1 (V0 claim should decode)", len(outs))
	}
	// decodeClaimAtom should succeed too.
	tr, err := decodeClaimAtom(claim, 1, time.Now(), "tx", 0, 0, "GTAKER")
	if err != nil {
		t.Fatalf("decodeClaimAtom V0: %v", err)
	}
	if tr.Maker == "" || tr.Maker[0] != 'G' {
		t.Errorf("Maker = %q, want G-strkey", tr.Maker)
	}
}

// One-side-zero fills are captured (real trades Hubble records; pricing skips
// the zero leg separately); only both-zero no-op atoms are dropped.
func TestDecoder_zeroAmountPolicy(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)

	// one-side-zero (sold rounds to 0) → captured.
	oneSide := mkOrderBookClaim(t, 0x20, 1, xlm, usdc, 0, 12_000_000)
	op, result := mkManageSellOfferOp([]xdr.ClaimAtom{oneSide})
	outs, _ := NewDecoder().Decode(dispatcher.OpContext{Op: op, OpResult: result})
	if len(outs) != 1 {
		t.Errorf("got %d outputs, want 1 (one-side-zero captured)", len(outs))
	}

	// both-zero no-op → dropped.
	both := mkOrderBookClaim(t, 0x21, 2, xlm, usdc, 0, 0)
	op2, result2 := mkManageSellOfferOp([]xdr.ClaimAtom{both})
	outs2, _ := NewDecoder().Decode(dispatcher.OpContext{Op: op2, OpResult: result2})
	if len(outs2) != 0 {
		t.Errorf("got %d outputs, want 0 (both-zero dropped)", len(outs2))
	}
}

// TestDecoder_claimIndexOverflow pins that a tradeIndex at or past opIndexFanoutStride is
// rejected, not folded into the OpIndex range reserved for the NEXT op. The trades PK upsert
// is ON CONFLICT DO UPDATE, so an alias would silently overwrite an unrelated trade.
func TestDecoder_claimIndexOverflow(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)
	claim := mkOrderBookClaim(t, 0x21, 1, xlm, usdc, 100_000_000, 1_200_000)

	if _, err := decodeClaimAtom(claim, 1, time.Now(), "tx", 0, opIndexFanoutStride-1, "GTAKER"); err != nil {
		t.Fatalf("decodeClaimAtom at stride-1 (last valid index): %v", err)
	}

	_, err := decodeClaimAtom(claim, 1, time.Now(), "tx", 0, opIndexFanoutStride, "GTAKER")
	if !errors.Is(err, ErrClaimIndexOverflow) {
		t.Fatalf("decodeClaimAtom at stride: err = %v, want ErrClaimIndexOverflow", err)
	}
}

func TestDecoder_Name(t *testing.T) {
	if got := NewDecoder().Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

// TestDecoder_DecodeCounted_ReportsPerClaimFailures: Decode silently drops an undecodable claim, so DecodeCounted must report
// the count. The census can then mark the ledger blind (completeness.BlindTracker) instead of certifying a clean reconcile.
func TestDecoder_DecodeCounted_ReportsPerClaimFailures(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)

	claims := []xdr.ClaimAtom{
		mkOrderBookClaim(t, 0x21, 1, xlm, usdc, 100_000_000, 1_200_000), // valid
		{Type: xdr.ClaimAtomType(99)},                                   // unknown atom type: decodeClaimAtom errors
	}
	op, result := mkManageSellOfferOp(claims)
	taker, _ := mkAccount(t, 0x01)
	ctx := dispatcher.OpContext{
		Ledger: 100, TxHash: "hash", OpIndex: 5,
		ClosedAt: time.Now(),
		TxSource: taker,
		Op:       op, OpResult: result,
	}

	outs, failed := NewDecoder().DecodeCounted(ctx)
	if len(outs) != 1 {
		t.Fatalf("DecodeCounted outs = %d, want 1 (one claim decodes, one fails)", len(outs))
	}
	if failed != 1 {
		t.Fatalf("DecodeCounted failed = %d, want 1 — the census cannot mark this ledger blind without this count", failed)
	}

	// Decode (the dispatcher.OpDecoder-conforming entry point) must keep
	// its existing contract unchanged: same trades, always a nil error.
	dOuts, err := NewDecoder().Decode(ctx)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(dOuts) != len(outs) {
		t.Fatalf("Decode outs = %d, DecodeCounted outs = %d — must agree", len(dOuts), len(outs))
	}
}

// TestDecoder_DecodeCounted_NoOpClaimIsNotAFailure — a both-zero no-op claim
// is dropped by the decoder and the census counters alike, so counting it
// would mark every ledger that carries one blind.
func TestDecoder_DecodeCounted_NoOpClaimIsNotAFailure(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)
	claims := []xdr.ClaimAtom{
		mkOrderBookClaim(t, 0x21, 1, xlm, usdc, 100_000_000, 1_200_000),
		mkOrderBookClaim(t, 0x22, 2, xlm, usdc, 0, 0),
	}
	op, result := mkManageSellOfferOp(claims)
	taker, _ := mkAccount(t, 0x01)

	outs, failed := NewDecoder().DecodeCounted(dispatcher.OpContext{
		Ledger: 1, TxHash: "hash", OpIndex: 0,
		ClosedAt: time.Now(),
		TxSource: taker,
		Op:       op, OpResult: result,
	})
	if failed != 0 {
		t.Errorf("failed = %d, want 0: a both-zero no-op claim is a documented drop, not a decode failure", failed)
	}
	if len(outs) != 1 {
		t.Fatalf("outs = %d, want 1 (the no-op claim is still dropped)", len(outs))
	}
	if tr := outs[0].(TradeEvent).Trade; tr.Maker == "" || tr.BaseAmount.BigInt().Int64() != 100_000_000 {
		t.Errorf("emitted trade = %+v, want the valid claim's fill", tr)
	}
}

// TestDecoder_DecodeCounted_CleanOpReportsZeroFailures — the counter must
// stay silent on the healthy path or every SDEX census ledger would read
// blind.
func TestDecoder_DecodeCounted_CleanOpReportsZeroFailures(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)
	claims := []xdr.ClaimAtom{mkOrderBookClaim(t, 0x21, 1, xlm, usdc, 100_000_000, 1_200_000)}
	op, result := mkManageSellOfferOp(claims)
	taker, _ := mkAccount(t, 0x01)

	outs, failed := NewDecoder().DecodeCounted(dispatcher.OpContext{
		Ledger: 1, TxHash: "hash", OpIndex: 0,
		ClosedAt: time.Now(),
		TxSource: taker,
		Op:       op, OpResult: result,
	})
	if failed != 0 {
		t.Errorf("failed = %d, want 0 on a fully-decodable op", failed)
	}
	if len(outs) != 1 {
		t.Errorf("outs = %d, want 1", len(outs))
	}
}

func TestDecoder_pathPaymentStrictReceive_end2end(t *testing.T) {
	d := NewDecoder()
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)
	claim := mkOrderBookClaim(t, 0x20, 1, xlm, usdc, 1_000_000_000, 12_420_000)
	op, result := mkPathPaymentStrictReceiveOp([]xdr.ClaimAtom{claim})

	out, err := d.Decode(dispatcher.OpContext{
		Op: op, OpResult: result,
		Ledger: 1, ClosedAt: time.Unix(1_770_000_000, 0).UTC(),
		TxHash: "abcd", OpIndex: 0,
		TxSource: "GTAKER0000000000000000000000000000000000000000000000000000",
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d trades, want 1", len(out))
	}
}

// TestTradeEvent_implementsConsumerEvent pins EventKind, Source and Decoder.Name: the event sink type-switches on EventKind
// for the metric label, and the dispatcher keys the per-source decode-error counter on Decoder.Name, so a changed value
// silently misroutes telemetry.
func TestTradeEvent_implementsConsumerEvent(t *testing.T) {
	te := TradeEvent{}
	if got := te.EventKind(); got != "sdex.trade" {
		t.Errorf("EventKind() = %q, want \"sdex.trade\"", got)
	}
	if got := te.Source(); got != SourceName {
		t.Errorf("Source() = %q, want %q", got, SourceName)
	}
	var _ consumer.Event = te
}

// extractClaimAtoms covers five op variants; decode_test.go covers ManageSellOffer and the failed-op branch, and this file
// covers the other four, the LiquidityPool ClaimAtom variant, and the native/alphanum12 paths in xdrAssetToCanonical.

// ─── op-variant fixtures ──────────────────────────────────────

func mkManageBuyOfferOp(claims []xdr.ClaimAtom) (xdr.Operation, xdr.OperationResult) {
	op := xdr.Operation{
		Body: xdr.OperationBody{Type: xdr.OperationTypeManageBuyOffer},
	}
	result := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeManageBuyOffer,
			ManageBuyOfferResult: &xdr.ManageBuyOfferResult{
				Code: xdr.ManageBuyOfferResultCodeManageBuyOfferSuccess,
				Success: &xdr.ManageOfferSuccessResult{
					OffersClaimed: claims,
				},
			},
		},
	}
	return op, result
}

func mkCreatePassiveSellOfferOp(claims []xdr.ClaimAtom) (xdr.Operation, xdr.OperationResult) {
	op := xdr.Operation{
		Body: xdr.OperationBody{Type: xdr.OperationTypeCreatePassiveSellOffer},
	}
	result := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeCreatePassiveSellOffer,
			CreatePassiveSellOfferResult: &xdr.ManageSellOfferResult{
				Code: xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
				Success: &xdr.ManageOfferSuccessResult{
					OffersClaimed: claims,
				},
			},
		},
	}
	return op, result
}

func mkPathPaymentStrictReceiveOp(claims []xdr.ClaimAtom) (xdr.Operation, xdr.OperationResult) {
	op := xdr.Operation{
		Body: xdr.OperationBody{Type: xdr.OperationTypePathPaymentStrictReceive},
	}
	result := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypePathPaymentStrictReceive,
			PathPaymentStrictReceiveResult: &xdr.PathPaymentStrictReceiveResult{
				Code: xdr.PathPaymentStrictReceiveResultCodePathPaymentStrictReceiveSuccess,
				Success: &xdr.PathPaymentStrictReceiveResultSuccess{
					Offers: claims,
				},
			},
		},
	}
	return op, result
}

func mkPathPaymentStrictSendOp(claims []xdr.ClaimAtom) (xdr.Operation, xdr.OperationResult) {
	op := xdr.Operation{
		Body: xdr.OperationBody{Type: xdr.OperationTypePathPaymentStrictSend},
	}
	result := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypePathPaymentStrictSend,
			PathPaymentStrictSendResult: &xdr.PathPaymentStrictSendResult{
				Code: xdr.PathPaymentStrictSendResultCodePathPaymentStrictSendSuccess,
				Success: &xdr.PathPaymentStrictSendResultSuccess{
					Offers: claims,
				},
			},
		},
	}
	return op, result
}

func mkLiquidityPoolClaim(soldAsset, boughtAsset xdr.Asset, soldAmount, boughtAmount int64) xdr.ClaimAtom {
	var poolID xdr.PoolId
	for i := range poolID {
		poolID[i] = byte(i ^ 0x55)
	}
	return xdr.ClaimAtom{
		Type: xdr.ClaimAtomTypeClaimAtomTypeLiquidityPool,
		LiquidityPool: &xdr.ClaimLiquidityAtom{
			LiquidityPoolId: poolID,
			AssetSold:       soldAsset,
			AmountSold:      xdr.Int64(soldAmount),
			AssetBought:     boughtAsset,
			AmountBought:    xdr.Int64(boughtAmount),
		},
	}
}

// ─── extractClaimAtoms: the four extra op types ───────────────

func TestExtractClaimAtoms_managBuyOffer_returnsClaims(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)
	claim := mkOrderBookClaim(t, 0x20, 1, xlm, usdc, 1_000_000_000, 12_420_000)

	op, result := mkManageBuyOfferOp([]xdr.ClaimAtom{claim})
	got := extractClaimAtoms(op, result)
	if len(got) != 1 {
		t.Fatalf("got %d claims, want 1", len(got))
	}
}

func TestExtractClaimAtoms_createPassiveSellOffer_returnsClaims(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)
	claim := mkOrderBookClaim(t, 0x20, 1, xlm, usdc, 1, 1)

	op, result := mkCreatePassiveSellOfferOp([]xdr.ClaimAtom{claim})
	got := extractClaimAtoms(op, result)
	if len(got) != 1 {
		t.Fatalf("got %d claims, want 1", len(got))
	}
}

// TestExtractClaimAtoms_createPassiveSellOffer_manageSellArm covers the real encoding: core emits passive-offer results under
// the MANAGE_SELL_OFFER arm, so GetCreatePassiveSellOfferResult returns ok=false and the ManageSellOffer fallback must still
// surface the claims.
func TestExtractClaimAtoms_createPassiveSellOffer_manageSellArm(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)
	claim := mkOrderBookClaim(t, 0x20, 1, xlm, usdc, 1, 1)

	op := xdr.Operation{Body: xdr.OperationBody{Type: xdr.OperationTypeCreatePassiveSellOffer}}
	result := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypeManageSellOffer, // core's real discriminant for passive offers
			ManageSellOfferResult: &xdr.ManageSellOfferResult{
				Code:    xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
				Success: &xdr.ManageOfferSuccessResult{OffersClaimed: []xdr.ClaimAtom{claim}},
			},
		},
	}
	got := extractClaimAtoms(op, result)
	if len(got) != 1 {
		t.Fatalf("got %d claims, want 1 (manage-sell arm fallback)", len(got))
	}
}

func TestExtractClaimAtoms_pathPaymentStrictReceive_returnsClaims(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)
	claim := mkOrderBookClaim(t, 0x20, 1, xlm, usdc, 1, 1)

	op, result := mkPathPaymentStrictReceiveOp([]xdr.ClaimAtom{claim})
	got := extractClaimAtoms(op, result)
	if len(got) != 1 {
		t.Fatalf("got %d claims, want 1", len(got))
	}
}

func TestExtractClaimAtoms_pathPaymentStrictSend_returnsClaims(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)
	claim := mkOrderBookClaim(t, 0x20, 1, xlm, usdc, 1, 1)

	op, result := mkPathPaymentStrictSendOp([]xdr.ClaimAtom{claim})
	got := extractClaimAtoms(op, result)
	if len(got) != 1 {
		t.Fatalf("got %d claims, want 1", len(got))
	}
}

func TestExtractClaimAtoms_unrelatedOpType_returnsNil(t *testing.T) {
	// Payment is classic but not trade-relevant — must yield nil.
	op := xdr.Operation{
		Body: xdr.OperationBody{Type: xdr.OperationTypePayment},
	}
	result := xdr.OperationResult{
		Code: xdr.OperationResultCodeOpInner,
		Tr: &xdr.OperationResultTr{
			Type: xdr.OperationTypePayment,
			PaymentResult: &xdr.PaymentResult{
				Code: xdr.PaymentResultCodePaymentSuccess,
			},
		},
	}
	if got := extractClaimAtoms(op, result); got != nil {
		t.Errorf("got %v, want nil for non-trade op", got)
	}
}

func TestExtractClaimAtoms_resultCodeNotInner_returnsNil(t *testing.T) {
	// Op-level failure (e.g. opBadAuth) — extractClaimAtoms must
	// short-circuit before peeking into Tr.
	op := xdr.Operation{
		Body: xdr.OperationBody{Type: xdr.OperationTypeManageSellOffer},
	}
	result := xdr.OperationResult{Code: xdr.OperationResultCodeOpBadAuth}
	if got := extractClaimAtoms(op, result); got != nil {
		t.Errorf("got %v, want nil for non-OpInner result", got)
	}
}

// ─── decodeClaimAtom: LiquidityPool variant + asset variants ───

func TestDecodeClaimAtom_liquidityPool_recordsPoolHexAsMaker(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)
	atom := mkLiquidityPoolClaim(xlm, usdc, 1_000_000_000, 12_420_000)

	trade, err := decodeClaimAtom(
		atom,
		1, time.Unix(1_770_000_000, 0).UTC(),
		"abcd", 0, 0,
		"GTAKER0000000000000000000000000000000000000000000000000000",
	)
	if err != nil {
		t.Fatalf("decodeClaimAtom: %v", err)
	}
	// Maker should be the hex pool id, not a G-strkey.
	if len(trade.Maker) != 64 {
		t.Errorf("Maker = %q (len %d), want 64-char hex pool id",
			trade.Maker, len(trade.Maker))
	}
	if trade.Source != SourceName {
		t.Errorf("Source = %q, want %q", trade.Source, SourceName)
	}
}

func TestDecodeClaimAtom_unknownClaimType_returnsError(t *testing.T) {
	// A junk-typed ClaimAtom must surface ErrUnknownClaimAtomType.
	atom := xdr.ClaimAtom{Type: xdr.ClaimAtomType(99)}
	_, err := decodeClaimAtom(atom, 1, time.Now(), "abcd", 0, 0, "GT")
	if err == nil {
		t.Error("expected error on unknown claim atom type, got nil")
	}
}

// One-side-zero fills (one leg rounds to 0) are REAL trades Hubble records; we
// capture them for completeness (pricing skips zero legs separately). Only the
// both-zero no-op atoms are dropped.
func TestDecodeClaimAtom_zeroAmountPolicy(t *testing.T) {
	xlm := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	usdc := mkAlphanum4Asset(t, "USDC", 0x10)

	// one-side-zero (bought rounds to 0) → emitted.
	oneSide := mkOrderBookClaim(t, 0x20, 1, xlm, usdc, 5, 0)
	if tr, err := decodeClaimAtom(oneSide, 1, time.Now(), "tx", 0, 0, "GT"); err != nil {
		t.Fatalf("one-side-zero should be captured, got error: %v", err)
	} else if tr.QuoteAmount.BigInt().Sign() != 0 {
		t.Errorf("expected QuoteAmount 0, got %s", tr.QuoteAmount.BigInt())
	}

	// one-side-zero (sold rounds to 0) → emitted.
	if _, err := decodeClaimAtom(mkOrderBookClaim(t, 0x21, 2, xlm, usdc, 0, 7), 1, time.Now(), "tx", 0, 0, "GT"); err != nil {
		t.Fatalf("one-side-zero (sold=0) should be captured, got error: %v", err)
	}

	// both-zero no-op → dropped.
	if _, err := decodeClaimAtom(mkOrderBookClaim(t, 0x22, 3, xlm, usdc, 0, 0), 1, time.Now(), "tx", 0, 0, "GT"); err == nil {
		t.Error("both-zero no-op claim should be dropped, got nil error")
	} else if !errors.Is(err, ErrNoOpClaim) {
		t.Errorf("both-zero no-op claim dropped with %v, want ErrNoOpClaim", err)
	}
}

// ─── xdrAssetToCanonical: native + alphanum12 paths ────────────

func TestXdrAssetToCanonical_native(t *testing.T) {
	got, err := xdrAssetToCanonical(xdr.Asset{Type: xdr.AssetTypeAssetTypeNative})
	if err != nil {
		t.Fatalf("xdrAssetToCanonical(native): %v", err)
	}
	if !got.Equal(c.NativeAsset()) {
		t.Errorf("got %+v, want native asset", got)
	}
}

func TestXdrAssetToCanonical_alphanum12(t *testing.T) {
	_, issuer := mkAccount(t, 0x10)
	var code [12]byte
	copy(code[:], "LONGTOKEN")
	a := xdr.Asset{
		Type: xdr.AssetTypeAssetTypeCreditAlphanum12,
		AlphaNum12: &xdr.AlphaNum12{
			AssetCode: code,
			Issuer:    issuer,
		},
	}
	got, err := xdrAssetToCanonical(a)
	if err != nil {
		t.Fatalf("xdrAssetToCanonical(alphanum12): %v", err)
	}
	if got.Code != "LONGTOKEN" {
		t.Errorf("Code = %q, want \"LONGTOKEN\"", got.Code)
	}
}

// ─── End-to-end through Decoder.Decode for the new op types ────

// A trade op whose inner result is a failure code carries no Success
// arm; every op type must short-circuit on the code rather than reach
// MustSuccess (which would panic) or read claims from a failed op.
func TestExtractClaimAtoms_nonSuccessInnerCode_returnsNil(t *testing.T) {
	sellFail := &xdr.ManageSellOfferResult{Code: xdr.ManageSellOfferResultCodeManageSellOfferUnderfunded}
	cases := []struct {
		name string
		op   xdr.OperationType
		tr   xdr.OperationResultTr
	}{
		{"manage_sell", xdr.OperationTypeManageSellOffer, xdr.OperationResultTr{
			Type: xdr.OperationTypeManageSellOffer, ManageSellOfferResult: sellFail,
		}},
		{"manage_buy", xdr.OperationTypeManageBuyOffer, xdr.OperationResultTr{
			Type: xdr.OperationTypeManageBuyOffer, ManageBuyOfferResult: &xdr.ManageBuyOfferResult{
				Code: xdr.ManageBuyOfferResultCodeManageBuyOfferUnderfunded,
			},
		}},
		{"passive_own_arm", xdr.OperationTypeCreatePassiveSellOffer, xdr.OperationResultTr{
			Type: xdr.OperationTypeCreatePassiveSellOffer, CreatePassiveSellOfferResult: sellFail,
		}},
		{"passive_manage_sell_arm", xdr.OperationTypeCreatePassiveSellOffer, xdr.OperationResultTr{
			Type: xdr.OperationTypeManageSellOffer, ManageSellOfferResult: sellFail,
		}},
		{"path_strict_receive", xdr.OperationTypePathPaymentStrictReceive, xdr.OperationResultTr{
			Type: xdr.OperationTypePathPaymentStrictReceive, PathPaymentStrictReceiveResult: &xdr.PathPaymentStrictReceiveResult{
				Code: xdr.PathPaymentStrictReceiveResultCodePathPaymentStrictReceiveOverSendmax,
			},
		}},
		{"path_strict_send", xdr.OperationTypePathPaymentStrictSend, xdr.OperationResultTr{
			Type: xdr.OperationTypePathPaymentStrictSend, PathPaymentStrictSendResult: &xdr.PathPaymentStrictSendResult{
				Code: xdr.PathPaymentStrictSendResultCodePathPaymentStrictSendUnderDestmin,
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := xdr.Operation{Body: xdr.OperationBody{Type: tc.op}}
			tr := tc.tr
			res := xdr.OperationResult{Code: xdr.OperationResultCodeOpInner, Tr: &tr}
			if got := extractClaimAtoms(op, res); got != nil {
				t.Fatalf("got %d claims from a failed %s op, want nil", len(got), tc.name)
			}
			if claims, drops := AuditOp(op, res); claims != 0 || drops != nil {
				t.Fatalf("AuditOp = (%d, %v), want (0, nil)", claims, drops)
			}
		})
	}
}

// Real mainnet op+result frames, byte-identical to those in classicmovements/real_bytes_test.go.
// Every other test here decodes hand-built XDR, so only these pin what stellar-core emits: the
// ClaimAtom discriminants, the atom order inside a multi-hop result, and which side is "sold".

type wantClaim struct {
	pair        string
	base, quote string
	maker       string
	opIndex     uint32
}

type realFrame struct {
	name      string
	bodyB64   string
	resultB64 string
	ledger    uint32
	txHash    string
	opIndex   int
	txSource  string
	closedAt  time.Time
	want      []wantClaim
}

var realFrames = []realFrame{
	{
		// PathPaymentStrictReceive native -> SHIB -> native: hop 0 crosses an
		// order-book offer, hop 1 a classic liquidity pool. The only real
		// mixed-atom fanout in the suite.
		name:      "pathPaymentStrictReceive_offerThenPool",
		bodyB64:   "AAAAAgAAAAAAAAAABPtnBgAAAAAYyCYed5ULPCCZ1wtggxYUtoK6Hu5uyhKp0+DNkFyZ1gAAAAAAAAAABPtuGAAAAAEAAAABU0hJQgAAAABa7upQ7YJtt/jfTu+F1mmJZbUiSrjeJ9cNliPnYMns5w==",
		resultB64: "AAAAAAAAAAIAAAAAAAAAAgAAAAEAAAAAbaHTfp0wRYC9BeJP51OYornphxPI+aozmlv3trm4KbAAAAAAOKJmcAAAAAFTSElCAAAAAFru6lDtgm23+N9O74XWaYlltSJKuN4n1w2WI+dgyeznAAAAjC6sEZoAAAAAAAAAAAT7J2kAAAACd3xyt7p6rXDgES6e0Vie5PU5pKZvgUqA2rAbTcL4mHEAAAAAAAAAAAT7bhgAAAABU0hJQgAAAABa7upQ7YJtt/jfTu+F1mmJZbUiSrjeJ9cNliPnYMns5wAAAIwurBGaAAAAABjIJh53lQs8IJnXC2CDFhS2groe7m7KEqnT4M2QXJnWAAAAAAAAAAAE+24Y",
		ledger:    40_000_003,
		txHash:    "49203432aa0b5da1a3f621e093cdde6a116064f969efe6f3b6162691d1afb84b",
		opIndex:   0,
		txSource:  "GAMMQJQ6O6KQWPBATHLQWYEDCYKLNAV2D3XG5SQSVHJ6BTMQLSM5MSLE",
		closedAt:  time.Date(2022, 3, 12, 19, 33, 16, 0, time.UTC),
		want: []wantClaim{
			{
				pair: "SHIB-GBNO52SQ5WBG3N7Y35HO7BOWNGEWLNJCJK4N4J6XBWLCHZ3AZHWOPRKF/native",
				base: "602078450074", quote: "83568489",
				maker: "GBW2DU36TUYELAF5AXRE7Z2TTCRLT2MHCPEPTKRTTJN7PNVZXAU3BDDS", opIndex: 0,
			},
			{
				pair: "native/SHIB-GBNO52SQ5WBG3N7Y35HO7BOWNGEWLNJCJK4N4J6XBWLCHZ3AZHWOPRKF",
				base: "83586584", quote: "602078450074",
				maker: "777c72b7ba7aad70e0112e9ed1589ee4f539a4a66f814a80dab01b4dc2f89871", opIndex: 1,
			},
		},
	},
	{
		// PathPaymentStrictSend aiXDOGE -> native -> AQUA at op_index 2: two
		// order-book atoms, so the fanout stride is exercised off op 0.
		name:      "pathPaymentStrictSend_twoOffers",
		bodyB64:   "AAAADQAAAAJhaVhET0dFAAAAAAAAAAAAHmZ99WHIvNYnad6AHqEYtIx8rynNCdIrpMMan93+ee8AAAAAC+vCAAAAAAAbwSApPmwboWhG14u1quvJh4f0t3hW09pLOIa1MPeGVwAAAAFBUVVBAAAAAFuULlOsM8j9CoDMfBsahdfYOKnEGXeq0Ys68Ff44z3wAAAAAAAAbpIAAAABAAAAAA==",
		resultB64: "AAAAAAAAAA0AAAAAAAAAAgAAAAEAAAAAQJzSfng2B5F/ARo1w+R7fYQ1nuZE0ILRfdDZNwTNIh8AAAAAOK4wUAAAAAAAAAAAAAAETAAAAAJhaVhET0dFAAAAAAAAAAAAHmZ99WHIvNYnad6AHqEYtIx8rynNCdIrpMMan93+ee8AAAAAC+vCAAAAAAEAAAAAQ5f/457bn13BXKa5Mccm5n80F2Y9HiOh0k2x4c4FIV4AAAAAOK47NAAAAAFBUVVBAAAAAFuULlOsM8j9CoDMfBsahdfYOKnEGXeq0Ys68Ff44z3wAAAAAAAA+DkAAAAAAAAAAAAABEwAAAAAG8EgKT5sG6FoRteLtarryYeH9Ld4VtPaSziGtTD3hlcAAAABQVFVQQAAAABblC5TrDPI/QqAzHwbGoXX2DipxBl3qtGLOvBX+OM98AAAAAAAAPg5",
		ledger:    40_000_000,
		txHash:    "04f7f85101dd3d9c3d370f65ddeb619b93058f5d8d55d1499932fdf8747a6a40",
		opIndex:   2,
		txSource:  "GAN4CIBJHZWBXILII3LYXNNK5PEYPB7UW54FNU62JM4INNJQ66DFPWWG",
		closedAt:  time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC),
		want: []wantClaim{
			{
				pair: "native/aiXDOGE-GAPGM7PVMHELZVRHNHPIAHVBDC2IY7FPFHGQTURLUTBRVH657Z466RAI",
				base: "1100", quote: "200000000",
				maker: "GBAJZUT6PA3APEL7AENDLQ7EPN6YINM64ZCNBAWRPXINSNYEZURB6ACP", opIndex: 2 * opIndexFanoutStride,
			},
			{
				pair: "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA/native",
				base: "63545", quote: "1100",
				maker: "GBBZP77DT3NZ6XOBLSTLSMOHE3TH6NAXMY6R4I5B2JG3DYOOAUQV5DGV", opIndex: 2*opIndexFanoutStride + 1,
			},
		},
	},
	{
		name:      "pathPaymentStrictReceive_singleOffer",
		bodyB64:   "AAAAAgAAAAAAAAAAALjvwAAAAAAWcHHVU3Pe9F+qSovW7OF1H73pa76MnRgmpMUXGO0/XQAAAAFTT05ZAAAAALsLcXbIOISuH+pMVlQ3U/ziOgkUcfkQQHnKR+iFALP4AAMyi5RMQAAAAAAA",
		resultB64: "AAAAAAAAAAIAAAAAAAAAAQAAAAEAAAAAtBJMX3t5P0oPc7eVIwj3MXMLwgQZNa6roDUwTYg44x8AAAAAOJxqdAAAAAFTT05ZAAAAALsLcXbIOISuH+pMVlQ3U/ziOgkUcfkQQHnKR+iFALP4AAMyi5RMQAAAAAAAAAAAAAC3GwAAAAAAFnBx1VNz3vRfqkqL1uzhdR+96Wu+jJ0YJqTFFxjtP10AAAABU09OWQAAAAC7C3F2yDiErh/qTFZUN1P84joJFHH5EEB5ykfohQCz+AADMouUTEAA",
		ledger:    40_000_001,
		txHash:    "32696e52909644d21ff1e36afbb6379cb8a555cbf658e8c7ae66a0c9c5b417b0",
		opIndex:   0,
		txSource:  "GALHA4OVKNZ555C7VJFIXVXM4F2R7PPJNO7IZHIYE2SMKFYY5U7V254G",
		closedAt:  time.Date(2022, 3, 12, 19, 33, 2, 0, time.UTC),
		want: []wantClaim{
			{
				pair: "SONY-GC5QW4LWZA4IJLQ75JGFMVBXKP6OEOQJCRY7SECAPHFEP2EFACZ7QZW5/native",
				base: "900000000000000", quote: "12000000",
				maker: "GC2BETC7PN4T6SQPOO3ZKIYI64YXGC6CAQMTLLVLUA2TATMIHDRR7HMS", opIndex: 0,
			},
		},
	},
}

// TestRealBytes_claimFanout pins, per claim atom of a real result, the pair
// orientation (base = the asset the maker sold), both amounts, the maker
// (G-address for an offer, pool-id hex for a pool) and the fanout OpIndex.
func TestRealBytes_claimFanout(t *testing.T) {
	for _, f := range realFrames {
		t.Run(f.name, func(t *testing.T) {
			ctx := realFrameContext(t, f)
			if !NewDecoder().Matches(ctx.Op) {
				t.Fatalf("Matches() = false for op type %s", ctx.Op.Body.Type)
			}
			outs, failed := NewDecoder().DecodeCounted(ctx)
			if failed != 0 {
				t.Fatalf("DecodeCounted failed = %d, want 0", failed)
			}
			if len(outs) != len(f.want) {
				t.Fatalf("got %d trades, want %d", len(outs), len(f.want))
			}
			for i, w := range f.want {
				ev, ok := outs[i].(TradeEvent)
				if !ok {
					t.Fatalf("claim %d: output is %T, want TradeEvent", i, outs[i])
				}
				assertRealClaim(t, i, ev, f, w)
			}
		})
	}
}

func assertRealClaim(t *testing.T, i int, ev TradeEvent, f realFrame, w wantClaim) {
	t.Helper()
	tr := ev.Trade
	if got := tr.Pair.String(); got != w.pair {
		t.Errorf("claim %d: Pair = %s, want %s", i, got, w.pair)
	}
	if tr.BaseAmount.String() != w.base || tr.QuoteAmount.String() != w.quote {
		t.Errorf("claim %d: amounts = %s/%s, want %s/%s", i, tr.BaseAmount.String(), tr.QuoteAmount.String(), w.base, w.quote)
	}
	if tr.Maker != w.maker {
		t.Errorf("claim %d: Maker = %s, want %s", i, tr.Maker, w.maker)
	}
	if tr.OpIndex != w.opIndex {
		t.Errorf("claim %d: OpIndex = %d, want %d", i, tr.OpIndex, w.opIndex)
	}
	if tr.Taker != f.txSource || tr.Ledger != f.ledger || tr.TxHash != f.txHash || !tr.Timestamp.Equal(f.closedAt) {
		t.Errorf("claim %d: taker/ledger/tx/ts = %s/%d/%s/%s, want %s/%d/%s/%s", i,
			tr.Taker, tr.Ledger, tr.TxHash, tr.Timestamp, f.txSource, f.ledger, f.txHash, f.closedAt)
	}
	if err := tr.Validate(); err != nil {
		t.Errorf("claim %d: Validate() = %v, want nil", i, err)
	}
}

func realFrameContext(t *testing.T, f realFrame) dispatcher.OpContext {
	t.Helper()
	var body xdr.OperationBody
	if err := xdr.SafeUnmarshalBase64(f.bodyB64, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var result xdr.OperationResult
	if err := xdr.SafeUnmarshalBase64(f.resultB64, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return dispatcher.OpContext{
		Ledger:   f.ledger,
		ClosedAt: f.closedAt,
		TxHash:   f.txHash,
		TxSource: f.txSource,
		OpIndex:  f.opIndex,
		Op:       xdr.Operation{Body: body},
		OpResult: result,
	}
}
