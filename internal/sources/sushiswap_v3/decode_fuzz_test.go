package sushiswap_v3

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// fuzzRecipient / fuzzSender are distinct G-accounts so a decoder that
// reads one field for the other is caught.
const fuzzRecipient = "GCBYPF2OVPSZ7NJSXIOINCBMOSMY7I6KOWHPZV36OH4OH5R2A63DKUKY"

var fuzzSender = func() string {
	raw := make([]byte, 32)
	raw[0], raw[31] = 0x5e, 0xed
	s, err := strkey.Encode(strkey.VersionByteAccountID, raw)
	if err != nil {
		panic(err)
	}
	return s
}()

// i128Parts splits a decoded amount back into its wire limbs, for
// seeding the corpus from the golden lake bodies.
func i128Parts(a canonical.Amount) (int64, uint64) {
	v := a.BigInt()
	if v.Sign() < 0 {
		v = new(big.Int).Add(v, new(big.Int).Lsh(big.NewInt(1), 128))
	}
	lo := new(big.Int).And(v, new(big.Int).SetUint64(^uint64(0))).Uint64()
	hi := new(big.Int).Rsh(v, 64).Uint64()
	return int64(hi), lo //nolint:gosec // two's-complement limb, reinterpreted on purpose
}

// i128Want is the exact value an Int128Parts wire value carries,
// computed independently of the decoder: (hi << 64) + lo, two's
// complement.
func i128Want(hi int64, lo uint64) *big.Int {
	v := new(big.Int).Lsh(big.NewInt(hi), 64)
	return v.Add(v, new(big.Int).SetUint64(lo))
}

func fuzzSym(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func fuzzI128(hi int64, lo uint64) xdr.ScVal {
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func fuzzB64(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// fuzzSwapBody builds a full seven-field swap body, entries rotated by
// rot so the decoder is exercised against every field position — decode
// is by NAME (docs/architecture/contract-schema-evolution.md).
func fuzzSwapBody(t *testing.T, amount0, amount1 xdr.ScVal, liq xdr.UInt128Parts,
	sqrt xdr.UInt256Parts, tick int32, rot int,
) string {
	t.Helper()
	i32 := xdr.Int32(tick)
	names := []string{"amount0", "amount1", "liquidity", "recipient", "sender", "sqrt_price_x96", "tick"}
	vals := []xdr.ScVal{
		amount0,
		amount1,
		{Type: xdr.ScValTypeScvU128, U128: &liq},
		addressScValFromStrkey(t, fuzzRecipient),
		addressScValFromStrkey(t, fuzzSender),
		{Type: xdr.ScValTypeScvU256, U256: &sqrt},
		{Type: xdr.ScValTypeScvI32, I32: &i32},
	}
	m := make(xdr.ScMap, len(names))
	for i := range names {
		j := (i + rot) % len(names)
		m[i] = xdr.ScMapEntry{Key: fuzzSym(names[j]), Val: vals[j]}
	}
	pm := &m
	return fuzzB64(t, xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm})
}

// FuzzSwapDirectionAndExactAmounts drives the production entry point
// (Decoder.Decode on a registered pool) over the whole i128 × i128
// domain of the two signed pool deltas and asserts the trade it emits
// against a reference computed with *big.Int, independently of the
// decoder:
//
//   - a trade exists iff exactly one delta is strictly positive and the
//     other strictly negative; every other combination (a zero on either
//     side, same signs) is a counted recognized no-op, never a trade at a
//     fabricated price;
//   - base = the token the pool RECEIVED at the exact positive delta,
//     quote = the token it PAID at the exact magnitude of the negative
//     delta — no int64 truncation (ADR-0003), including -2^127 whose
//     magnitude does not fit an i128;
//   - OpIndex fans out by event index, Taker is the output recipient;
//   - the best-effort fields (liquidity u128, sqrt_price_x96 u256, tick,
//     sender) round-trip at full width regardless of field order.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzSwapDirectionAndExactAmounts$ -fuzztime=60s -parallel=2 ./internal/sources/sushiswap_v3/
func FuzzSwapDirectionAndExactAmounts(f *testing.F) {
	type seed struct {
		h0     int64
		l0     uint64
		h1     int64
		l1     uint64
		sqrtHi uint64
		rot    uint8
		op     uint16
		ev     uint16
	}
	// The golden lake swaps (fixtures_test.go): sell token0, sell token1,
	// the pre-upgrade WASM, and the one non-directional swap in history.
	for i, body := range []string{goldenSwapSellToken0, goldenSwapSellToken1, goldenSwapPreUpgrade, goldenSwapNonDirectional} {
		g, err := sdkDecodeSwapFields(body)
		if err != nil {
			f.Fatalf("golden %d: %v", i, err)
		}
		h0, l0 := i128Parts(g.Amount0)
		h1, l1 := i128Parts(g.Amount1)
		f.Add(h0, l0, h1, l1, uint64(i), uint8(i), uint16(0), uint16(2+i))
	}
	for _, s := range []seed{
		{0, 0, 0, 5, 0, 1, 0, 0},                                               // zero token0 leg: no trade
		{0, 5, 0, 0, 0, 1, 1, 0},                                               // zero token1 leg: no trade
		{0, 0, 0, 0, 0, 0, 0, 0},                                               // nothing moved
		{0, 7, 0, 9, 0, 2, 0, 0},                                               // both received: no trade
		{-1, ^uint64(0), -1, ^uint64(0), 0, 5, 0, 0},                           // both paid: no trade
		{0, 1 << 63, -1, 1 << 63, 1 << 40, 4, 3, 14},                           // Lo above 2^63
		{1, 0, -2, 0, 1, 6, 65535, 65535},                                      // 2^64 legs, max indices
		{int64(^uint64(0) >> 1), ^uint64(0), -1 << 63, 0, ^uint64(0), 3, 7, 9}, // max i128 vs min i128
	} {
		f.Add(s.h0, s.l0, s.h1, s.l1, s.sqrtHi, s.rot, s.op, s.ev)
	}

	meta := MainnetPools[mainPool]
	tok0, err := canonical.NewSorobanAsset(meta.Token0)
	if err != nil {
		f.Fatalf("token0: %v", err)
	}
	tok1, err := canonical.NewSorobanAsset(meta.Token1)
	if err != nil {
		f.Fatalf("token1: %v", err)
	}

	f.Fuzz(func(t *testing.T, h0 int64, l0 uint64, h1 int64, l1 uint64, sqrtHi uint64, rot uint8, op, evIdx uint16) {
		want0, want1 := i128Want(h0, l0), i128Want(h1, l1)
		liq := xdr.UInt128Parts{Hi: xdr.Uint64(sqrtHi >> 1), Lo: xdr.Uint64(l0)}
		sqrt := xdr.UInt256Parts{
			HiHi: xdr.Uint64(sqrtHi), HiLo: xdr.Uint64(l1),
			LoHi: xdr.Uint64(uint64(h0)), LoLo: xdr.Uint64(uint64(h1)), //nolint:gosec // bit pattern reuse, not a value conversion
		}
		tick := int32(uint32(l0)) //nolint:gosec // any i32 bit pattern is a valid tick for the round-trip
		body := fuzzSwapBody(t, fuzzI128(h0, l0), fuzzI128(h1, l1), liq, sqrt, tick, int(rot))

		// Best-effort fields: exact full-width round trip.
		fields, err := sdkDecodeSwapFields(body)
		if err != nil {
			t.Fatalf("well-formed body refused: %v", err)
		}
		if fields.Amount0.BigInt().Cmp(want0) != 0 || fields.Amount1.BigInt().Cmp(want1) != 0 {
			t.Fatalf("amounts = (%s, %s), want (%s, %s)", fields.Amount0, fields.Amount1, want0, want1)
		}
		wantLiq := new(big.Int).Lsh(new(big.Int).SetUint64(sqrtHi>>1), 64)
		wantLiq.Add(wantLiq, new(big.Int).SetUint64(l0))
		if fields.Liquidity.BigInt().Cmp(wantLiq) != 0 {
			t.Fatalf("liquidity = %s, want %s", fields.Liquidity, wantLiq)
		}
		wantSqrt := new(big.Int)
		for _, w := range []uint64{sqrtHi, l1, uint64(h0), uint64(h1)} { //nolint:gosec // bit pattern reuse
			wantSqrt.Lsh(wantSqrt, 64).Add(wantSqrt, new(big.Int).SetUint64(w))
		}
		if fields.SqrtPriceX96.BigInt().Cmp(wantSqrt) != 0 {
			t.Fatalf("sqrt_price_x96 = %s, want %s (u256 must keep full width)", fields.SqrtPriceX96, wantSqrt)
		}
		if fields.Tick != tick || fields.Sender != fuzzSender || fields.Recipient != fuzzRecipient {
			t.Fatalf("tick/sender/recipient = %d/%s/%s, want %d/%s/%s",
				fields.Tick, fields.Sender, fields.Recipient, tick, fuzzSender, fuzzRecipient)
		}

		// Production entry point.
		d := NewDecoder()
		ev := swapEvent(mainPool, body, 64_200_014, int(op), int(evIdx))
		if !d.Matches(ev) {
			t.Fatal("swap from a registered pool not matched")
		}
		out, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}

		s0, s1 := want0.Sign(), want1.Sign()
		directional := (s0 > 0 && s1 < 0) || (s1 > 0 && s0 < 0)
		if !directional {
			if len(out) != 0 {
				t.Fatalf("non-directional deltas (%s, %s) produced %d trade(s)", want0, want1, len(out))
			}
			if got := d.SkippedNonDirectional(); got != 1 {
				t.Fatalf("SkippedNonDirectional = %d, want 1", got)
			}
			return
		}
		if len(out) != 1 {
			t.Fatalf("directional deltas (%s, %s) produced %d events, want 1", want0, want1, len(out))
		}
		trade := out[0].(TradeEvent).Trade

		wantBase, wantQuote := tok0, tok1
		wantBaseAmt, wantQuoteAmt := want0, new(big.Int).Neg(want1)
		if s1 > 0 {
			wantBase, wantQuote = tok1, tok0
			wantBaseAmt, wantQuoteAmt = want1, new(big.Int).Neg(want0)
		}
		if trade.Pair.Base.String() != wantBase.String() || trade.Pair.Quote.String() != wantQuote.String() {
			t.Fatalf("pair = %s/%s, want %s/%s", trade.Pair.Base, trade.Pair.Quote, wantBase, wantQuote)
		}
		if trade.BaseAmount.BigInt().Cmp(wantBaseAmt) != 0 {
			t.Fatalf("base amount = %s, want %s", trade.BaseAmount, wantBaseAmt)
		}
		if trade.QuoteAmount.BigInt().Cmp(wantQuoteAmt) != 0 {
			t.Fatalf("quote amount = %s, want %s", trade.QuoteAmount, wantQuoteAmt)
		}
		if trade.BaseAmount.Sign() <= 0 || trade.QuoteAmount.Sign() <= 0 {
			t.Fatalf("trade legs must be positive magnitudes: base=%s quote=%s", trade.BaseAmount, trade.QuoteAmount)
		}
		if want := canonical.FanoutOpIndex(int(op), int(evIdx)); trade.OpIndex != want {
			t.Fatalf("OpIndex = %d, want FanoutOpIndex(%d, %d) = %d", trade.OpIndex, op, evIdx, want)
		}
		if trade.Taker != fuzzRecipient {
			t.Fatalf("Taker = %s, want recipient %s", trade.Taker, fuzzRecipient)
		}
	})
}

// FuzzSwapAmountForeignScValType asserts the price-forming amounts are
// read ONLY from an i128: the same 128 bits wrapped as u128 / i64 / u64 /
// i256 / u256 is a foreign schema and must be refused, never decoded as
// a magnitude (a u128 read of an i128 slot silently flips a negative
// delta into a ~2^128 positive one).
func FuzzSwapAmountForeignScValType(f *testing.F) {
	for typ := uint8(0); typ < 6; typ++ {
		f.Add(typ, int64(0), uint64(1_000_000), false)
		f.Add(typ, int64(-1), uint64(1)<<63, true)
	}
	f.Fuzz(func(t *testing.T, typ uint8, hi int64, lo uint64, onAmount1 bool) {
		var foreign xdr.ScVal
		isI128 := false
		switch typ % 6 {
		case 0:
			foreign, isI128 = fuzzI128(hi, lo), true
		case 1:
			p := xdr.UInt128Parts{Hi: xdr.Uint64(uint64(hi)), Lo: xdr.Uint64(lo)} //nolint:gosec // bit pattern reuse
			foreign = xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &p}
		case 2:
			v := xdr.Int64(hi)
			foreign = xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &v}
		case 3:
			v := xdr.Uint64(lo)
			foreign = xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v}
		case 4:
			p := xdr.Int256Parts{HiHi: xdr.Int64(hi >> 63), HiLo: xdr.Uint64(uint64(hi >> 63)), LoHi: xdr.Uint64(uint64(hi)), LoLo: xdr.Uint64(lo)} //nolint:gosec // bit pattern reuse
			foreign = xdr.ScVal{Type: xdr.ScValTypeScvI256, I256: &p}
		default:
			p := xdr.UInt256Parts{LoHi: xdr.Uint64(uint64(hi)), LoLo: xdr.Uint64(lo)} //nolint:gosec // bit pattern reuse
			foreign = xdr.ScVal{Type: xdr.ScValTypeScvU256, U256: &p}
		}
		good := fuzzI128(0, 42)
		a0, a1 := foreign, good
		if onAmount1 {
			a0, a1 = good, foreign
		}
		body := fuzzSwapBody(t, a0, a1, xdr.UInt128Parts{}, xdr.UInt256Parts{}, 0, int(lo%7))

		got, err := sdkDecodeSwapFields(body)
		if !isI128 {
			if err == nil {
				t.Fatalf("amount of foreign type %d decoded as %s/%s; must be refused", typ%6, got.Amount0, got.Amount1)
			}
			if !errors.Is(err, ErrMalformedPayload) {
				t.Fatalf("foreign amount error %v does not wrap ErrMalformedPayload", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("i128 amount refused: %v", err)
		}
		gotAmt := got.Amount0
		if onAmount1 {
			gotAmt = got.Amount1
		}
		if gotAmt.BigInt().Cmp(i128Want(hi, lo)) != 0 {
			t.Fatalf("amount = %s, want %s", gotAmt, i128Want(hi, lo))
		}
	})
}

// TestDecode_PairErrorOnADirectionalSwapIsReturned pins that emitTrade
// swallows ONLY ErrNonDirectionalSwap. A token map that names the same
// asset twice cannot form a pair; that is a registry defect and must
// surface as a decode error, not be counted as an ordinary dust no-op.
func TestDecode_PairErrorOnADirectionalSwapIsReturned(t *testing.T) {
	raw := make([]byte, 32)
	raw[0] = 0x5e
	pool, err := strkey.Encode(strkey.VersionByteContract, raw)
	if err != nil {
		t.Fatal(err)
	}
	same, err := canonical.NewSorobanAsset(tokenUSDC)
	if err != nil {
		t.Fatal(err)
	}
	d := NewDecoder()
	d.SeedPool(pool, same, same, MainnetFactory, 64_000_000)

	out, err := d.Decode(swapEvent(pool, goldenSwapSellToken0, 64_200_014, 0, 2))
	if err == nil {
		t.Fatalf("Decode = %d events, nil error; want the pair error surfaced", len(out))
	}
	if errors.Is(err, ErrNonDirectionalSwap) {
		t.Fatalf("pair error misreported as non-directional: %v", err)
	}
	if got := d.SkippedNonDirectional(); got != 0 {
		t.Fatalf("SkippedNonDirectional = %d, want 0 — a registry defect is not a dust swap", got)
	}
}
