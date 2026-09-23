package phoenix

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Property fuzz targets for the phoenix money path. Every amount here lands
// in a NUMERIC column, so the reference for each is (hi << 64) + lo computed
// with big.Int independently of the decoder (ADR-0003). Seeds run as plain
// tests under `go test`; the generative run is
// `go test -run=^$ -fuzz=^FuzzXxx$ -fuzztime=60s ./internal/sources/phoenix/`.

// i128Ref is the exact value an Int128Parts carries, two's complement.
func i128Ref(hi int64, lo uint64) *big.Int {
	v := new(big.Int).Lsh(big.NewInt(hi), 64)
	return v.Add(v, new(big.Int).SetUint64(lo))
}

func i128Val(hi int64, lo uint64) xdr.ScVal {
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func contractVal(t *testing.T, c string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, c)
	if err != nil {
		t.Fatalf("strkey.Decode: %v", err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
}

// accountVal returns an account ScAddress and its G-strkey.
func accountVal(t *testing.T, seed byte) (xdr.ScVal, string) {
	t.Helper()
	var key xdr.Uint256
	key[0], key[31] = seed, 0xA5
	aid, err := xdr.NewAccountId(xdr.PublicKeyTypePublicKeyTypeEd25519, key)
	if err != nil {
		t.Fatalf("NewAccountId: %v", err)
	}
	g, err := strkey.Encode(strkey.VersionByteAccountID, key[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}, g
}

func symVal(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func strVal(s string) xdr.ScVal {
	str := xdr.ScString(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &str}
}

func amountEq(t *testing.T, what string, got canonical.Amount, want *big.Int) {
	t.Helper()
	if got.BigInt().Cmp(want) != 0 {
		t.Fatalf("%s = %s, want %s (i128 not carried exactly)", what, got, want)
	}
}

// ─── bare i128 bodies ──────────────────────────────────────────────

// FuzzSdkDecodeI128 pins the single amount decoder every phoenix action
// shares: the full signed 128-bit value must survive (no int64 truncation of
// Lo, no dropped Hi limb), and a same-parts U128 / i64 body must be refused
// rather than re-read as an i128.
func FuzzSdkDecodeI128(f *testing.F) {
	for _, s := range [][2]uint64{
		{0, 0},
		{0, 1},
		{0, 55194571},
		{0, 273730773},
		{0, 1 << 63},
		{0, ^uint64(0)},
		{1, 0},
		{1, ^uint64(0)},
		{uint64(1) << 62, 12345},
		{^uint64(0) >> 1, ^uint64(0)},
		{^uint64(0), ^uint64(0)},
		{^uint64(0), 0},
		{1 << 63, 0},
	} {
		f.Add(int64(s[0]), s[1])
	}
	f.Fuzz(func(t *testing.T, hi int64, lo uint64) {
		want := i128Ref(hi, lo)
		got, err := sdkDecodeI128(b64Marshal(t, i128Val(hi, lo)))
		if err != nil {
			t.Fatalf("sdkDecodeI128(%d,%d): %v", hi, lo, err)
		}
		amountEq(t, "i128", got, want)

		u := xdr.UInt128Parts{Hi: xdr.Uint64(uint64(hi)), Lo: xdr.Uint64(lo)}
		if _, err := sdkDecodeI128(b64Marshal(t, xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &u})); err == nil {
			t.Fatal("sdkDecodeI128 accepted a U128 body")
		}
		i := xdr.Int64(hi)
		if _, err := sdkDecodeI128(b64Marshal(t, xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &i})); err == nil {
			t.Fatal("sdkDecodeI128 accepted an I64 body")
		}
	})
}

// ─── single-event Map swap (Q5) ────────────────────────────────────

var mapSwapKeys = [...]string{
	FieldSender, FieldSellToken, FieldOfferAmount, FieldBuyToken, FieldReturnAmount,
	"actual_received_amount", FieldSpreadAmount, FieldReferralFee,
}

const mapSwapRequired = 5 // mapSwapKeys[:5] are the fields decodeSwapMap reads

// FuzzDecodeSwapMap builds the Map body from fuzzed amounts, token seeds,
// dropped fields, entry order and key encoding, and checks decodeSwapMap
// against an independent model: accept iff every read field is present
// under a Symbol key, both amounts are strictly positive and the legs are
// distinct; on accept the amounts are exact, base is sell_token, quote is
// buy_token, and actual_received_amount (== offer on the wire) never leaks
// into the quote.
func FuzzDecodeSwapMap(f *testing.F) {
	// Real CBENABXP fixture amounts, then the boundaries the positivity gate
	// and the i128 carry sit on.
	f.Add(int64(0), uint64(mapSwapOffer), int64(0), uint64(mapSwapReturn), byte(1), byte(2), uint8(0), uint8(0), uint8(0xFF), uint16(0), uint16(3))
	f.Add(int64(0), uint64(0), int64(0), uint64(5), byte(1), byte(2), uint8(0), uint8(3), uint8(0xFF), uint16(1), uint16(0))
	f.Add(int64(0), uint64(5), int64(0), uint64(0), byte(1), byte(2), uint8(0), uint8(1), uint8(0xFF), uint16(0), uint16(9))
	f.Add(int64(-1), ^uint64(0), int64(0), uint64(5), byte(1), byte(2), uint8(0), uint8(2), uint8(0xFF), uint16(0), uint16(0))
	f.Add(int64(0), uint64(5), int64(-1), uint64(0), byte(1), byte(2), uint8(0), uint8(5), uint8(0xFF), uint16(0), uint16(0))
	f.Add(int64(1), ^uint64(0), int64(0), uint64(1)<<63, byte(1), byte(2), uint8(0), uint8(7), uint8(0xFF), uint16(0xFFFF), uint16(0xFFFF))
	f.Add(int64(0), uint64(1), int64(0), uint64(1), byte(7), byte(7), uint8(0), uint8(0), uint8(0xFF), uint16(0), uint16(0))
	f.Add(int64(0), uint64(9), int64(0), uint64(9), byte(1), byte(2), uint8(1<<4), uint8(0), uint8(0xFF), uint16(0), uint16(0))
	f.Add(int64(0), uint64(9), int64(0), uint64(8), byte(1), byte(2), uint8(0xE0), uint8(4), uint8(0xFF), uint16(0), uint16(0))
	f.Add(int64(0), uint64(9), int64(0), uint64(8), byte(1), byte(2), uint8(0), uint8(0), uint8(2), uint16(0), uint16(0))
	f.Add(int64(0), uint64(9), int64(0), uint64(8), byte(1), byte(2), uint8(0), uint8(0), uint8(6), uint16(0), uint16(0))

	f.Fuzz(func(t *testing.T, offerHi int64, offerLo uint64, retHi int64, retLo uint64,
		sellSeed, buySeed byte, drop, rot, strKey uint8, opIdx, evIdx uint16,
	) {
		offer, ret := i128Ref(offerHi, offerLo), i128Ref(retHi, retLo)
		senderVal, sender := accountVal(t, sellSeed^buySeed)
		sell, buy := makeC(t, sellSeed), makeC(t, buySeed)
		vals := [len(mapSwapKeys)]xdr.ScVal{
			senderVal, contractVal(t, sell), i128Val(offerHi, offerLo),
			contractVal(t, buy), i128Val(retHi, retLo),
			i128Val(offerHi, offerLo), // actual_received_amount == offer on the wire
			i128Val(0, 424242), i128Val(0, 0),
		}

		wantOK := offer.Sign() > 0 && ret.Sign() > 0 && sellSeed != buySeed
		var entries xdr.ScMap
		for j := range mapSwapKeys {
			i := (j + int(rot)) % len(mapSwapKeys)
			if drop&(1<<i) != 0 {
				if i < mapSwapRequired {
					wantOK = false
				}
				continue
			}
			key := symVal(mapSwapKeys[i])
			if int(strKey) == i {
				// Same name as an ScvString: a foreign shape MapField must
				// not treat as the field.
				key = strVal(mapSwapKeys[i])
				if i < mapSwapRequired {
					wantOK = false
				}
			}
			entries = append(entries, xdr.ScMapEntry{Key: key, Val: vals[i]})
		}
		m := &entries
		ev := mapSwapEvent()
		ev.Value = b64Marshal(t, xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &m})
		ev.OperationIndex, ev.EventIndex = int(opIdx), int(evIdx)
		closedAt := time.Unix(1_751_534_217, 0).UTC()

		tr, err := decodeSwapMap(&ev, closedAt)
		if !wantOK {
			if err == nil {
				t.Fatalf("decodeSwapMap accepted an invalid swap (offer %s, return %s, sell==buy %v, drop %08b, strKey %d): %+v",
					offer, ret, sellSeed == buySeed, drop, strKey, tr)
			}
			return
		}
		if err != nil {
			t.Fatalf("decodeSwapMap rejected a valid swap: %v", err)
		}
		amountEq(t, "BaseAmount", tr.BaseAmount, offer)
		amountEq(t, "QuoteAmount", tr.QuoteAmount, ret)
		if tr.Pair.Base.ContractID != sell || tr.Pair.Quote.ContractID != buy {
			t.Fatalf("pair = %s/%s, want sell %s / buy %s", tr.Pair.Base, tr.Pair.Quote, sell, buy)
		}
		if tr.Taker != sender {
			t.Fatalf("taker = %q, want %q", tr.Taker, sender)
		}
		if want := uint32(opIdx)<<16 | uint32(evIdx); tr.OpIndex != want {
			t.Fatalf("OpIndex = %d, want %d", tr.OpIndex, want)
		}
		if tr.Ledger != ev.Ledger || tr.TxHash != ev.TxHash || !tr.Timestamp.Equal(closedAt) || tr.Source != SourceName {
			t.Fatalf("identity fields not carried: %+v", tr)
		}
	})
}

// ─── 8-event String swap reassembly ────────────────────────────────

var stringSwapTopics = [SwapFieldCount]string{
	TopicSymbolSender, TopicSymbolSellToken, TopicSymbolOfferAmount, TopicSymbolActualReceived,
	TopicSymbolBuyToken, TopicSymbolReturnAmount, TopicSymbolSpreadAmount, TopicSymbolReferralFee,
}

// permute returns a permutation of [0,n) derived from seed (Lehmer code).
func permute(n int, seed uint64) []int {
	pool := make([]int, n)
	for i := range pool {
		pool[i] = i
	}
	out := make([]int, 0, n)
	for k := n; k > 0; k-- {
		j := int(seed % uint64(k))
		seed /= uint64(k)
		out = append(out, pool[j])
		pool = append(pool[:j], pool[j+1:]...)
	}
	return out
}

// FuzzDecoderStringSwap drives the 8 ScvString field-events of one swap
// through Decoder.Decode in every arrival order. Nothing may emit before the
// 8th field; the 8th emits exactly one trade whose amounts are the exact
// offer / return i128s (never actual-received), fanned out on the FIRST
// arriving field's event index — or, for a non-positive amount, a
// ErrMalformedPayload and no trade.
func FuzzDecoderStringSwap(f *testing.F) {
	f.Add(int64(0), uint64(55194571), int64(0), uint64(273730773), uint64(0), uint16(0))
	f.Add(int64(0), uint64(0), int64(0), uint64(1), uint64(40319), uint16(7))
	f.Add(int64(0), uint64(1), int64(0), uint64(0), uint64(12345), uint16(100))
	f.Add(int64(-5), uint64(0), int64(0), uint64(1), uint64(999), uint16(0))
	f.Add(int64(0), uint64(1)<<63, int64(1), ^uint64(0), uint64(20000), uint16(0xFFF0))
	f.Add(int64(^uint64(0)>>1), ^uint64(0), int64(0), uint64(2), uint64(31), uint16(1))

	f.Fuzz(func(t *testing.T, offerHi int64, offerLo uint64, retHi int64, retLo uint64, order uint64, firstIdx uint16) {
		if firstIdx > 0xFFFF-SwapFieldCount {
			firstIdx = 0xFFFF - SwapFieldCount
		}
		offer, ret := i128Ref(offerHi, offerLo), i128Ref(retHi, retLo)
		sell, buy := makeC(t, 0x31), makeC(t, 0x32)
		senderVal, sender := accountVal(t, 0x33)
		bodies := [SwapFieldCount]string{
			b64Marshal(t, senderVal), b64Marshal(t, contractVal(t, sell)),
			b64Marshal(t, i128Val(offerHi, offerLo)), b64Marshal(t, i128Val(offerHi, offerLo)),
			b64Marshal(t, contractVal(t, buy)), b64Marshal(t, i128Val(retHi, retLo)),
			b64Marshal(t, i128Val(0, 424242)), b64Marshal(t, i128Val(0, 0)),
		}
		d := newTestDecoder()
		wantOK := offer.Sign() > 0 && ret.Sign() > 0
		for n, i := range permute(SwapFieldCount, order) {
			ev := makeFieldEventIdx(t, stringSwapTopics[i], bodies[i], int(firstIdx)+n)
			ev.OperationIndex = 3
			out, err := d.Decode(ev)
			if n < SwapFieldCount-1 {
				if err != nil || len(out) != 0 {
					t.Fatalf("field %d/%d emitted early: out=%v err=%v", n+1, SwapFieldCount, out, err)
				}
				continue
			}
			if !wantOK {
				if !errors.Is(err, ErrMalformedPayload) || len(out) != 0 {
					t.Fatalf("non-positive swap (offer %s, return %s): out=%v err=%v", offer, ret, out, err)
				}
				return
			}
			if err != nil || len(out) != 1 {
				t.Fatalf("8th field: out=%v err=%v, want exactly one trade", out, err)
			}
			tr := out[0].(TradeEvent).Trade
			amountEq(t, "BaseAmount", tr.BaseAmount, offer)
			amountEq(t, "QuoteAmount", tr.QuoteAmount, ret)
			if tr.Pair.Base.ContractID != sell || tr.Pair.Quote.ContractID != buy || tr.Taker != sender {
				t.Fatalf("legs/taker wrong: %+v", tr)
			}
			if want := uint32(3)<<16 | uint32(firstIdx); tr.OpIndex != want {
				t.Fatalf("OpIndex = %d, want %d (first-arriving field's index)", tr.OpIndex, want)
			}
		}
		if d.buf.size() != 0 {
			t.Fatalf("buffer holds %d groups after a complete swap", d.buf.size())
		}
	})
}

// ─── liquidity / stake amounts ─────────────────────────────────────

const (
	fzStakeC = "CDFZSTAKE"
	fzTx     = "fuzzfuzzfuzzfuzzfuzzfuzzfuzzfuzzfuzzfuzzfuzzfuzzfuzzfuzzfuzzfuzz"
)

func feed(t *testing.T, d *Decoder, evs []events.Event, order uint64) consumer.Event {
	t.Helper()
	var got consumer.Event
	for n, i := range permute(len(evs), order) {
		ev := evs[i]
		ev.EventIndex = n
		out, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("Decode field %d: %v", n, err)
		}
		if n < len(evs)-1 && len(out) != 0 {
			t.Fatalf("emitted after %d/%d fields: %v", n+1, len(evs), out)
		}
		if n == len(evs)-1 {
			if len(out) != 1 {
				t.Fatalf("final field emitted %d events, want 1", len(out))
			}
			got = out[0]
		}
	}
	return got
}

// FuzzDecoderLiquidityStakeAmounts checks that provide_liquidity,
// withdraw_liquidity and bond carry each fuzzed i128 into the field it was
// sent on — exactly, and never cross-wired between the A and B legs —
// whatever the arrival order.
func FuzzDecoderLiquidityStakeAmounts(f *testing.F) {
	f.Add(int64(0), uint64(1_000_000), int64(0), uint64(2_000_000), int64(0), uint64(3_000_000), uint64(0))
	f.Add(int64(0), ^uint64(0), int64(1), uint64(0), int64(0), uint64(1)<<63, uint64(119))
	f.Add(int64(^uint64(0)>>1), ^uint64(0), int64(0), uint64(7), int64(2), uint64(9), uint64(5))
	f.Add(int64(0), uint64(42), int64(0), uint64(42), int64(0), uint64(42), uint64(1))

	f.Fuzz(func(t *testing.T, aHi int64, aLo uint64, bHi int64, bLo uint64, cHi int64, cLo uint64, order uint64) {
		a, b, c := i128Ref(aHi, aLo), i128Ref(bHi, bLo), i128Ref(cHi, cLo)
		aB, bB, cB := b64Marshal(t, i128Val(aHi, aLo)), b64Marshal(t, i128Val(bHi, bLo)), b64Marshal(t, i128Val(cHi, cLo))
		userVal, user := accountVal(t, 0x41)
		userB := b64Marshal(t, userVal)
		tokA, tokB := makeC(t, 0x42), makeC(t, 0x43)
		tokAB, tokBB := b64Marshal(t, contractVal(t, tokA)), b64Marshal(t, contractVal(t, tokB))
		d := NewDecoder(contractid.WithSeed([]string{plPool, wlPool, fzStakeC}))

		pl := feed(t, d, []events.Event{
			plField(TopicSymbolPLSender, userB, fzTx), plField(TopicSymbolPLTokenA, tokAB, fzTx),
			plField(TopicSymbolPLTokenAAmt, aB, fzTx), plField(TopicSymbolPLTokenB, tokBB, fzTx),
			plField(TopicSymbolPLTokenBAmt, bB, fzTx),
		}, order).(LiquidityEvent).Change
		amountEq(t, "provide AmountA", pl.AmountA, a)
		amountEq(t, "provide AmountB", pl.AmountB, b)
		if pl.TokenA != tokA || pl.TokenB != tokB || pl.Sender != user {
			t.Fatalf("provide legs wrong: %+v", pl)
		}

		wl := feed(t, d, []events.Event{
			wlField(TopicSymbolWLSender, userB, fzTx), wlField(TopicSymbolWLSharesAmount, cB, fzTx),
			wlField(TopicSymbolWLReturnAmountA, aB, fzTx), wlField(TopicSymbolWLReturnAmountB, bB, fzTx),
		}, order).(LiquidityEvent).Change
		amountEq(t, "withdraw SharesAmount", wl.SharesAmount, c)
		amountEq(t, "withdraw AmountA", wl.AmountA, a)
		amountEq(t, "withdraw AmountB", wl.AmountB, b)

		bondEvs := []events.Event{
			bondField(TopicSymbolStakeUser, userB, fzTx), bondField(TopicSymbolStakeToken, tokAB, fzTx),
			bondField(TopicSymbolStakeAmount, cB, fzTx),
		}
		for i := range bondEvs {
			bondEvs[i].ContractID = fzStakeC
		}
		st := feed(t, d, bondEvs, order).(StakeEvent).Change
		amountEq(t, "bond Amount", st.Amount, c)
		if st.Action != EventActionBond || st.User != user || st.LPToken != tokA {
			t.Fatalf("bond fields wrong: %+v", st)
		}
	})
}

// ─── arbitrary bodies ──────────────────────────────────────────────

// fuzzBodySeeds returns every real on-wire phoenix body under test/fixtures:
// the 8-event swap captures, the factory create announcements and the Map
// swap golden.
func fuzzBodySeeds(f *testing.F) [][]byte {
	f.Helper()
	var out [][]byte
	add := func(b64 string) {
		if b, err := base64.StdEncoding.DecodeString(b64); err == nil {
			out = append(out, b)
		}
	}
	add(mapSwapBodyB64)
	root := filepath.Join("..", "..", "..", "test", "fixtures", "phoenix")
	swaps, _ := filepath.Glob(filepath.Join(root, "*", "swap_*.json"))
	for _, p := range swaps {
		raw, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		var fx phoenixSwapFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			f.Fatal(err)
		}
		for _, e := range fx.Events {
			add(e.Value)
		}
	}
	creates, _ := filepath.Glob(filepath.Join(root, "factory-create", "*.jsonl"))
	for _, p := range creates {
		raw, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var row struct {
				Data string `json:"data_xdr"`
			}
			if json.Unmarshal([]byte(line), &row) == nil {
				add(row.Data)
			}
		}
	}
	if len(out) < 10 {
		f.Fatalf("only %d fixture bodies found; fixture path moved?", len(out))
	}
	return out
}

// symbolField is an independent MapField: first entry with a Symbol key.
func symbolField(m xdr.ScMap, key string) (xdr.ScVal, bool) {
	for _, e := range m {
		if e.Key.Type == xdr.ScValTypeScvSymbol && e.Key.Sym != nil && string(*e.Key.Sym) == key {
			return e.Val, true
		}
	}
	return xdr.ScVal{}, false
}

// FuzzDecodeBodies feeds arbitrary bytes to every body decoder. None may
// panic, and any body a decoder ACCEPTS must be one it had the right to:
// an i128 read equals the wire parts exactly, an announced pool is a real
// C-strkey, and a Map swap has positive amounts that are the Symbol-keyed
// offer_amount / return_amount of the body and two distinct legs.
func FuzzDecodeBodies(f *testing.F) {
	for _, s := range fuzzBodySeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		b64 := base64.StdEncoding.EncodeToString(raw)
		var sv xdr.ScVal
		parsed := sv.UnmarshalBinary(raw) == nil

		if amt, err := sdkDecodeI128(b64); err == nil {
			if !parsed || sv.Type != xdr.ScValTypeScvI128 {
				t.Fatalf("sdkDecodeI128 accepted a non-I128 body (%v)", sv.Type)
			}
			amountEq(t, "i128", amt, i128Ref(int64(sv.I128.Hi), uint64(sv.I128.Lo)))
		}

		ev := mapSwapEvent()
		ev.Value = b64
		if pool, err := decodeAnnouncedPool(&ev); err == nil {
			if _, derr := strkey.Decode(strkey.VersionByteContract, pool); derr != nil {
				t.Fatalf("decodeAnnouncedPool accepted non-contract %q", pool)
			}
		}

		tr, err := decodeSwapMap(&ev, time.Unix(0, 0))
		if err != nil {
			return
		}
		if !parsed || sv.Type != xdr.ScValTypeScvMap || sv.Map == nil || *sv.Map == nil {
			t.Fatalf("decodeSwapMap accepted a non-Map body (%v)", sv.Type)
		}
		m := **sv.Map
		for key, got := range map[string]canonical.Amount{FieldOfferAmount: tr.BaseAmount, FieldReturnAmount: tr.QuoteAmount} {
			v, ok := symbolField(m, key)
			if !ok || v.Type != xdr.ScValTypeScvI128 {
				t.Fatalf("decodeSwapMap accepted a body without an i128 %q", key)
			}
			amountEq(t, key, got, i128Ref(int64(v.I128.Hi), uint64(v.I128.Lo)))
			if got.Sign() <= 0 {
				t.Fatalf("decodeSwapMap accepted non-positive %s = %s", key, got)
			}
		}
		if tr.Pair.Base == tr.Pair.Quote {
			t.Fatalf("decodeSwapMap accepted a self-pair %s", tr.Pair.Base)
		}
	})
}
