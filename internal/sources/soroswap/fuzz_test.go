package soroswap

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// refI128 is the i128 reference value hi·2^64 + lo computed in big.Int,
// independent of canonical.FromInt128Parts (ADR-0003).
func refI128(hi int64, lo uint64) *big.Int {
	r := big.NewInt(hi)
	r.Lsh(r, 64)
	return r.Add(r, new(big.Int).SetUint64(lo))
}

func rawI128(hi int64, lo uint64) xdr.ScVal {
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func entry(name string, v xdr.ScVal) xdr.ScMapEntry {
	return xdr.ScMapEntry{Key: symbol(name), Val: v}
}

// rotate returns entries rotated by k so decoders are exercised on field
// orders other than the contract's sorted one: decoding is by name.
func rotate(es []xdr.ScMapEntry, k uint8) []xdr.ScMapEntry {
	if len(es) == 0 {
		return es
	}
	n := int(k) % len(es)
	return append(append([]xdr.ScMapEntry{}, es[n:]...), es[:n]...)
}

func accountAddr(seed byte) (xdr.ScVal, string) {
	var ed xdr.Uint256
	for i := range ed {
		ed[i] = seed ^ byte(i)
	}
	acct := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &ed}
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct}
	s, err := strkey.Encode(strkey.VersionByteAccountID, ed[:])
	if err != nil {
		panic(err)
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}, s
}

func contractAddr(seed byte) (xdr.ScVal, string) {
	var cid xdr.ContractId
	for i := range cid {
		cid[i] = seed + byte(i)
	}
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	s, err := strkey.Encode(strkey.VersionByteContract, cid[:])
	if err != nil {
		panic(err)
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}, s
}

func wantAmount(t *testing.T, field string, got canonical.Amount, want *big.Int) {
	t.Helper()
	if got.BigInt().Cmp(want) != 0 {
		t.Fatalf("%s = %s, want %s", field, got, want)
	}
}

// fixtureValues returns the base64 event bodies of the real mainnet
// captures for one event name.
func fixtureValues(f *testing.F, eventName string) []string {
	f.Helper()
	files, _ := filepath.Glob(filepath.Join("..", "..", "..", "test", "fixtures", "soroswap", "*", "*.json"))
	var out []string
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		var fx soroswapFixture
		if err := json.Unmarshal(b, &fx); err != nil {
			f.Fatalf("%s: %v", p, err)
		}
		if fx.EventName == eventName {
			out = append(out, fx.Value)
		}
	}
	return out
}

// seedI128s adds the i128 legs of every real swap body as structured seeds.
func seedI128s(f *testing.F, add func(parts []xdr.Int128Parts)) {
	f.Helper()
	for _, v := range append(fixtureValues(f, EventSwap), ndSwapData) {
		var sv xdr.ScVal
		if err := xdr.SafeUnmarshalBase64(v, &sv); err != nil {
			f.Fatal(err)
		}
		var parts []xdr.Int128Parts
		for _, e := range **sv.Map {
			if e.Val.Type == xdr.ScValTypeScvI128 {
				parts = append(parts, *e.Val.I128)
			}
		}
		add(parts)
	}
}

// FuzzSdkDecodeSwapAmounts: every leg decodes to exactly hi·2^64+lo, by
// field name regardless of map order, and a body missing any one leg is
// refused rather than decoded with a zero in its place.
func FuzzSdkDecodeSwapAmounts(f *testing.F) {
	seedI128s(f, func(p []xdr.Int128Parts) {
		if len(p) == 4 {
			f.Add(int64(p[0].Hi), uint64(p[0].Lo), int64(p[1].Hi), uint64(p[1].Lo),
				int64(p[2].Hi), uint64(p[2].Lo), int64(p[3].Hi), uint64(p[3].Lo), uint8(0), uint8(9))
		}
	})
	f.Add(int64(-1), uint64(0), int64(1<<62), ^uint64(0), int64(0), uint64(1)<<63, int64(-1<<63), uint64(0), uint8(3), uint8(2))
	f.Fuzz(func(t *testing.T, h0 int64, l0 uint64, h1 int64, l1 uint64, h2 int64, l2 uint64, h3 int64, l3 uint64, rot, drop uint8) {
		names := []string{"amount_0_in", "amount_1_in", "amount_0_out", "amount_1_out"}
		his := []int64{h0, h1, h2, h3}
		los := []uint64{l0, l1, l2, l3}
		es := make([]xdr.ScMapEntry, 0, 5)
		for i, n := range names {
			es = append(es, entry(n, rawI128(his[i], los[i])))
		}
		toSv, _ := accountAddr(byte(rot))
		es = append(es, entry("to", toSv))

		got, err := sdkDecodeSwapAmounts(b64(t, scMap(rotate(es, rot)...)))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		for i, amt := range []canonical.Amount{got.Amount0In, got.Amount1In, got.Amount0Out, got.Amount1Out} {
			wantAmount(t, names[i], amt, refI128(his[i], los[i]))
		}

		if int(drop) < len(names) {
			missing := append(append([]xdr.ScMapEntry{}, es[:drop]...), es[drop+1:]...)
			if _, err := sdkDecodeSwapAmounts(b64(t, scMap(missing...))); err == nil {
				t.Fatalf("body without %s decoded", names[drop])
			}
			// Same leg as u128 (a foreign shape) must be refused, not reinterpreted.
			wrong := append([]xdr.ScMapEntry{}, es...)
			u := xdr.UInt128Parts{Hi: xdr.Uint64(uint64(his[drop])), Lo: xdr.Uint64(los[drop])}
			wrong[drop] = entry(names[drop], xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &u})
			if _, err := sdkDecodeSwapAmounts(b64(t, scMap(wrong...))); err == nil {
				t.Fatalf("u128 %s decoded as i128", names[drop])
			}
		}
	})
}

// FuzzDecodeSwap pins direction resolution against an independent model:
// exactly one strictly-positive in-leg on one token and one out-leg on the
// other is a trade (base = in token, amounts carried verbatim); anything
// else is refused, as Ambiguous once three or more legs are positive.
func FuzzDecodeSwap(f *testing.F) {
	seedI128s(f, func(p []xdr.Int128Parts) {
		if len(p) == 4 {
			f.Add(int64(p[0].Hi), uint64(p[0].Lo), int64(p[1].Hi), uint64(p[1].Lo),
				int64(p[2].Hi), uint64(p[2].Lo), int64(p[3].Hi), uint64(p[3].Lo), uint16(3), uint16(7))
		}
	})
	f.Add(int64(0), uint64(5), int64(0), uint64(0), int64(0), uint64(0), int64(0), uint64(9), uint16(0), uint16(0))
	f.Add(int64(0), uint64(0), int64(0), uint64(5), int64(0), uint64(9), int64(0), uint64(0), uint16(1), uint16(1))
	f.Add(int64(0), uint64(1), int64(0), uint64(1), int64(0), uint64(1), int64(0), uint64(1), uint16(2), uint16(2))
	f.Add(int64(0), uint64(1), int64(-1), uint64(0), int64(0), uint64(0), int64(0), uint64(1), uint16(2), uint16(2))
	// A negative out-leg is not a leg: must be refused, not emitted as a negative quote.
	f.Add(int64(0), uint64(5), int64(0), uint64(0), int64(0), uint64(0), int64(-1), ^uint64(4), uint16(0), uint16(0))
	// in0 with both out-legs: three legs, ambiguous, never a trade.
	f.Add(int64(0), uint64(5), int64(0), uint64(0), int64(0), uint64(1), int64(0), uint64(9), uint16(0), uint16(0))
	f.Fuzz(func(t *testing.T, hi0 int64, li0 uint64, hi1 int64, li1 uint64, ho0 int64, lo0 uint64, ho1 int64, lo1 uint64, op, evIdx uint16) {
		in0, in1 := refI128(hi0, li0), refI128(hi1, li1)
		out0, out1 := refI128(ho0, lo0), refI128(ho1, lo1)
		body := scMap(
			entry("amount_0_in", rawI128(hi0, li0)),
			entry("amount_0_out", rawI128(ho0, lo0)),
			entry("amount_1_in", rawI128(hi1, li1)),
			entry("amount_1_out", rawI128(ho1, lo1)),
		)
		_, pair := contractAddr(0x10)
		_, t0 := contractAddr(0x20)
		_, t1 := contractAddr(0x30)
		tok0, err := canonical.NewSorobanAsset(t0)
		if err != nil {
			t.Fatal(err)
		}
		tok1, err := canonical.NewSorobanAsset(t1)
		if err != nil {
			t.Fatal(err)
		}
		closed := time.Date(2026, 4, 16, 20, 11, 40, 0, time.UTC)
		r := RawPair{
			Ledger: 62150023, TxHash: "tx", OpIndex: uint32(op), Pair: pair, ClosedAt: closed,
			Swap: &events.Event{ContractID: pair, Value: b64(t, body), EventIndex: int(evIdx)},
			Sync: &events.Event{ContractID: pair},
		}

		trade, err := decodeSwap(r, tok0, tok1)

		pos := func(x *big.Int) bool { return x.Sign() > 0 }
		legs := 0
		for _, x := range []*big.Int{in0, in1, out0, out1} {
			if pos(x) {
				legs++
			}
		}
		var wantBase, wantQuote canonical.Asset
		var wantBaseAmt, wantQuoteAmt *big.Int
		switch {
		case legs == 2 && pos(in0) && pos(out1):
			wantBase, wantQuote, wantBaseAmt, wantQuoteAmt = tok0, tok1, in0, out1
		case legs == 2 && pos(in1) && pos(out0):
			wantBase, wantQuote, wantBaseAmt, wantQuoteAmt = tok1, tok0, in1, out0
		default:
			if err == nil {
				t.Fatalf("legs in=(%s,%s) out=(%s,%s) emitted trade %+v", in0, in1, out0, out1, trade)
			}
			if !errors.Is(err, ErrNonDirectionalSwap) {
				t.Fatalf("refusal %v is not ErrNonDirectionalSwap", err)
			}
			if got := errors.Is(err, ErrAmbiguousSwapDirection); got != (legs >= 3) {
				t.Fatalf("legs=%d: ambiguous=%v (%v)", legs, got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("legs in=(%s,%s) out=(%s,%s): %v", in0, in1, out0, out1, err)
		}
		if trade.Pair.Base != wantBase || trade.Pair.Quote != wantQuote {
			t.Fatalf("pair = %s/%s, want %s/%s", trade.Pair.Base, trade.Pair.Quote, wantBase, wantQuote)
		}
		wantAmount(t, "BaseAmount", trade.BaseAmount, wantBaseAmt)
		wantAmount(t, "QuoteAmount", trade.QuoteAmount, wantQuoteAmt)
		if want := uint32(op)<<16 | uint32(evIdx); trade.OpIndex != want {
			t.Fatalf("OpIndex = %d, want %d", trade.OpIndex, want)
		}
		if trade.Source != SourceName || trade.Ledger != r.Ledger || trade.TxHash != r.TxHash || !trade.Timestamp.Equal(closed) {
			t.Fatalf("trade identity = %+v", trade)
		}
	})
}

// FuzzSdkDecodeLiquidity: all five amounts round-trip exactly by name and
// the provider strkey round-trips; a missing field is refused.
func FuzzSdkDecodeLiquidity(f *testing.F) {
	f.Add(int64(0), uint64(1), int64(0), uint64(2), int64(0), uint64(3), int64(0), uint64(4), int64(0), uint64(5), uint8(0), true, uint8(9))
	f.Add(int64(-1), ^uint64(0), int64(1<<62), uint64(7), int64(-1<<63), uint64(0), int64(1<<63-1), ^uint64(0), int64(0), uint64(0), uint8(4), false, uint8(2))
	f.Fuzz(func(t *testing.T, h0 int64, l0 uint64, h1 int64, l1 uint64, h2 int64, l2 uint64, h3 int64, l3 uint64, h4 int64, l4 uint64, rot uint8, contractTo bool, drop uint8) {
		names := []string{"amount_0", "amount_1", "liquidity", "new_reserve_0", "new_reserve_1", "to"}
		his := []int64{h0, h1, h2, h3, h4}
		los := []uint64{l0, l1, l2, l3, l4}
		es := make([]xdr.ScMapEntry, 0, 6)
		for i := range his {
			es = append(es, entry(names[i], rawI128(his[i], los[i])))
		}
		toSv, toStr := accountAddr(rot)
		if contractTo {
			toSv, toStr = contractAddr(rot)
		}
		es = append(es, entry("to", toSv))

		got, err := sdkDecodeLiquidity(b64(t, scMap(rotate(es, rot)...)))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		for i, amt := range []canonical.Amount{got.Amount0, got.Amount1, got.Liquidity, got.NewReserve0, got.NewReserve1} {
			wantAmount(t, names[i], amt, refI128(his[i], los[i]))
		}
		if got.To != toStr {
			t.Fatalf("To = %q, want %q", got.To, toStr)
		}
		if int(drop) < len(names) {
			missing := append(append([]xdr.ScMapEntry{}, es[:drop]...), es[drop+1:]...)
			if _, err := sdkDecodeLiquidity(b64(t, scMap(missing...))); err == nil {
				t.Fatalf("body without %s decoded", names[drop])
			}
		}
	})
}

// FuzzSdkDecodeSkim: the skimmed_N name wins over its amount_N alias,
// the alias is used only when the primary is absent, neither present is
// refused, and `to` is optional.
func FuzzSdkDecodeSkim(f *testing.F) {
	f.Add(int64(0), uint64(10), int64(0), uint64(20), int64(0), uint64(30), uint8(1), uint8(1), true, uint8(0))
	f.Add(int64(-1), uint64(1), int64(1<<40), uint64(0), int64(7), uint64(7), uint8(3), uint8(2), false, uint8(2))
	f.Fuzz(func(t *testing.T, hp int64, lp uint64, ha int64, la uint64, h1 int64, l1 uint64, shape0, shape1 uint8, withTo bool, rot uint8) {
		// shape: bit0 = primary present, bit1 = alias present.
		var es []xdr.ScMapEntry
		add := func(shape uint8, primary, alias string, ph int64, pl uint64, ah int64, al uint64) *big.Int {
			var want *big.Int
			if shape&2 != 0 {
				es = append(es, entry(alias, rawI128(ah, al)))
				want = refI128(ah, al)
			}
			if shape&1 != 0 {
				es = append(es, entry(primary, rawI128(ph, pl)))
				want = refI128(ph, pl)
			}
			return want
		}
		want0 := add(shape0, "skimmed_0", "amount_0", hp, lp, ha, la)
		want1 := add(shape1, "skimmed_1", "amount_1", h1, l1, ha, la)
		toSv, toStr := contractAddr(rot)
		if withTo {
			es = append(es, entry("to", toSv))
		} else {
			toStr = ""
		}

		got, err := sdkDecodeSkim(b64(t, scMap(rotate(es, rot)...)))
		if want0 == nil || want1 == nil {
			if err == nil {
				t.Fatalf("shapes (%d,%d) decoded %+v", shape0&3, shape1&3, got)
			}
			return
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		wantAmount(t, "Amount0", got.Amount0, want0)
		wantAmount(t, "Amount1", got.Amount1, want1)
		if got.To != toStr {
			t.Fatalf("To = %q, want %q", got.To, toStr)
		}
	})
}

// FuzzSdkDecodeNewPair: token and pair strkeys round-trip by field name,
// and each token becomes the Soroban asset of exactly that contract.
func FuzzSdkDecodeNewPair(f *testing.F) {
	f.Add(uint8(1), uint8(2), uint8(3), uint8(0), uint8(9))
	f.Fuzz(func(t *testing.T, s0, s1, sp, rot, drop uint8) {
		sv0, str0 := contractAddr(s0)
		sv1, str1 := contractAddr(s1)
		svp, strp := contractAddr(sp)
		names := []string{"token_0", "token_1", "pair"}
		es := []xdr.ScMapEntry{entry("token_0", sv0), entry("token_1", sv1), entry("pair", svp)}

		got, err := sdkDecodeNewPair(b64(t, scMap(rotate(es, rot)...)))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		a0, _ := canonical.NewSorobanAsset(str0)
		a1, _ := canonical.NewSorobanAsset(str1)
		if got.Token0 != a0 || got.Token1 != a1 || got.Pair != strp {
			t.Fatalf("got %+v, want token0=%s token1=%s pair=%s", got, str0, str1, strp)
		}
		if int(drop) < len(names) {
			missing := append(append([]xdr.ScMapEntry{}, es[:drop]...), es[drop+1:]...)
			if _, err := sdkDecodeNewPair(b64(t, scMap(missing...))); err == nil {
				t.Fatalf("body without %s decoded", names[drop])
			}
		}
	})
}

// FuzzDecodersArbitraryBody feeds arbitrary base64 to every body decoder:
// none may panic, and any body a decoder accepts must, read independently
// with the XDR SDK, be a map whose named i128 fields equal what was decoded.
func FuzzDecodersArbitraryBody(f *testing.F) {
	for _, name := range []string{EventSwap, EventSync} {
		for _, v := range fixtureValues(f, name) {
			f.Add(v)
		}
	}
	f.Add(ndSwapData)
	f.Add(ndSyncData)
	f.Add("")
	f.Add("AAAAAQ==")
	f.Fuzz(func(t *testing.T, body string) {
		fieldOf := func(name string) (*big.Int, bool) {
			var sv xdr.ScVal
			if err := xdr.SafeUnmarshalBase64(body, &sv); err != nil || sv.Type != xdr.ScValTypeScvMap || sv.Map == nil || *sv.Map == nil {
				return nil, false
			}
			for _, e := range **sv.Map {
				if e.Key.Type == xdr.ScValTypeScvSymbol && string(*e.Key.Sym) == name {
					if e.Val.Type != xdr.ScValTypeScvI128 {
						return nil, false
					}
					return refI128(int64(e.Val.I128.Hi), uint64(e.Val.I128.Lo)), true
				}
			}
			return nil, false
		}
		check := func(name string, got canonical.Amount) {
			want, ok := fieldOf(name)
			if !ok {
				t.Fatalf("decoder accepted %s absent or non-i128 in %q", name, body)
			}
			wantAmount(t, name, got, want)
		}
		if s, err := sdkDecodeSwapAmounts(body); err == nil {
			check("amount_0_in", s.Amount0In)
			check("amount_1_in", s.Amount1In)
			check("amount_0_out", s.Amount0Out)
			check("amount_1_out", s.Amount1Out)
		}
		if l, err := sdkDecodeLiquidity(body); err == nil {
			check("amount_0", l.Amount0)
			check("liquidity", l.Liquidity)
			check("new_reserve_1", l.NewReserve1)
		}
		_, _ = sdkDecodeSkim(body)
		_, _ = sdkDecodeNewPair(body)
		_ = decodeSwapTaker(body)
	})
}

// FuzzClassify checks classify against the topic table: pair-prefixed
// events by topic[1], new_pair only under the factory prefix, LP-token
// SEP-41 symbols by topic[0], everything else unclassified.
func FuzzClassify(f *testing.F) {
	topics := []string{
		TopicPrefixPair, TopicPrefixFactory, TopicSymbolSwap, TopicSymbolSync, TopicSymbolDeposit,
		TopicSymbolWithdraw, TopicSymbolSkim, TopicSymbolNewPair, TopicSymbolLPTransfer,
		TopicSymbolLPMint, TopicSymbolLPBurn, TopicSymbolLPApprove, "", "AAAADwAAAARzd2Fw ",
	}
	for i := range topics {
		f.Add(uint8(i), uint8((i+1)%len(topics)), "", uint8(2))
	}
	f.Fuzz(func(t *testing.T, i0, i1 uint8, raw string, n uint8) {
		pick := func(i uint8) string {
			if int(i) < len(topics) {
				return topics[i]
			}
			return raw
		}
		topic := []string{pick(i0), pick(i1), raw}[:n%4%3]
		if n%4 == 3 {
			topic = []string{pick(i0), pick(i1), raw}
		}
		want := ""
		if len(topic) >= 2 {
			bySymbol := map[string]string{
				TopicSymbolSwap: EventSwap, TopicSymbolSync: EventSync, TopicSymbolDeposit: EventDeposit,
				TopicSymbolWithdraw: EventWithdraw, TopicSymbolSkim: EventSkim,
			}
			lp := map[string]bool{TopicSymbolLPTransfer: true, TopicSymbolLPMint: true, TopicSymbolLPBurn: true, TopicSymbolLPApprove: true}
			switch {
			case topic[0] == TopicPrefixPair:
				want = bySymbol[topic[1]]
			case topic[0] == TopicPrefixFactory && topic[1] == TopicSymbolNewPair:
				want = EventNewPair
			case lp[topic[0]]:
				want = EventPairToken
			}
		}
		if got := classify(&events.Event{Topic: topic}); got != want {
			t.Fatalf("classify(%q) = %q, want %q", topic, got, want)
		}
	})
}

// FuzzBufferCorrelation drives the swap/sync buffer with an arbitrary
// event sequence over colliding (ledger, tx, op, pool) keys and checks it against
// a model: a completed pair's swap and sync share the full key including
// the emitting pool (COR-08), the swap is the latest one absorbed for that
// key, and nothing completes, vanishes or lingers that the model disagrees on.
func FuzzBufferCorrelation(f *testing.F) {
	f.Add([]byte{0x00, 0x01})
	f.Add([]byte{0x00, 0x04, 0x01, 0x05})
	f.Add([]byte{0x00, 0x04, 0x05, 0x01, 0x02, 0x0b, 0x03})
	f.Add([]byte{0x00, 0x20, 0x21, 0x01})
	f.Fuzz(func(t *testing.T, script []byte) {
		type key struct {
			ledger   uint32
			tx       string
			op       int
			contract string
		}
		type slot struct{ swap, sync *events.Event }
		b := newBuffer()
		b.maxAge = 0
		model := map[key]*slot{}
		closed := time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC)
		for i, c := range script {
			k := key{ledger: 7 + uint32(c>>5&1), tx: []string{"a", "b"}[c>>1&1], op: int(c >> 2 & 1), contract: []string{"CA", "CB", "CC"}[int(c>>3&3)%3]}
			kind := EventSwap
			if c&1 != 0 {
				kind = EventSync
			}
			e := &events.Event{Ledger: k.ledger, TxHash: k.tx, OperationIndex: k.op, ContractID: k.contract, EventIndex: i}
			completed, evicted := b.absorb(e, kind, closed.Add(time.Duration(i)*time.Hour))
			if len(evicted) != 0 {
				t.Fatalf("eviction with maxAge disabled: %+v", evicted)
			}
			s := model[k]
			if s == nil {
				s = &slot{}
				model[k] = s
			}
			if kind == EventSwap {
				s.swap = e
			} else {
				s.sync = e
			}
			if s.swap != nil && s.sync != nil {
				if len(completed) != 1 {
					t.Fatalf("step %d: model completes %+v, buffer returned %d", i, k, len(completed))
				}
				p := completed[0]
				if p.Swap != s.swap || p.Sync != s.sync || p.Pair != k.contract || p.Ledger != k.ledger || p.TxHash != k.tx || int(p.OpIndex) != k.op {
					t.Fatalf("step %d: completed %+v does not match model slot %+v for %+v", i, p, s, k)
				}
				delete(model, k)
			} else if len(completed) != 0 {
				t.Fatalf("step %d: buffer completed %+v, model has only half of %+v", i, completed, k)
			}
			if b.size() != len(model) {
				t.Fatalf("step %d: buffer size %d, model %d", i, b.size(), len(model))
			}
		}
	})
}
