package classicmovements

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// realOpFixtures are the mainnet (body, result) XDR pairs from
// real_bytes_test.go, reused as the seed corpus for FuzzDecodeOp.
var realOpFixtures = [][2]string{
	{"AAAAAQAAAABs5He80fq3sKhGa7EvGdEJ9HUvB6qJt46lPuC0SkcnwwAAAAFYWEEAAAAAALh5cFAWaLQ6ZlreaIAMkEayxj7HzipA3vLbAaJXUi9wAAAAAo4EFBg=", "AAAAAAAAAAEAAAAA"},
	{"AAAAAQAAAACGM7BaSUMQn9EXPK0RmuUNAVBUgmUpCQKCpCXwq5gaBAAAAAFVU0RDAAAAADuZETgO/piLoKiQDrHP5E82b32+lGvtB3JA9/Yk3xXFAAAAAC7HpUY=", "AAAAAAAAAAEAAAAA"},
	{"AAAAAQAAAABjDz9pTvtUpLGFEobNwdCiPL/fSI9lFaS0EGC05did6QAAAAAAAAAAAAAACg==", "AAAAAAAAAAEAAAAA"},
	{"AAAAAAAAAABEhNo2pKcX+rr5g64sjcqJtM316fADqGjbpQ4fEgr+uAAAAACi2GcH", "AAAAAAAAAAAAAAAA"},
	{"AAAAAQAAAAA3mZB7bnHFoxwyZpTTMRdQvzdJKQrlJgLjpW6jCNCEtwAAAAJSQU5ESTEAAAAAAAAAAAAAjNPgGB5OHDkXLDlbk4XOaCVSZtwsVbgQoA84G19p0YkAAAAAAAAAAQ==", "/////g=="},
	{"AAAAAgAAAAAAAAAAALjvwAAAAAAWcHHVU3Pe9F+qSovW7OF1H73pa76MnRgmpMUXGO0/XQAAAAFTT05ZAAAAALsLcXbIOISuH+pMVlQ3U/ziOgkUcfkQQHnKR+iFALP4AAMyi5RMQAAAAAAA", "AAAAAAAAAAIAAAAAAAAAAQAAAAEAAAAAtBJMX3t5P0oPc7eVIwj3MXMLwgQZNa6roDUwTYg44x8AAAAAOJxqdAAAAAFTT05ZAAAAALsLcXbIOISuH+pMVlQ3U/ziOgkUcfkQQHnKR+iFALP4AAMyi5RMQAAAAAAAAAAAAAC3GwAAAAAAFnBx1VNz3vRfqkqL1uzhdR+96Wu+jJ0YJqTFFxjtP10AAAABU09OWQAAAAC7C3F2yDiErh/qTFZUN1P84joJFHH5EEB5ykfohQCz+AADMouUTEAA"},
	{"AAAAAgAAAAAAAAAABPtnBgAAAAAYyCYed5ULPCCZ1wtggxYUtoK6Hu5uyhKp0+DNkFyZ1gAAAAAAAAAABPtuGAAAAAEAAAABU0hJQgAAAABa7upQ7YJtt/jfTu+F1mmJZbUiSrjeJ9cNliPnYMns5w==", "AAAAAAAAAAIAAAAAAAAAAgAAAAEAAAAAbaHTfp0wRYC9BeJP51OYornphxPI+aozmlv3trm4KbAAAAAAOKJmcAAAAAFTSElCAAAAAFru6lDtgm23+N9O74XWaYlltSJKuN4n1w2WI+dgyeznAAAAjC6sEZoAAAAAAAAAAAT7J2kAAAACd3xyt7p6rXDgES6e0Vie5PU5pKZvgUqA2rAbTcL4mHEAAAAAAAAAAAT7bhgAAAABU0hJQgAAAABa7upQ7YJtt/jfTu+F1mmJZbUiSrjeJ9cNliPnYMns5wAAAIwurBGaAAAAABjIJh53lQs8IJnXC2CDFhS2groe7m7KEqnT4M2QXJnWAAAAAAAAAAAE+24Y"},
	{"AAAADQAAAAJhaVhET0dFAAAAAAAAAAAAHmZ99WHIvNYnad6AHqEYtIx8rynNCdIrpMMan93+ee8AAAAAC+vCAAAAAAAbwSApPmwboWhG14u1quvJh4f0t3hW09pLOIa1MPeGVwAAAAFBUVVBAAAAAFuULlOsM8j9CoDMfBsahdfYOKnEGXeq0Ys68Ff44z3wAAAAAAAAbpIAAAABAAAAAA==", "AAAAAAAAAA0AAAAAAAAAAgAAAAEAAAAAQJzSfng2B5F/ARo1w+R7fYQ1nuZE0ILRfdDZNwTNIh8AAAAAOK4wUAAAAAAAAAAAAAAETAAAAAJhaVhET0dFAAAAAAAAAAAAHmZ99WHIvNYnad6AHqEYtIx8rynNCdIrpMMan93+ee8AAAAAC+vCAAAAAAEAAAAAQ5f/457bn13BXKa5Mccm5n80F2Y9HiOh0k2x4c4FIV4AAAAAOK47NAAAAAFBUVVBAAAAAFuULlOsM8j9CoDMfBsahdfYOKnEGXeq0Ys68Ff44z3wAAAAAAAA+DkAAAAAAAAAAAAABEwAAAAAG8EgKT5sG6FoRteLtarryYeH9Ld4VtPaSziGtTD3hlcAAAABQVFVQQAAAABblC5TrDPI/QqAzHwbGoXX2DipxBl3qtGLOvBX+OM98AAAAAAAAPg5"},
	{"AAAADgAAAAFHQUxBAAAAADV6FeHCtmgz8V4u/kE1liwByrbw7SK8T5xatMG3j16DAAAAAAAF57gAAAACAAAAAAAAAACEKCUFXS4fGHiDDkqG2fsp03cR+Rd9bRJKybyy/rBgjgAAAAMAAAABAAAABAAAAABiLPP5AAAAAAAAAAA+CSgxOgl5fsc6se+nt2OaXHhgjaBK88g+wvrbZ7xZUgAAAAUAAAAAAAk6gA==", "AAAAAAAAAA4AAAAAAAAAAAZiRenTfqxyI9zYHNhROvZuqvaj0+hcn77xY+YUzgCd"},
	{"AAAADwAAAAD5qm+e1LhKIbwH2WwTRW1z21a8SyVBB6JhPsbYJ17Asg==", "AAAAAAAAAA8AAAAA"},
	{"AAAAEwAAAAJJcmFxaURpbmFyAAAAAAAAQqpSc3O4BsnF00z3DCgHZslFSsG9q8vhnVv8SUCYlBIAAAAAixcHcr3R/h+JDHbDxqHVbjXTcvfNj79TgUAVVq6QOfwAAAAFpDh1gA==", "AAAAAAAAABMAAAAA"},
	{"AAAAFAAAAAAndivT9qSWT2XUT+ui1Gnkzi+2ve7uG05Gx6EErpxlWw==", "AAAAAAAAABT////9"},
	{"AAAACAAAAAA0/LLInLnVygX6rN/I+sRIih1i5JrUYv0elzaRi/8EBg==", "AAAAAAAAAAgAAAAAAAAAAA7mshw="},
}

func fuzzAccountID(seed byte) xdr.AccountId {
	var pub xdr.Uint256
	pub[0] = seed
	pub[31] = 0x5a
	return xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
}

func fuzzCreditAsset(code string, issuerSeed byte) xdr.Asset {
	var c [4]byte
	copy(c[:], code)
	return xdr.Asset{
		Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{AssetCode: c, Issuer: fuzzAccountID(issuerSeed)},
	}
}

// fuzzDecoder is NewDecoder with a small ring: NewDecoder preallocates
// an 8M-slot eviction ring, far too heavy to build per fuzz iteration.
func fuzzDecoder(ring int) *Decoder {
	return &Decoder{
		balances:     make(map[string]claimableBalanceInfo),
		balanceOrder: make([]string, ring),
	}
}

func bigSum(xs ...int64) *big.Int {
	s := new(big.Int)
	for _, x := range xs {
		s.Add(s, big.NewInt(x))
	}
	return s
}

var maxInt64 = big.NewInt(math.MaxInt64)

// fuzzAtom is one record of FuzzPathPaymentStrictReceiveSourceAmount's
// input: 10 bytes = [claim-atom variant][asset selector][int64 big-endian].
type fuzzAtom struct {
	variant byte // %4: 0 OrderBook, 1 LiquidityPool, 2 V0, 3 unknown discriminant
	matches bool // AssetBought == SendAsset
	amount  int64
}

func parseFuzzAtoms(data []byte) []fuzzAtom {
	var out []fuzzAtom
	for len(data) >= 10 && len(out) < 12 {
		out = append(out, fuzzAtom{
			variant: data[0] % 4,
			matches: data[1]%3 == 0,
			amount:  int64(binary.BigEndian.Uint64(data[2:10])), //nolint:gosec // reinterpreting fuzz bytes as a signed amount is the point.
		})
		data = data[10:]
	}
	return out
}

func encodeFuzzAtoms(atoms ...fuzzAtom) []byte {
	var b []byte
	for _, a := range atoms {
		sel := byte(1)
		if a.matches {
			sel = 0
		}
		var amt [8]byte
		binary.BigEndian.PutUint64(amt[:], uint64(a.amount)) //nolint:gosec // round-trips parseFuzzAtoms.
		b = append(b, a.variant, sel)
		b = append(b, amt[:]...)
	}
	return b
}

// FuzzPathPaymentStrictReceiveSourceAmount checks the StrictReceive
// source-leg derivation against a big.Int reference: the result is the
// exact sum of the leading run of claim atoms whose AssetBought is the
// SendAsset, and anything the reference cannot represent as a positive
// int64 stroop amount must be an error, never a wrapped value.
func FuzzPathPaymentStrictReceiveSourceAmount(f *testing.F) {
	m := func(amt int64) fuzzAtom { return fuzzAtom{matches: true, amount: amt} }
	f.Add([]byte{}, int64(500))
	f.Add(encodeFuzzAtoms(m(100)), int64(7))
	f.Add(encodeFuzzAtoms(m(60), fuzzAtom{variant: 1, matches: true, amount: 40}), int64(7))
	f.Add(encodeFuzzAtoms(m(100), fuzzAtom{amount: 50}, m(900)), int64(7))
	f.Add(encodeFuzzAtoms(fuzzAtom{amount: 100}), int64(7))
	f.Add(encodeFuzzAtoms(m(math.MaxInt64), m(math.MaxInt64), m(3)), int64(7))
	f.Add(encodeFuzzAtoms(m(5), m(-3)), int64(7))
	f.Add(encodeFuzzAtoms(fuzzAtom{variant: 3, matches: true, amount: 1}), int64(7))
	f.Add(encodeFuzzAtoms(m(0)), int64(7))
	f.Add(encodeFuzzAtoms(m(math.MaxInt64-1), m(1)), int64(7))
	f.Add(encodeFuzzAtoms(fuzzAtom{variant: 2, matches: true, amount: 100}), int64(7))

	sendAsset := fuzzCreditAsset("SEND", 0x01)
	// Same code, different issuer: an impersonating asset must never be
	// counted as the SendAsset.
	otherAssets := []xdr.Asset{fuzzCreditAsset("SEND", 0x02), xdr.MustNewNativeAsset()}

	f.Fuzz(func(t *testing.T, data []byte, lastAmount int64) {
		atoms := parseFuzzAtoms(data)
		claims := make([]xdr.ClaimAtom, 0, len(atoms))
		for i, a := range atoms {
			bought := sendAsset
			if !a.matches {
				bought = otherAssets[i%len(otherAssets)]
			}
			amt := xdr.Int64(a.amount)
			switch a.variant {
			case 0:
				claims = append(claims, xdr.ClaimAtom{
					Type:      xdr.ClaimAtomTypeClaimAtomTypeOrderBook,
					OrderBook: &xdr.ClaimOfferAtom{AssetBought: bought, AmountBought: amt},
				})
			case 1:
				claims = append(claims, xdr.ClaimAtom{
					Type:          xdr.ClaimAtomTypeClaimAtomTypeLiquidityPool,
					LiquidityPool: &xdr.ClaimLiquidityAtom{AssetBought: bought, AmountBought: amt},
				})
			case 2:
				claims = append(claims, xdr.ClaimAtom{
					Type: xdr.ClaimAtomTypeClaimAtomTypeV0,
					V0:   &xdr.ClaimOfferAtomV0{AssetBought: bought, AmountBought: amt},
				})
			default:
				claims = append(claims, xdr.ClaimAtom{Type: xdr.ClaimAtomType(99)})
			}
		}

		got, err := pathPaymentStrictReceiveSourceAmount(sendAsset, claims, xdr.Int64(lastAmount))

		want, wantOK := refStrictReceiveSourceAmount(atoms, lastAmount)
		if !wantOK {
			if err == nil {
				t.Fatalf("atoms=%+v: got %d, want an error", atoms, got)
			}
			return
		}
		if err != nil {
			t.Fatalf("atoms=%+v: unexpected error %v (want %s)", atoms, err, want)
		}
		if big.NewInt(int64(got)).Cmp(want) != 0 {
			t.Fatalf("atoms=%+v: got %d, want %s", atoms, got, want)
		}
	})
}

// refStrictReceiveSourceAmount is the big.Int model of
// pathPaymentStrictReceiveSourceAmount. ok=false means the input must be
// rejected.
func refStrictReceiveSourceAmount(atoms []fuzzAtom, lastAmount int64) (*big.Int, bool) {
	if len(atoms) == 0 {
		return big.NewInt(lastAmount), true
	}
	sum := new(big.Int)
	for i, a := range atoms {
		if a.variant == 3 {
			return nil, false
		}
		if !a.matches {
			if i == 0 {
				return nil, false
			}
			break
		}
		if a.amount < 0 {
			return nil, false
		}
		sum.Add(sum, big.NewInt(a.amount))
	}
	if sum.Sign() <= 0 || sum.Cmp(maxInt64) > 0 {
		return nil, false
	}
	return sum, true
}

// FuzzLiquidityPoolReserveDeltas checks the LP deposit/withdraw legs
// against big.Int reserve arithmetic: every emitted leg is the exact
// reserve delta on the right asset and leg index, and a reserve set the
// protocol cannot produce (a negative reserve, a wrong-signed delta) is
// rejected, never emitted as a wrapped int64.
func FuzzLiquidityPoolReserveDeltas(f *testing.F) {
	const (
		haveState   = 1 << 0
		haveUpdated = 1 << 1
		haveRemoved = 1 << 2
		isWithdraw  = 1 << 3
	)
	f.Add(int64(1000), int64(2000), int64(1100), int64(2200), uint8(haveState|haveUpdated))
	f.Add(int64(0), int64(0), int64(500), int64(700), uint8(haveUpdated))
	f.Add(int64(1000), int64(2000), int64(900), int64(1800), uint8(haveState|haveUpdated|isWithdraw))
	f.Add(int64(1000), int64(2000), int64(0), int64(0), uint8(haveState|haveRemoved|isWithdraw))
	f.Add(int64(1000), int64(2000), int64(1000), int64(1800), uint8(haveState|haveUpdated|isWithdraw))
	f.Add(int64(math.MinInt64), int64(0), int64(1), int64(1), uint8(haveState|haveUpdated))
	f.Add(int64(-5), int64(1), int64(10), int64(10), uint8(haveState|haveUpdated))
	f.Add(int64(math.MinInt64), int64(5), int64(1), int64(1), uint8(haveState|haveUpdated|isWithdraw))
	f.Add(int64(1000), int64(2000), int64(1100), int64(2000), uint8(haveState|haveUpdated))
	f.Add(int64(1000), int64(2000), int64(900), int64(2000), uint8(haveState|haveUpdated|isWithdraw))
	f.Add(int64(5), int64(-1), int64(10), int64(10), uint8(haveState|haveUpdated))
	f.Add(int64(0), int64(0), int64(1)<<40, int64(3)<<33, uint8(haveUpdated))

	assetA := fuzzCreditAsset("AAA", 0x0a)
	assetB := xdr.MustNewNativeAsset()
	fromAddr := fuzzAccountID(0x33).Address()
	poolID := xdr.PoolId{0xab}
	wantPoolID := "ab" + strings.Repeat("0", 62)

	f.Fuzz(func(t *testing.T, beforeA, beforeB, afterA, afterB int64, flags uint8) {
		st, up, rm := flags&haveState != 0, flags&haveUpdated != 0, flags&haveRemoved != 0
		withdraw := flags&isWithdraw != 0
		var changes []EntryChangeXDR
		if st {
			changes = append(changes, EntryChangeXDR{ChangeType: "state", Entry: fuzzPoolEntry(assetA, assetB, beforeA, beforeB)})
		}
		if up {
			changes = append(changes, EntryChangeXDR{ChangeType: "updated", Entry: fuzzPoolEntry(assetA, assetB, afterA, afterB)})
		}
		if rm {
			changes = append(changes, EntryChangeXDR{ChangeType: "removed"})
		}

		op, result, kind := fuzzLPOp(withdraw, poolID)
		got, err := DecodeLiquidityPoolOp(9, time.Unix(0, 0), "tx", 3, fromAddr, op, result, changes)

		type leg struct {
			idx    uint32
			asset  xdr.Asset
			amount *big.Int
		}
		var want []leg
		var wantErr error
		switch {
		case !withdraw && !up, withdraw && !st, withdraw && !up && !rm:
			wantErr = ErrEntryChangesUnavailable
		default:
			bA, bB, aA, aB := beforeA, beforeB, afterA, afterB
			if !st {
				bA, bB = 0, 0
			}
			if !up {
				aA, aB = 0, 0
			}
			if bA < 0 || bB < 0 || aA < 0 || aB < 0 {
				wantErr = ErrMalformedMovement
				break
			}
			dA, dB := bigSum(aA, -bA), bigSum(aB, -bB)
			if withdraw {
				dA.Neg(dA)
				dB.Neg(dB)
				if dA.Sign() < 0 || dB.Sign() < 0 || (dA.Sign() == 0 && dB.Sign() == 0) {
					wantErr = ErrMalformedMovement
					break
				}
				if dA.Sign() > 0 {
					want = append(want, leg{0, assetA, dA})
				}
				if dB.Sign() > 0 {
					want = append(want, leg{1, assetB, dB})
				}
				break
			}
			if dA.Sign() <= 0 || dB.Sign() <= 0 {
				wantErr = ErrMalformedMovement
				break
			}
			want = append(want, leg{0, assetA, dA}, leg{1, assetB, dB})
		}

		if wantErr != nil {
			if !errors.Is(err, wantErr) {
				t.Fatalf("flags=%04b before=(%d,%d) after=(%d,%d): err=%v movements=%d, want %v",
					flags, beforeA, beforeB, afterA, afterB, err, len(got), wantErr)
			}
			return
		}
		if err != nil {
			t.Fatalf("flags=%04b before=(%d,%d) after=(%d,%d): unexpected error %v", flags, beforeA, beforeB, afterA, afterB, err)
		}
		if len(got) != len(want) {
			t.Fatalf("got %d legs, want %d", len(got), len(want))
		}
		wantFrom, wantTo := fromAddr, ""
		if withdraw {
			wantFrom, wantTo = "", fromAddr
		}
		for i, w := range want {
			g := got[i]
			if g.Kind != kind || g.LegIndex != w.idx || g.Asset != xdrjson.AssetID(w.asset) || g.Amount.BigInt().Cmp(w.amount) != 0 {
				t.Fatalf("leg %d = {%s leg=%d %s %s}, want {%s leg=%d %s %s}", i,
					g.Kind, g.LegIndex, g.Asset, g.Amount, kind, w.idx, xdrjson.AssetID(w.asset), w.amount)
			}
			if g.FromAddress != wantFrom || g.ToAddress != wantTo {
				t.Fatalf("leg %d from/to = %q/%q, want %q/%q", i, g.FromAddress, g.ToAddress, wantFrom, wantTo)
			}
			if g.Attributes["pool_id"] != wantPoolID {
				t.Fatalf("leg %d pool_id = %v", i, g.Attributes["pool_id"])
			}
		}
	})
}

func fuzzLPOp(withdraw bool, poolID xdr.PoolId) (xdr.Operation, xdr.OperationResult, Kind) {
	if withdraw {
		return xdr.Operation{Body: xdr.OperationBody{
				Type:                    xdr.OperationTypeLiquidityPoolWithdraw,
				LiquidityPoolWithdrawOp: &xdr.LiquidityPoolWithdrawOp{LiquidityPoolId: poolID},
			}}, xdr.OperationResult{Code: xdr.OperationResultCodeOpInner, Tr: &xdr.OperationResultTr{
				Type:                        xdr.OperationTypeLiquidityPoolWithdraw,
				LiquidityPoolWithdrawResult: &xdr.LiquidityPoolWithdrawResult{Code: xdr.LiquidityPoolWithdrawResultCodeLiquidityPoolWithdrawSuccess},
			}},
			KindLiquidityPoolWithdraw
	}
	return xdr.Operation{Body: xdr.OperationBody{
			Type:                   xdr.OperationTypeLiquidityPoolDeposit,
			LiquidityPoolDepositOp: &xdr.LiquidityPoolDepositOp{LiquidityPoolId: poolID},
		}}, xdr.OperationResult{Code: xdr.OperationResultCodeOpInner, Tr: &xdr.OperationResultTr{
			Type:                       xdr.OperationTypeLiquidityPoolDeposit,
			LiquidityPoolDepositResult: &xdr.LiquidityPoolDepositResult{Code: xdr.LiquidityPoolDepositResultCodeLiquidityPoolDepositSuccess},
		}},
		KindLiquidityPoolDeposit
}

func fuzzPoolEntry(assetA, assetB xdr.Asset, reserveA, reserveB int64) *xdr.LedgerEntry {
	return &xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeLiquidityPool,
		LiquidityPool: &xdr.LiquidityPoolEntry{Body: xdr.LiquidityPoolEntryBody{
			Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct,
			ConstantProduct: &xdr.LiquidityPoolEntryConstantProduct{
				Params:   xdr.LiquidityPoolConstantProductParameters{AssetA: assetA, AssetB: assetB, Fee: 30},
				ReserveA: xdr.Int64(reserveA),
				ReserveB: xdr.Int64(reserveB),
			},
		}},
	}}
}

// FuzzDecodeOp drives arbitrary (OperationBody, OperationResult) XDR
// through the op-only decode surface. Properties: nothing out of scope
// is decoded; a failed op moves nothing; every movement's amount is the
// exact int64 field the protocol defines for its kind (no truncation, no
// sign flip); counterparties are G-strkeys, never M-strkeys; and every
// error is a classified sentinel.
func FuzzDecodeOp(f *testing.F) {
	for _, fx := range realOpFixtures {
		body, err := base64.StdEncoding.DecodeString(fx[0])
		if err != nil {
			f.Fatal(err)
		}
		result, err := base64.StdEncoding.DecodeString(fx[1])
		if err != nil {
			f.Fatal(err)
		}
		f.Add(body, result)
	}
	txSource := fuzzAccountID(0x44).Address()

	f.Fuzz(func(t *testing.T, bodyXDR, resultXDR []byte) {
		var body xdr.OperationBody
		if err := xdr.SafeUnmarshal(bodyXDR, &body); err != nil {
			return
		}
		var result xdr.OperationResult
		if err := xdr.SafeUnmarshal(resultXDR, &result); err != nil {
			return
		}
		op := xdr.Operation{Body: body}
		d := fuzzDecoder(4)

		got, err := d.decodeOp(7, time.Unix(1, 0), "tx", 5, txSource, op, result)
		if !d.Matches(op) {
			if !errors.Is(err, ErrUnsupportedOpType) || len(got) != 0 {
				t.Fatalf("out-of-scope %s: got %d movements err=%v, want ErrUnsupportedOpType", body.Type, len(got), err)
			}
			return
		}
		if err != nil {
			if !errors.Is(err, ErrMalformedMovement) {
				t.Fatalf("%s: unclassified error %v", body.Type, err)
			}
			return
		}
		if result.Code != xdr.OperationResultCodeOpInner && len(got) != 0 {
			t.Fatalf("%s: result code %s moved %d rows", body.Type, result.Code, len(got))
		}
		if len(got) > 1 {
			t.Fatalf("%s: op-only surface emitted %d rows, want at most 1", body.Type, len(got))
		}
		for _, mv := range got {
			checkFuzzMovement(t, body, result, txSource, mv)
		}
	})
}

func checkFuzzMovement(t *testing.T, body xdr.OperationBody, result xdr.OperationResult, txSource string, mv Movement) {
	t.Helper()
	if !mv.Kind.IsValid() || mv.Provenance != ProvenanceClassicDerived || mv.Ledger != 7 || mv.TxHash != "tx" || mv.OpIndex != 5 || mv.LegIndex != 0 {
		t.Fatalf("%s: bad envelope %+v", body.Type, mv)
	}
	for _, addr := range []string{mv.FromAddress, mv.ToAddress} {
		if addr != "" && !strkey.IsValidEd25519PublicKey(addr) {
			t.Fatalf("%s: counterparty %q is not a G-strkey", body.Type, addr)
		}
	}
	amt := mv.Amount.BigInt()
	if amt.Sign() < 0 || amt.Cmp(maxInt64) > 0 {
		t.Fatalf("%s: amount %s outside [0, MaxInt64]", body.Type, amt)
	}
	tr := result.MustTr()
	var wantKind Kind
	var wantAmount int64
	var wantAsset string
	switch body.Type {
	case xdr.OperationTypeCreateAccount:
		wantKind, wantAmount, wantAsset = KindCreateAccount, int64(body.MustCreateAccountOp().StartingBalance), "native"
	case xdr.OperationTypePayment:
		p := body.MustPaymentOp()
		wantKind, wantAmount, wantAsset = KindPayment, int64(p.Amount), xdrjson.AssetID(p.Asset)
		if want := p.Destination.ToAccountId().Address(); mv.ToAddress != want {
			t.Fatalf("payment to %q, want base account %q", mv.ToAddress, want)
		}
	case xdr.OperationTypePathPaymentStrictReceive:
		last := tr.MustPathPaymentStrictReceiveResult().MustSuccess().Last
		wantKind, wantAmount, wantAsset = KindPathPayment, int64(last.Amount), xdrjson.AssetID(last.Asset)
		if mv.Attributes["send_asset"] != xdrjson.AssetID(body.MustPathPaymentStrictReceiveOp().SendAsset) {
			t.Fatalf("strict receive send_asset=%v", mv.Attributes["send_asset"])
		}
	case xdr.OperationTypePathPaymentStrictSend:
		last := tr.MustPathPaymentStrictSendResult().MustSuccess().Last
		wantKind, wantAmount, wantAsset = KindPathPayment, int64(last.Amount), xdrjson.AssetID(last.Asset)
		sa := int64(body.MustPathPaymentStrictSendOp().SendAmount)
		if mv.Attributes["send_amount"] != big.NewInt(sa).String() {
			t.Fatalf("strict send send_amount=%v, want %d", mv.Attributes["send_amount"], sa)
		}
	case xdr.OperationTypeCreateClaimableBalance:
		c := body.MustCreateClaimableBalanceOp()
		wantKind, wantAmount, wantAsset = KindClaimableBalanceCreate, int64(c.Amount), xdrjson.AssetID(c.Asset)
	case xdr.OperationTypeClawback:
		c := body.MustClawbackOp()
		wantKind, wantAmount, wantAsset = KindClawback, int64(c.Amount), xdrjson.AssetID(c.Asset)
		if want := c.From.ToAccountId().Address(); mv.FromAddress != want || mv.ToAddress != txSource {
			t.Fatalf("clawback %q->%q, want holder %q -> issuer %q", mv.FromAddress, mv.ToAddress, want, txSource)
		}
	case xdr.OperationTypeAccountMerge:
		wantKind, wantAmount, wantAsset = KindAccountMerge, int64(tr.MustAccountMergeResult().MustSourceAccountBalance()), "native"
	default:
		// Claim / clawback of a claimable balance: the fresh decoder's
		// index is empty, so these must park as pending, never emit.
		t.Fatalf("%s emitted a movement from an empty balance index", body.Type)
	}
	if mv.Kind != wantKind || mv.Asset != wantAsset || amt.Cmp(big.NewInt(wantAmount)) != 0 {
		t.Fatalf("%s: got {%s %s %s}, want {%s %s %d}", body.Type, mv.Kind, mv.Asset, amt, wantKind, wantAsset, wantAmount)
	}
	if amt.Sign() == 0 && wantKind != KindCreateAccount && wantKind != KindAccountMerge {
		t.Fatalf("%s: zero-amount %s movement", body.Type, wantKind)
	}
	if wantKind != KindClawback && mv.FromAddress != txSource {
		t.Fatalf("%s: from %q, want tx source %q", body.Type, mv.FromAddress, txSource)
	}
}

// FuzzClaimableBalanceIndex models the in-run BalanceId index as a
// FIFO of distinct ids with a fixed capacity: after any sequence of
// creates (duplicates included), exactly the newest `ring` distinct ids
// resolve, each to the amount its latest create carried.
func FuzzClaimableBalanceIndex(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4}, uint8(3), int64(10))
	f.Add([]byte{0, 1, 0, 2, 0, 3}, uint8(2), int64(-1))
	f.Add([]byte{5, 5, 5}, uint8(1), int64(math.MaxInt64))

	idKey := func(k byte) string { return string([]byte{'a' + k, 'a' + k}) }
	f.Fuzz(func(t *testing.T, ids []byte, capacity uint8, base int64) {
		ring := int(capacity%8) + 1
		d := fuzzDecoder(ring)
		var queue []byte
		latest := map[byte]*big.Int{}
		for i, id := range ids {
			k := id % 16
			amt := bigSum(base, int64(i))
			d.indexClaimableBalanceCreate(Movement{
				Kind:        KindClaimableBalanceCreate,
				Asset:       "native",
				Amount:      canonical.NewAmount(amt),
				FromAddress: "G",
				Attributes:  map[string]any{"balance_id": idKey(k)},
			})
			if !bytes.Contains(queue, []byte{k}) {
				if len(queue) == ring {
					delete(latest, queue[0])
					queue = queue[1:]
				}
				queue = append(queue, k)
			}
			latest[k] = amt
		}
		for k := byte(0); k < 16; k++ {
			_, amount, _, found := d.ResolveBalance(idKey(k))
			want, wantFound := latest[k]
			if found != wantFound {
				t.Fatalf("ids=%v ring=%d: id %d found=%v, want %v", ids, ring, k, found, wantFound)
			}
			if found && amount.BigInt().Cmp(want) != 0 {
				t.Fatalf("ids=%v ring=%d: id %d amount=%s, want %s", ids, ring, k, amount, want)
			}
		}
		if len(d.balances) != len(queue) {
			t.Fatalf("ids=%v ring=%d: index holds %d, want %d", ids, ring, len(d.balances), len(queue))
		}
	})
}
