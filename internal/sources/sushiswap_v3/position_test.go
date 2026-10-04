package sushiswap_v3

import (
	"encoding/base64"
	"errors"
	"math"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

const (
	goldenPositionOwner  = "CARTUL5AWDZYBSN7HUUJZSKCAKCIAKM7M54Z76G6KRYCK4XPR3OHUQZ4"
	goldenPositionSender = "GCFB64LD5OX6XXUQW44LXAE7RAHORF2FJTSQ3DXEAV6DXVOML433X6HF"
)

func positionEvent(contractID, topic, body string) events.Event {
	return events.Event{
		Type:           "contract",
		Ledger:         61_487_383,
		LedgerClosedAt: "2026-03-03T20:52:22Z",
		ContractID:     contractID,
		TxHash:         swapTxHash,
		OperationIndex: 0,
		EventIndex:     3,
		Topic:          []string{topic},
		Value:          body,
	}
}

func decodeOnePosition(t *testing.T, d *Decoder, ev events.Event) PositionEvent {
	t.Helper()
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	pe, ok := out[0].(PositionEvent)
	if !ok {
		t.Fatalf("got %T, want PositionEvent", out[0])
	}
	return pe
}

// TestDecode_MintBecomesAPositionEvent decodes the real lake mint body and
// checks every field, including the token identities that come from the
// pool registry rather than the body.
func TestDecode_MintBecomesAPositionEvent(t *testing.T) {
	pe := decodeOnePosition(t, NewDecoder(), positionEvent(mainPool, TopicSymbolMint, goldenMint))
	if pe.EventKind() != "sushiswap_v3.position" || pe.Source() != SourceName {
		t.Errorf("kind/source = %s/%s", pe.EventKind(), pe.Source())
	}
	if pe.Action != EventMint || pe.ContractID != mainPool || pe.EventIndex != 3 || pe.Ledger != 61_487_383 {
		t.Errorf("identity = %s %s idx=%d ledger=%d", pe.Action, pe.ContractID, pe.EventIndex, pe.Ledger)
	}
	if pe.Liquidity.String() != "496117982" || pe.Amount0.String() != "100000000" || pe.Amount1.String() != "18231333" {
		t.Errorf("amounts = liq %s / %s / %s", pe.Liquidity, pe.Amount0, pe.Amount1)
	}
	if pe.TickLower != -18780 || pe.TickUpper != -15180 {
		t.Errorf("ticks = %d / %d", pe.TickLower, pe.TickUpper)
	}
	if pe.Owner != goldenPositionOwner || pe.Sender != goldenPositionSender || pe.Recipient != "" {
		t.Errorf("owner/sender/recipient = %q/%q/%q", pe.Owner, pe.Sender, pe.Recipient)
	}
	if pe.Token0 != tokenXLM || pe.Token1 != tokenUSDC {
		t.Errorf("tokens = %s / %s", pe.Token0, pe.Token1)
	}
}

func TestDecode_BurnBecomesAPositionEventWithoutSender(t *testing.T) {
	pe := decodeOnePosition(t, NewDecoder(), positionEvent(mainPool, TopicSymbolBurn, goldenBurn))
	if pe.Action != EventBurn || pe.Sender != "" || pe.Owner != goldenPositionOwner {
		t.Errorf("burn = %+v", pe)
	}
	if pe.Liquidity.String() != "992235963" || pe.Amount0.String() != "199999999" || pe.Amount1.String() != "36462663" {
		t.Errorf("amounts = liq %s / %s / %s", pe.Liquidity, pe.Amount0, pe.Amount1)
	}
}

// TestDecode_CollectCarriesNoLiquidity: a collect moves owed tokens, not
// liquidity, so its decode must not claim a liquidity delta.
func TestDecode_CollectCarriesNoLiquidity(t *testing.T) {
	pe := decodeOnePosition(t, NewDecoder(), positionEvent(mainPool, TopicSymbolCollect, goldenBurn))
	if pe.Action != EventCollect || pe.Liquidity.String() != "0" {
		t.Errorf("collect = %+v", pe)
	}
	f, err := sdkDecodePositionFields(EventCollect, goldenBurn)
	if err != nil || f.HasLiquidity {
		t.Fatalf("collect HasLiquidity = %v err = %v, want false", f.HasLiquidity, err)
	}
}

// TestDecode_CollectBodyShape: a collect body has no `amount` and carries a
// `recipient`; built from the burn body's own fields so owner, ticks and
// amounts stay real wire values.
func TestDecode_CollectBodyShape(t *testing.T) {
	sv, err := scval.Parse(goldenBurn)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := scval.AsMap(sv)
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := scval.MapField(entries, "owner")
	m := xdr.ScMap{}
	for _, e := range entries {
		if e.Key.Sym != nil && string(*e.Key.Sym) == "amount" {
			continue
		}
		m = append(m, e)
	}
	rk := xdr.ScSymbol("recipient")
	m = append(m, xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &rk}, Val: owner})
	pm := &m
	body := fuzzB64(t, xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm})

	f, err := sdkDecodePositionFields(EventCollect, body)
	if err != nil {
		t.Fatalf("decode collect: %v", err)
	}
	if f.HasLiquidity || f.Recipient != goldenPositionOwner || f.Owner != goldenPositionOwner {
		t.Errorf("collect = %+v", f)
	}
}

// TestDecode_PositionBodyMissingRequiredFieldIsMalformed: a body without
// the fields a row needs is a visible decode error, never a zero-filled
// row. A swap body carries no owner or ticks.
func TestDecode_PositionBodyMissingRequiredFieldIsMalformed(t *testing.T) {
	for _, kind := range []string{EventMint, EventBurn, EventCollect} {
		out, err := NewDecoder().Decode(positionEvent(mainPool, scval.MustEncodeSymbol(kind), goldenSwapSellToken0))
		if !errors.Is(err, ErrMalformedPayload) || len(out) != 0 {
			t.Errorf("%s: out=%d err=%v, want ErrMalformedPayload and no rows", kind, len(out), err)
		}
	}
}

func symbolScVal(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func u128ScVal(hi, lo uint64) xdr.ScVal {
	p := xdr.UInt128Parts{Hi: xdr.Uint64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &p}
}

func i32ScVal(n int32) xdr.ScVal {
	v := xdr.Int32(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvI32, I32: &v}
}

func mapBody(t *testing.T, keys []string, vals []xdr.ScVal) string {
	t.Helper()
	m := make(xdr.ScMap, len(keys))
	for i, k := range keys {
		m[i] = xdr.ScMapEntry{Key: symbolScVal(k), Val: vals[i]}
	}
	pm := &m
	b, err := (xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}).MarshalBinary()
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// TestDecode_PositionAmountsKeepFull128Bits pins the money invariant: a
// u128 amount above 2^64 must survive decode unchanged (no int64 / float).
func TestDecode_PositionAmountsKeepFull128Bits(t *testing.T) {
	const want = "340282366920938463463374607431768211455" // 2^128 - 1
	widest := u128ScVal(math.MaxUint64, math.MaxUint64)
	body := mapBody(t,
		[]string{"amount", "amount0", "amount1", "owner", "tick_lower", "tick_upper"},
		[]xdr.ScVal{widest, widest, widest, addressScValFromStrkey(t, goldenPositionSender), i32ScVal(-1), i32ScVal(1)})
	f, err := sdkDecodePositionFields(EventBurn, body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Amount0.String() != want || f.Amount1.String() != want || f.Liquidity.String() != want {
		t.Errorf("amounts = %s / %s / %s, want %s", f.Amount0, f.Amount1, f.Liquidity, want)
	}
}

// TestDecode_GatedPoolWithNoTokenMappingFailsClosedOnPosition: same
// fail-closed rule as a swap — counted, no row, never invented assets.
func TestDecode_GatedPoolWithNoTokenMappingFailsClosedOnPosition(t *testing.T) {
	const warmedPool = "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3"
	d := NewDecoder(contractid.WithSeed([]string{warmedPool}))
	out, err := d.Decode(positionEvent(warmedPool, TopicSymbolMint, goldenMint))
	if !errors.Is(err, ErrUnknownPool) || len(out) != 0 {
		t.Fatalf("out=%d err=%v, want ErrUnknownPool and no rows", len(out), err)
	}
	if d.SkippedUnknownPool() != 1 {
		t.Errorf("SkippedUnknownPool = %d, want 1", d.SkippedUnknownPool())
	}
}
