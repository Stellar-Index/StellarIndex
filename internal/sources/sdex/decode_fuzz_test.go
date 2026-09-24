package sdex

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/sdexclaim"
)

// Generative runs: go test -run=^$ -fuzz=^FuzzXxx$ -fuzztime=60s ./internal/sources/sdex
// Under a plain `go test` the seeds run as ordinary cases.

// fuzzAsset builds an xdr.Asset from raw fuzz bytes: native when
// native is set, alphanum4 for a code of <=4 bytes, alphanum12 otherwise.
// The code bytes are NOT sanitised — control bytes and interior NULs are
// exactly the on-chain shapes canonical.AssetFromXDR must reject.
func fuzzAsset(native bool, code, issuer []byte) xdr.Asset {
	if native {
		return xdr.MustNewNativeAsset()
	}
	var pk xdr.Uint256
	copy(pk[:], issuer)
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
	if len(code) <= 4 {
		var c xdr.AssetCode4
		copy(c[:], code)
		return xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: &xdr.AlphaNum4{AssetCode: c, Issuer: aid}}
	}
	var c xdr.AssetCode12
	copy(c[:], code)
	return xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum12, AlphaNum12: &xdr.AlphaNum12{AssetCode: c, Issuer: aid}}
}

// fuzzClaimAtom builds one ClaimAtom of the variant selected by kind
// (0 OrderBook, 1 LiquidityPool, 2 V0, anything else an unknown
// discriminant) from independent fuzz inputs.
func fuzzClaimAtom(kind uint8, seller []byte, sold, bought xdr.Asset, soldAmt, boughtAmt int64) xdr.ClaimAtom {
	var pk xdr.Uint256
	copy(pk[:], seller)
	switch kind % 4 {
	case 0:
		return xdr.ClaimAtom{Type: xdr.ClaimAtomTypeClaimAtomTypeOrderBook, OrderBook: &xdr.ClaimOfferAtom{
			SellerId:  xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk},
			OfferId:   7,
			AssetSold: sold, AmountSold: xdr.Int64(soldAmt),
			AssetBought: bought, AmountBought: xdr.Int64(boughtAmt),
		}}
	case 1:
		return xdr.ClaimAtom{Type: xdr.ClaimAtomTypeClaimAtomTypeLiquidityPool, LiquidityPool: &xdr.ClaimLiquidityAtom{
			LiquidityPoolId: xdr.PoolId(pk),
			AssetSold:       sold, AmountSold: xdr.Int64(soldAmt),
			AssetBought: bought, AmountBought: xdr.Int64(boughtAmt),
		}}
	case 2:
		return xdr.ClaimAtom{Type: xdr.ClaimAtomTypeClaimAtomTypeV0, V0: &xdr.ClaimOfferAtomV0{
			SellerEd25519: pk,
			OfferId:       7,
			AssetSold:     sold, AmountSold: xdr.Int64(soldAmt),
			AssetBought: bought, AmountBought: xdr.Int64(boughtAmt),
		}}
	}
	return xdr.ClaimAtom{Type: xdr.ClaimAtomType(3 + int32(kind))}
}

// atomLegs is the test's own destructuring of a ClaimAtom, written
// independently of decodeClaimAtom so the fuzz oracle is not the code
// under test.
func atomLegs(a xdr.ClaimAtom) (maker string, sold, bought xdr.Asset, soldAmt, boughtAmt xdr.Int64, ok bool) {
	switch a.Type {
	case xdr.ClaimAtomTypeClaimAtomTypeOrderBook:
		ob := a.MustOrderBook()
		m, _ := strkey.Encode(strkey.VersionByteAccountID, ob.SellerId.Ed25519[:])
		return m, ob.AssetSold, ob.AssetBought, ob.AmountSold, ob.AmountBought, true
	case xdr.ClaimAtomTypeClaimAtomTypeLiquidityPool:
		lp := a.MustLiquidityPool()
		return fmt.Sprintf("%x", lp.LiquidityPoolId[:]), lp.AssetSold, lp.AssetBought, lp.AmountSold, lp.AmountBought, true
	case xdr.ClaimAtomTypeClaimAtomTypeV0:
		v0 := a.MustV0()
		m, _ := strkey.Encode(strkey.VersionByteAccountID, v0.SellerEd25519[:])
		return m, v0.AssetSold, v0.AssetBought, v0.AmountSold, v0.AmountBought, true
	}
	return "", xdr.Asset{}, xdr.Asset{}, 0, 0, false
}

// assertTradeMatchesAtom checks every money- and identity-bearing field
// of a decoded trade against the atom it came from.
func assertTradeMatchesAtom(t *testing.T, tr canonical.Trade, atom xdr.ClaimAtom) {
	t.Helper()
	maker, sold, bought, soldAmt, boughtAmt, ok := atomLegs(atom)
	if !ok {
		t.Fatalf("decoded a trade from unknown atom type %d", atom.Type)
	}
	// ADR-0003: the amount must be the exact int64 on the wire, via
	// *big.Int — no int32/float narrowing, no sign loss, no leg swap.
	if got, want := tr.BaseAmount.BigInt(), big.NewInt(int64(soldAmt)); got.Cmp(want) != 0 {
		t.Fatalf("BaseAmount = %s, want sold %s", got, want)
	}
	if got, want := tr.QuoteAmount.BigInt(), big.NewInt(int64(boughtAmt)); got.Cmp(want) != 0 {
		t.Fatalf("QuoteAmount = %s, want bought %s", got, want)
	}
	wantBase, err := canonical.AssetFromXDR(sold)
	if err != nil {
		t.Fatalf("decoded a trade whose sold asset AssetFromXDR rejects: %v", err)
	}
	wantQuote, err := canonical.AssetFromXDR(bought)
	if err != nil {
		t.Fatalf("decoded a trade whose bought asset AssetFromXDR rejects: %v", err)
	}
	if tr.Pair.Base != wantBase || tr.Pair.Quote != wantQuote {
		t.Fatalf("Pair = %v/%v, want sold %v / bought %v", tr.Pair.Base, tr.Pair.Quote, wantBase, wantQuote)
	}
	if tr.Maker != maker {
		t.Fatalf("Maker = %q, want %q", tr.Maker, maker)
	}
	if tr.Source != SourceName {
		t.Fatalf("Source = %q, want %q", tr.Source, SourceName)
	}
}

// FuzzAmountFromInt64 pins amountFromInt64 to the *big.Int reference
// over the whole int64 domain, including the sign bit and the values
// above 2^31 / 2^53 a table test tends to skip.
func FuzzAmountFromInt64(f *testing.F) {
	for _, n := range []int64{
		0, 1, -1, 10_000_000, math.MaxInt32, math.MaxInt32 + 1, 1 << 53, (1 << 53) + 1,
		math.MaxInt64, math.MinInt64, -(1 << 40),
	} {
		f.Add(n)
	}
	f.Fuzz(func(t *testing.T, n int64) {
		got := amountFromInt64(xdr.Int64(n))
		if got.BigInt().Cmp(big.NewInt(n)) != 0 {
			t.Fatalf("amountFromInt64(%d) = %s", n, got)
		}
		if got.String() != strconv.FormatInt(n, 10) {
			t.Fatalf("amountFromInt64(%d).String() = %q", n, got.String())
		}
	})
}

// FuzzDecodeClaimAtom drives decodeClaimAtom over arbitrary atoms and
// asserts: (1) it accepts exactly the atoms sdexclaim.IsRealTrade counts,
// so the ADR-0033 census oracle equals COUNT(trades) by construction;
// (2) every accepted atom decodes to its own exact amounts, assets and
// maker; (3) the both-legs-non-positive no-op and unknown variants are
// rejected with the documented sentinels, never mis-decoded.
func FuzzDecodeClaimAtom(f *testing.F) {
	iss := []byte{1, 2, 3}
	f.Add(uint8(0), []byte{9}, false, []byte("USDC"), iss, true, []byte(nil), []byte(nil), int64(10_000_000), int64(3_000_000), uint8(0), uint16(0))
	f.Add(uint8(1), []byte{0xab}, true, []byte(nil), []byte(nil), false, []byte("AQUA"), iss, int64(1), int64(0), uint8(3), uint16(5))
	f.Add(uint8(2), []byte{7}, false, []byte("LONGCODE12AB"), iss, true, []byte(nil), []byte(nil), int64(math.MaxInt64), int64(math.MaxInt32)+1, uint8(99), uint16(999))
	f.Add(uint8(0), []byte{9}, false, []byte("US\x01D"), iss, true, []byte(nil), []byte(nil), int64(5), int64(5), uint8(0), uint16(0))
	f.Add(uint8(0), []byte{9}, true, []byte(nil), []byte(nil), true, []byte(nil), []byte(nil), int64(5), int64(5), uint8(0), uint16(0))
	f.Add(uint8(0), []byte{9}, false, []byte("USDC"), iss, true, []byte(nil), []byte(nil), int64(0), int64(0), uint8(0), uint16(0))
	f.Add(uint8(0), []byte{9}, false, []byte("USDC"), iss, true, []byte(nil), []byte(nil), int64(-4), int64(0), uint8(0), uint16(0))
	f.Add(uint8(3), []byte{9}, false, []byte("USDC"), iss, true, []byte(nil), []byte(nil), int64(5), int64(5), uint8(0), uint16(0))

	closedAt := time.Unix(1_700_000_000, 0).UTC()
	const txHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	f.Fuzz(func(t *testing.T, kind uint8, seller []byte,
		soldNative bool, soldCode, soldIssuer []byte,
		boughtNative bool, boughtCode, boughtIssuer []byte,
		soldAmt, boughtAmt int64, opIdx uint8, tradeIdx uint16,
	) {
		if len(soldCode) > 12 || len(boughtCode) > 12 {
			t.Skip()
		}
		// The protocol caps offers crossed per op at 1000, below the stride.
		tradeIdx %= opIndexFanoutStride
		atom := fuzzClaimAtom(kind, seller,
			fuzzAsset(soldNative, soldCode, soldIssuer),
			fuzzAsset(boughtNative, boughtCode, boughtIssuer),
			soldAmt, boughtAmt)

		tr, err := decodeClaimAtom(atom, 42, closedAt, txHash, int(opIdx), int(tradeIdx), "GTAKER")

		if got, want := err == nil, sdexclaim.IsRealTrade(atom); got != want {
			t.Fatalf("decodeClaimAtom accepted=%v but sdexclaim.IsRealTrade=%v (err=%v)", got, want, err)
		}
		if err != nil {
			if !errors.Is(err, ErrMalformedClaimAtom) && !errors.Is(err, ErrUnknownClaimAtomType) {
				t.Fatalf("rejection without a documented sentinel: %v", err)
			}
			if kind%4 == 3 && !errors.Is(err, ErrUnknownClaimAtomType) {
				t.Fatalf("unknown atom type rejected with %v, want ErrUnknownClaimAtomType", err)
			}
			return
		}
		if soldAmt <= 0 && boughtAmt <= 0 {
			t.Fatalf("both-legs-non-positive atom decoded to a trade: sold=%d bought=%d", soldAmt, boughtAmt)
		}
		assertTradeMatchesAtom(t, tr, atom)
		if want := uint32(opIdx)*opIndexFanoutStride + uint32(tradeIdx); tr.OpIndex != want {
			t.Fatalf("OpIndex = %d, want %d", tr.OpIndex, want)
		}
		if tr.Ledger != 42 || tr.TxHash != txHash || !tr.Timestamp.Equal(closedAt) || tr.Taker != "GTAKER" {
			t.Fatalf("context not propagated: %+v", tr)
		}
		// A two-sided positive fill is a complete, storable trade.
		if soldAmt > 0 && boughtAmt > 0 {
			if verr := tr.Validate(); verr != nil {
				t.Fatalf("decoded positive fill fails Validate: %v", verr)
			}
		}
	})
}

// tradeOpTypes are the op types matchesTradeOp admits, plus one it
// must not (Payment) to prove foreign op shapes yield no trades.
var tradeOpTypes = []xdr.OperationType{
	xdr.OperationTypeManageSellOffer,
	xdr.OperationTypeManageBuyOffer,
	xdr.OperationTypeCreatePassiveSellOffer,
	xdr.OperationTypePathPaymentStrictReceive,
	xdr.OperationTypePathPaymentStrictSend,
	xdr.OperationTypePayment,
}

// resultArmAccepted reports whether extractClaimAtoms may read claims
// from a result arm of type trType for an op of type opType: the arm
// must be the op's own, except that core emits CreatePassiveSellOffer
// results under the ManageSellOffer arm.
func resultArmAccepted(opType, trType xdr.OperationType) bool {
	if opType == trType {
		return true
	}
	return opType == xdr.OperationTypeCreatePassiveSellOffer && trType == xdr.OperationTypeManageSellOffer
}

// FuzzDecodeOperationResultXDR feeds arbitrary XDR-encoded
// OperationResults (seeded from every trade-op success shape) through the
// production decoder and the audit seam, asserting they agree with each
// other and with the census predicate, that each emitted trade carries its
// own atom's exact amounts, that op_index values are unique, and that a
// result arm foreign to the op type yields nothing.
func FuzzDecodeOperationResultXDR(f *testing.F) {
	for _, seed := range operationResultSeeds(f) {
		f.Add(seed.opSel, seed.raw, uint8(2), false)
	}
	f.Fuzz(func(t *testing.T, opSel uint8, raw []byte, opIdx uint8, withOpSource bool) {
		var res xdr.OperationResult
		if err := xdr.SafeUnmarshal(raw, &res); err != nil {
			t.Skip()
		}
		opType := tradeOpTypes[int(opSel)%len(tradeOpTypes)]
		op := xdr.Operation{Body: xdr.OperationBody{Type: opType}}
		ctx := dispatcher.OpContext{
			Ledger: 7, ClosedAt: time.Unix(1_700_000_000, 0).UTC(),
			TxHash: "bb", OpIndex: int(opIdx), Op: op, OpResult: res, TxSource: "GTX",
		}
		if withOpSource {
			ctx.OpSource = "GOP"
		}

		atoms := extractClaimAtoms(op, res)
		d := NewDecoder()
		if d.Matches(op) != (opType != xdr.OperationTypePayment) {
			t.Fatalf("Matches(%v) wrong", opType)
		}
		if opType == xdr.OperationTypePayment && len(atoms) != 0 {
			t.Fatalf("non-trade op yielded %d claim atoms", len(atoms))
		}
		if len(atoms) > 0 {
			if res.Code != xdr.OperationResultCodeOpInner || res.Tr == nil || !resultArmAccepted(opType, res.Tr.Type) {
				t.Fatalf("claims read from a foreign result shape: code=%v tr=%v op=%v", res.Code, res.Tr, opType)
			}
		}

		events, failed := d.DecodeCounted(ctx)
		claims, drops := AuditOp(op, res)
		if claims != len(atoms) {
			t.Fatalf("AuditOp claims = %d, want %d", claims, len(atoms))
		}
		if failed != len(drops) || len(events)+failed != len(atoms) {
			t.Fatalf("events=%d failed=%d drops=%d atoms=%d do not balance", len(events), failed, len(drops), len(atoms))
		}
		if realN := sdexclaim.RealTradeCount(atoms); realN != len(events) {
			t.Fatalf("emitted %d trades, sdexclaim.RealTradeCount = %d", len(events), realN)
		}
		plain, err := d.Decode(ctx)
		if err != nil || len(plain) != len(events) {
			t.Fatalf("Decode = %d events, %v; DecodeCounted = %d", len(plain), err, len(events))
		}

		wantTaker := "GTX"
		if withOpSource {
			wantTaker = "GOP"
		}
		seen := map[uint32]bool{}
		base := uint32(opIdx) * opIndexFanoutStride
		for _, ev := range events {
			tr := ev.(TradeEvent).Trade
			if tr.OpIndex < base || tr.OpIndex >= base+uint32(len(atoms)) {
				t.Fatalf("OpIndex %d outside [%d,%d)", tr.OpIndex, base, base+uint32(len(atoms)))
			}
			if seen[tr.OpIndex] {
				t.Fatalf("duplicate OpIndex %d", tr.OpIndex)
			}
			seen[tr.OpIndex] = true
			assertTradeMatchesAtom(t, tr, atoms[tr.OpIndex-base])
			if tr.Taker != wantTaker {
				t.Fatalf("Taker = %q, want %q", tr.Taker, wantTaker)
			}
		}
	})
}

type opResultSeed struct {
	opSel uint8
	raw   []byte
}

// operationResultSeeds marshals one success result per trade-op arm
// (and the passive-under-manage-sell arm core really emits), each
// carrying an OrderBook, LiquidityPool and V0 claim.
func operationResultSeeds(f *testing.F) []opResultSeed {
	f.Helper()
	usdc := fuzzAsset(false, []byte("USDC"), []byte{1})
	xlm := fuzzAsset(true, nil, nil)
	claims := []xdr.ClaimAtom{
		fuzzClaimAtom(0, []byte{9}, usdc, xlm, 10_000_000, 30_000_000),
		fuzzClaimAtom(1, []byte{0xab}, xlm, usdc, math.MaxInt32+1, 1),
		fuzzClaimAtom(2, []byte{7}, usdc, xlm, 0, 5),
		fuzzClaimAtom(0, []byte{9}, usdc, xlm, 0, 0),
	}
	manageSell := &xdr.ManageSellOfferResult{
		Code:    xdr.ManageSellOfferResultCodeManageSellOfferSuccess,
		Success: &xdr.ManageOfferSuccessResult{OffersClaimed: claims, Offer: xdr.ManageOfferSuccessResultOffer{Effect: xdr.ManageOfferEffectManageOfferDeleted}},
	}
	manageBuy := &xdr.ManageBuyOfferResult{
		Code:    xdr.ManageBuyOfferResultCodeManageBuyOfferSuccess,
		Success: &xdr.ManageOfferSuccessResult{OffersClaimed: claims, Offer: xdr.ManageOfferSuccessResultOffer{Effect: xdr.ManageOfferEffectManageOfferDeleted}},
	}
	last := xdr.SimplePaymentResult{Destination: xdr.MustAddress("GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"), Asset: xlm, Amount: 1}
	trs := []struct {
		sel uint8
		tr  xdr.OperationResultTr
	}{
		{0, xdr.OperationResultTr{Type: xdr.OperationTypeManageSellOffer, ManageSellOfferResult: manageSell}},
		{1, xdr.OperationResultTr{Type: xdr.OperationTypeManageBuyOffer, ManageBuyOfferResult: manageBuy}},
		{2, xdr.OperationResultTr{Type: xdr.OperationTypeCreatePassiveSellOffer, CreatePassiveSellOfferResult: manageSell}},
		{2, xdr.OperationResultTr{Type: xdr.OperationTypeManageSellOffer, ManageSellOfferResult: manageSell}},
		{3, xdr.OperationResultTr{Type: xdr.OperationTypePathPaymentStrictReceive, PathPaymentStrictReceiveResult: &xdr.PathPaymentStrictReceiveResult{
			Code:    xdr.PathPaymentStrictReceiveResultCodePathPaymentStrictReceiveSuccess,
			Success: &xdr.PathPaymentStrictReceiveResultSuccess{Offers: claims, Last: last},
		}}},
		{4, xdr.OperationResultTr{Type: xdr.OperationTypePathPaymentStrictSend, PathPaymentStrictSendResult: &xdr.PathPaymentStrictSendResult{
			Code:    xdr.PathPaymentStrictSendResultCodePathPaymentStrictSendSuccess,
			Success: &xdr.PathPaymentStrictSendResultSuccess{Offers: claims, Last: last},
		}}},
		{4, xdr.OperationResultTr{Type: xdr.OperationTypePathPaymentStrictSend, PathPaymentStrictSendResult: &xdr.PathPaymentStrictSendResult{
			Code: xdr.PathPaymentStrictSendResultCodePathPaymentStrictSendUnderDestmin,
		}}},
	}
	out := make([]opResultSeed, 0, len(trs))
	for _, s := range trs {
		tr := s.tr
		raw, err := xdr.OperationResult{Code: xdr.OperationResultCodeOpInner, Tr: &tr}.MarshalBinary()
		if err != nil {
			f.Fatalf("marshal seed %d: %v", s.sel, err)
		}
		out = append(out, opResultSeed{opSel: s.sel, raw: raw})
	}
	return out
}
