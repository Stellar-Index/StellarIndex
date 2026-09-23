package comet

import (
	"encoding/base64"
	"errors"
	"math/big"
	"slices"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

const fuzzTxHash = "78d86d0651d6e432dab500c7c2cb340fd60b391d681124ae83ea49cf7251eac7"

// refI128 is the independent reference for an i128 (hi, lo) pair:
// hi*2^64 + lo, computed without canonical.
func refI128(hi int64, lo uint64) *big.Int {
	r := new(big.Int).Lsh(big.NewInt(hi), 64)
	return r.Add(r, new(big.Int).SetUint64(lo))
}

func i128FromParts(hi int64, lo uint64) xdr.ScVal {
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

type mapField struct {
	key string
	val xdr.ScVal
}

func encodeFields(t *testing.T, fields []mapField) string {
	t.Helper()
	m := make(xdr.ScMap, len(fields))
	for i, f := range fields {
		sym := xdr.ScSymbol(f.key)
		m[i] = xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, Val: f.val}
	}
	pm := &m
	body := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}
	b, err := body.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func fuzzEvent(topic1, value string) events.Event {
	return events.Event{
		ContractID: MainnetBackstopPool, Ledger: 1, TxHash: fuzzTxHash,
		LedgerClosedAt: "2026-05-26T12:00:00Z",
		Topic:          []string{TopicSymbolPool, topic1},
		Value:          value,
	}
}

// FuzzDecodeSwap drives both swap amounts across the full i128 range
// through the production Decode entry point. Non-positive sides and
// self-pairs are recognised zero-row drops; everything else must emit one
// trade whose amounts equal the big.Int reference and whose base is
// token_in.
func FuzzDecodeSwap(f *testing.F) {
	f.Add(int64(0), uint64(1_000_000), int64(0), uint64(990_000), false)
	f.Add(int64(0), uint64(5), int64(0), uint64(5), true)
	f.Add(int64(-1), ^uint64(0), int64(0), uint64(1), false)
	f.Add(int64(0x7fffffffffffffff), ^uint64(0), int64(1), uint64(0), false)
	dec := NewDecoder().WithoutMetrics()
	f.Fuzz(func(t *testing.T, inHi int64, inLo uint64, outHi int64, outLo uint64, selfPair bool) {
		tokenIn, tokenOut := contractStrkeyFromSeed(t, 0x01), contractStrkeyFromSeed(t, 0x02)
		if selfPair {
			tokenOut = tokenIn
		}
		caller := accountStrkeyFromSeed(t, 0x03)
		body := encodeFields(t, []mapField{
			{"caller", addressScValFromStrkey(t, caller)},
			{"token_amount_in", i128FromParts(inHi, inLo)},
			{"token_amount_out", i128FromParts(outHi, outLo)},
			{"token_in", addressScValFromStrkey(t, tokenIn)},
			{"token_out", addressScValFromStrkey(t, tokenOut)},
		})
		in, out := refI128(inHi, inLo), refI128(outHi, outLo)
		got, err := dec.Decode(fuzzEvent(TopicSymbolSwap, body))
		if err != nil {
			t.Fatalf("in=%s out=%s: %v", in, out, err)
		}
		if in.Sign() <= 0 || out.Sign() <= 0 || selfPair {
			if len(got) != 0 {
				t.Fatalf("in=%s out=%s self=%v: emitted %v, want zero rows", in, out, selfPair, got)
			}
			return
		}
		if len(got) != 1 {
			t.Fatalf("emitted %d events, want 1", len(got))
		}
		tr := got[0].(TradeEvent).Trade
		if tr.BaseAmount.BigInt().Cmp(in) != 0 || tr.QuoteAmount.BigInt().Cmp(out) != 0 {
			t.Fatalf("amounts (%s,%s), want (%s,%s)", tr.BaseAmount, tr.QuoteAmount, in, out)
		}
		if tr.Pair.Base.ContractID != tokenIn || tr.Pair.Quote.ContractID != tokenOut || tr.Taker != caller {
			t.Fatalf("pair %s/%s taker %s, want %s/%s %s", tr.Pair.Base, tr.Pair.Quote, tr.Taker, tokenIn, tokenOut, caller)
		}
		if err := tr.Validate(); err != nil {
			t.Fatalf("emitted trade fails Validate: %v", err)
		}
	})
}

var liquidityCases = []struct {
	topic       string
	kind        LiquidityKind
	tokenField  string
	amountField string
}{
	{TopicSymbolJoinPool, LiquidityJoinPool, "token_in", "token_amount_in"},
	{TopicSymbolExitPool, LiquidityExitPool, "token_out", "token_amount_out"},
	{TopicSymbolDeposit, LiquidityDeposit, "token_in", "token_amount_in"},
	{TopicSymbolWithdraw, LiquidityWithdraw, "token_out", "token_amount_out"},
}

// FuzzDecodeLiquidity drives the token amount and (for withdraw) the BPT
// burn across the full i128 range. Emitted rows must satisfy the
// comet_liquidity CHECKs (amount > 0; pool_amount_in > 0 on withdraw,
// absent otherwise) and carry the exact reference values.
func FuzzDecodeLiquidity(f *testing.F) {
	for k := range liquidityCases {
		f.Add(uint8(k), int64(0), uint64(700_000), int64(0), uint64(12_345))
	}
	f.Add(uint8(3), int64(0), uint64(1), int64(0), uint64(0))
	f.Add(uint8(0), int64(-1), uint64(0), int64(0), uint64(0))
	dec := NewDecoder().WithoutMetrics()
	f.Fuzz(func(t *testing.T, kindIdx uint8, aHi int64, aLo uint64, pHi int64, pLo uint64) {
		c := liquidityCases[int(kindIdx)%len(liquidityCases)]
		caller, token := accountStrkeyFromSeed(t, 0x10), contractStrkeyFromSeed(t, 0x11)
		fields := []mapField{
			{"caller", addressScValFromStrkey(t, caller)},
			{c.tokenField, addressScValFromStrkey(t, token)},
			{c.amountField, i128FromParts(aHi, aLo)},
		}
		if c.kind == LiquidityWithdraw {
			fields = append(fields, mapField{"pool_amount_in", i128FromParts(pHi, pLo)})
		}
		amt, pool := refI128(aHi, aLo), refI128(pHi, pLo)
		got, err := dec.Decode(fuzzEvent(c.topic, encodeFields(t, fields)))
		if amt.Sign() <= 0 || (c.kind == LiquidityWithdraw && pool.Sign() <= 0) {
			if !errors.Is(err, ErrNonPositiveAmounts) {
				t.Fatalf("%s amount=%s pool=%s: (%v, %v), want ErrNonPositiveAmounts", c.kind, amt, pool, got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("%s amount=%s pool=%s: %v", c.kind, amt, pool, err)
		}
		le := got[0].(LiquidityEvent)
		if le.Kind != c.kind || le.Caller != caller || le.Token != token {
			t.Fatalf("kind/caller/token = %s/%s/%s", le.Kind, le.Caller, le.Token)
		}
		if le.Amount.BigInt().Cmp(amt) != 0 {
			t.Fatalf("amount=%s, want %s", le.Amount, amt)
		}
		if c.kind == LiquidityWithdraw {
			if le.PoolAmountIn.BigInt().Cmp(pool) != 0 {
				t.Fatalf("pool_amount_in=%s, want %s", le.PoolAmountIn, pool)
			}
		} else if !le.PoolAmountIn.IsZero() {
			t.Fatalf("%s carries pool_amount_in=%s", c.kind, le.PoolAmountIn)
		}
	})
}

// validSymbol reports whether s is a legal SCSymbol (<= 32 chars of
// [A-Za-z0-9_]); anything else is not a field name a contract can emit.
func validSymbol(s string) bool {
	if len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// FuzzSwapBody_fieldNames renames one required swap field. Decode is by
// map field name, so the body must decode iff the name is unchanged, and
// an unrelated extra field must never change what decodes.
func FuzzSwapBody_fieldNames(f *testing.F) {
	f.Add(uint8(0), "caller", "extra")
	f.Add(uint8(3), "token_amount_in", "fee")
	f.Add(uint8(4), "token_out_", "token_in")
	keys := []string{"caller", "token_in", "token_out", "token_amount_in", "token_amount_out"}
	f.Fuzz(func(t *testing.T, slot uint8, rename, extra string) {
		vals := []xdr.ScVal{
			addressScValFromStrkey(t, accountStrkeyFromSeed(t, 0x20)),
			addressScValFromStrkey(t, contractStrkeyFromSeed(t, 0x21)),
			addressScValFromStrkey(t, contractStrkeyFromSeed(t, 0x22)),
			i128FromParts(0, 1_000),
			i128FromParts(0, 900),
		}
		base := make([]mapField, len(keys))
		for i := range keys {
			base[i] = mapField{keys[i], vals[i]}
		}
		want, err := sdkDecodeSwapBody(encodeFields(t, base))
		if err != nil {
			t.Fatalf("baseline: %v", err)
		}

		renamed := slices.Clone(base)
		i := int(slot) % len(keys)
		renamed[i].key = rename
		_, err = sdkDecodeSwapBody(encodeFields(t, renamed))
		if (err == nil) != (rename == keys[i]) {
			t.Fatalf("rename %q->%q: err=%v", keys[i], rename, err)
		}

		if slices.Contains(keys, extra) || !validSymbol(extra) {
			return
		}
		got, err := sdkDecodeSwapBody(encodeFields(t, append(slices.Clone(base), mapField{extra, i128FromParts(-1, 0)})))
		if err != nil {
			t.Fatalf("extra field %q broke decode: %v", extra, err)
		}
		if got.Caller != want.Caller || got.TokenIn != want.TokenIn || got.TokenOut != want.TokenOut ||
			got.AmountIn.BigInt().Cmp(want.AmountIn.BigInt()) != 0 || got.AmountOut.BigInt().Cmp(want.AmountOut.BigInt()) != 0 {
			t.Fatalf("extra field %q changed decode: %+v vs %+v", extra, got, want)
		}
	})
}

// FuzzDecoderDecode_invariants pushes arbitrary body bytes through every
// Comet kind; anything emitted must be a row the served tier accepts.
func FuzzDecoderDecode_invariants(f *testing.F) {
	topics := []string{TopicSymbolSwap, TopicSymbolJoinPool, TopicSymbolExitPool, TopicSymbolDeposit, TopicSymbolWithdraw}
	f.Add(uint8(0), []byte{0, 0, 0, 17, 0, 0, 0, 1, 0, 0, 0, 0})
	f.Add(uint8(4), []byte{})
	dec := NewDecoder().WithoutMetrics()
	f.Fuzz(func(t *testing.T, kindIdx uint8, body []byte) {
		got, err := dec.Decode(fuzzEvent(topics[int(kindIdx)%len(topics)], base64.StdEncoding.EncodeToString(body)))
		if err != nil {
			return
		}
		for _, o := range got {
			switch e := o.(type) {
			case TradeEvent:
				if err := e.Trade.Validate(); err != nil {
					t.Fatalf("emitted trade fails Validate: %v", err)
				}
			case LiquidityEvent:
				if e.Amount.Sign() <= 0 {
					t.Fatalf("amount=%s", e.Amount)
				}
				if (e.Kind == LiquidityWithdraw) != (e.PoolAmountIn.Sign() > 0) {
					t.Fatalf("%s pool_amount_in=%s", e.Kind, e.PoolAmountIn)
				}
			default:
				t.Fatalf("unexpected emitted type %T", o)
			}
		}
	})
}

// FuzzMatches_identityGate: a Comet-shaped event from any contract outside
// the curated pool set must not match.
func FuzzMatches_identityGate(f *testing.F) {
	f.Add([]byte{1, 2, 3}, uint8(0))
	topics := []string{TopicSymbolSwap, TopicSymbolJoinPool, TopicSymbolExitPool, TopicSymbolDeposit, TopicSymbolWithdraw}
	f.Fuzz(func(t *testing.T, seed []byte, kindIdx uint8) {
		var raw [32]byte
		copy(raw[:], seed)
		cid, err := strkey.Encode(strkey.VersionByteContract, raw[:])
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(MainnetGatedSet(), cid) {
			return
		}
		ev := events.Event{ContractID: cid, Topic: []string{TopicSymbolPool, topics[int(kindIdx)%len(topics)]}}
		if NewDecoder().WithoutMetrics().Matches(ev) {
			t.Fatalf("untrusted contract %s matched", cid)
		}
	})
}
