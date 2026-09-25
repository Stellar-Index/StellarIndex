package sep41_transfers

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

func i128Parts(hi int64, lo uint64) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}}
}

// wantI128 is the reference value of (hi << 64) + lo, computed without
// the decoder under test.
func wantI128(hi int64, lo uint64) *big.Int {
	w := new(big.Int).Lsh(big.NewInt(hi), 64)
	return w.Add(w, new(big.Int).SetUint64(lo))
}

// transferMap is the CAP-67 transfer body { amount, to_muxed_id }.
// amountFirst flips field order: decode is by field NAME.
func transferMap(amount xdr.ScVal, amountFirst bool) xdr.ScVal {
	amtKey, muxKey := xdr.ScSymbol("amount"), xdr.ScSymbol("to_muxed_id")
	mux := xdr.Uint64(42)
	amt := xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &amtKey}, Val: amount}
	muxE := xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &muxKey}, Val: xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &mux}}
	m := xdr.ScMap{muxE, amt}
	if amountFirst {
		m = xdr.ScMap{amt, muxE}
	}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

func decodeOne(t *testing.T, d *Decoder, ev events.Event) Event {
	t.Helper()
	outs, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("Decode emitted %d events, want 1", len(outs))
	}
	out, ok := outs[0].(Event)
	if !ok {
		t.Fatalf("Decode emitted %T, want Event", outs[0])
	}
	return out
}

// FuzzTransferAmount: the SEP-41 transfer body is EITHER a bare i128 OR the
// CAP-67 map { amount, to_muxed_id }. Both shapes must decode to the same,
// exact i128 (ADR-0003: never the low limb) over the whole domain, with
// from/to kept in topic order. The decoder passes the sign through
// verbatim; the storage writer is the negative-amount gate.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzTransferAmount$ -fuzztime=60s -parallel=2 ./internal/sources/sep41_transfers
func FuzzTransferAmount(f *testing.F) {
	f.Add(int64(0), uint64(1_000_000))
	f.Add(int64(0), uint64(2_000_000))
	f.Add(int64(0), uint64(0))
	f.Add(int64(0), uint64(1)<<63)
	f.Add(int64(0), ^uint64(0))
	f.Add(int64(1), uint64(0))
	f.Add(int64(^uint64(0)>>1), ^uint64(0))
	f.Add(int64(-1), uint64(0))
	f.Add(int64(-1)<<63, uint64(0)) // min i128

	d, err := NewDecoder([]string{cWatched})
	if err != nil {
		f.Fatalf("NewDecoder: %v", err)
	}
	f.Fuzz(func(t *testing.T, hi int64, lo uint64) {
		want := wantI128(hi, lo)
		amount := i128Parts(hi, lo)
		for name, body := range map[string]xdr.ScVal{
			"bare i128":             amount,
			"map amount-first":      transferMap(amount, true),
			"map to_muxed_id-first": transferMap(amount, false),
		} {
			ev := transferEvent(t, cWatched, 0)
			ev.Value = encScVal(t, body)
			out := decodeOne(t, d, ev)
			if out.Kind != SymbolTransfer {
				t.Fatalf("%s: Kind = %q, want transfer", name, out.Kind)
			}
			if out.Amount == nil || out.Amount.Cmp(want) != 0 {
				t.Fatalf("%s: Amount = %v, want %s (hi=%d lo=%d)", name, out.Amount, want, hi, lo)
			}
			if out.FromAddr != gFrom || out.ToAddr != gTo {
				t.Fatalf("%s: from/to = %q/%q, want %q/%q", name, out.FromAddr, out.ToAddr, gFrom, gTo)
			}
		}
	})
}

// FuzzApproveAmount: approve's Vec[i128, u32] body must carry the exact
// allowance and live-until ledger, spender in ToAddr.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzApproveAmount$ -fuzztime=60s -parallel=2 ./internal/sources/sep41_transfers
func FuzzApproveAmount(f *testing.F) {
	f.Add(int64(0), uint64(500), uint32(999_999))
	f.Add(int64(0), uint64(1)<<63, uint32(0))
	f.Add(int64(0), ^uint64(0), ^uint32(0))
	f.Add(int64(1), uint64(0), uint32(1))
	f.Add(int64(^uint64(0)>>1), ^uint64(0), uint32(123))

	d, err := NewDecoder([]string{cWatched})
	if err != nil {
		f.Fatalf("NewDecoder: %v", err)
	}
	f.Fuzz(func(t *testing.T, hi int64, lo uint64, liveUntil uint32) {
		ev := approveEvent(t, cWatched, 0, 0)
		ev.Value = encScVal(t, vec(i128Parts(hi, lo), u32(liveUntil)))
		out := decodeOne(t, d, ev)
		if want := wantI128(hi, lo); out.Amount == nil || out.Amount.Cmp(want) != 0 {
			t.Fatalf("Amount = %v, want %s (hi=%d lo=%d)", out.Amount, want, hi, lo)
		}
		if out.LiveUntilLedger != liveUntil {
			t.Fatalf("LiveUntilLedger = %d, want %d", out.LiveUntilLedger, liveUntil)
		}
		if out.Kind != SymbolApprove || out.FromAddr != gFrom || out.ToAddr != gSpender {
			t.Fatalf("kind/from/spender = %q/%q/%q, want approve/%q/%q", out.Kind, out.FromAddr, out.ToAddr, gFrom, gSpender)
		}
	})
}

// FuzzDecodeArbitraryValue feeds arbitrary bytes as the event body for
// each of the four kinds. The decoder must never panic, and anything it
// accepts must be one of the documented shapes with the value read from
// the right place. Foreign ScVal shapes are refused with ErrBadValue.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzDecodeArbitraryValue$ -fuzztime=60s -parallel=2 ./internal/sources/sep41_transfers
func FuzzDecodeArbitraryValue(f *testing.F) {
	seed := func(sv xdr.ScVal) []byte {
		b, err := sv.MarshalBinary()
		if err != nil {
			f.Fatalf("marshal seed: %v", err)
		}
		return b
	}
	u64v := xdr.Uint64(7)
	u64 := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u64v}
	for k := uint8(0); k < 4; k++ {
		f.Add(k, seed(i128(5)))
		f.Add(k, seed(transferMap(i128Parts(1, 2), true)))
		f.Add(k, seed(transferMap(u64, true)))
		f.Add(k, seed(vec(i128(5), u32(9))))
		f.Add(k, seed(vec(i128(5), u32(9), u32(9))))
		f.Add(k, seed(boolVal(true)))
		f.Add(k, seed(u64))
		f.Add(k, []byte{})
		f.Add(k, []byte{0, 0, 0, 10})
	}

	d, err := NewDecoder([]string{cWatched})
	if err != nil {
		f.Fatalf("NewDecoder: %v", err)
	}
	f.Fuzz(func(t *testing.T, kind uint8, body []byte) {
		var ev events.Event
		switch kind % 4 {
		case 0:
			ev = transferEvent(t, cWatched, 0)
		case 1:
			ev = approveEvent(t, cWatched, 0, 0)
		case 2:
			ev = setAdminEvent(t, cWatched, true)
		default:
			ev = setAuthorizedEvent(t, cWatched, false)
		}
		ev.Value = base64.StdEncoding.EncodeToString(body)

		// Parseability is scval.Parse's contract (shared by every decoder),
		// not this package's; what is under test is the shape handling.
		sv, perr := scval.Parse(ev.Value)
		parsed := perr == nil
		outs, derr := d.Decode(ev)
		if !parsed {
			if derr == nil {
				t.Fatalf("kind %d: unparseable body %x accepted: %+v", kind%4, body, outs)
			}
			return
		}
		want, wantOK := referenceDecode(kind%4, sv)
		if !wantOK {
			if derr == nil {
				t.Fatalf("kind %d: foreign shape %s accepted: %+v", kind%4, sv.Type, outs)
			}
			if !errors.Is(derr, ErrBadValue) {
				t.Fatalf("kind %d: foreign shape %s refused with %v, want ErrBadValue", kind%4, sv.Type, derr)
			}
			return
		}
		if derr != nil {
			// A documented shape can still fail on its payload (e.g. an
			// Address of an unencodable variant) — refusal is safe.
			return
		}
		out := outs[0].(Event)
		switch kind % 4 {
		case 0, 1:
			if out.Amount == nil || out.Amount.Cmp(want.amount) != 0 {
				t.Fatalf("kind %d: Amount = %v, want %s", kind%4, out.Amount, want.amount)
			}
			if kind%4 == 1 && out.LiveUntilLedger != want.liveUntil {
				t.Fatalf("LiveUntilLedger = %d, want %d", out.LiveUntilLedger, want.liveUntil)
			}
		case 3:
			if out.Authorized == nil || *out.Authorized != want.authorized {
				t.Fatalf("Authorized = %v, want %v", out.Authorized, want.authorized)
			}
		}
	})
}

type refValue struct {
	amount     *big.Int
	liveUntil  uint32
	authorized bool
}

// referenceDecode states, independently of decode.go, which body shapes
// each kind accepts and what they carry.
func referenceDecode(kind uint8, sv xdr.ScVal) (refValue, bool) {
	fromI128 := func(v xdr.ScVal) (*big.Int, bool) {
		if v.Type != xdr.ScValTypeScvI128 || v.I128 == nil {
			return nil, false
		}
		return wantI128(int64(v.I128.Hi), uint64(v.I128.Lo)), true
	}
	switch kind {
	case 0:
		if a, ok := fromI128(sv); ok {
			return refValue{amount: a}, true
		}
		if sv.Type != xdr.ScValTypeScvMap || sv.Map == nil || *sv.Map == nil {
			return refValue{}, false
		}
		for _, e := range **sv.Map {
			if e.Key.Type == xdr.ScValTypeScvSymbol && e.Key.Sym != nil && string(*e.Key.Sym) == "amount" {
				a, ok := fromI128(e.Val)
				return refValue{amount: a}, ok
			}
		}
		return refValue{}, false
	case 1:
		if sv.Type != xdr.ScValTypeScvVec || sv.Vec == nil || *sv.Vec == nil || len(**sv.Vec) != 2 {
			return refValue{}, false
		}
		v := **sv.Vec
		a, ok := fromI128(v[0])
		if !ok || v[1].Type != xdr.ScValTypeScvU32 || v[1].U32 == nil {
			return refValue{}, false
		}
		return refValue{amount: a, liveUntil: uint32(*v[1].U32)}, true
	case 2:
		return refValue{}, sv.Type == xdr.ScValTypeScvAddress
	default:
		if sv.Type != xdr.ScValTypeScvBool || sv.B == nil {
			return refValue{}, false
		}
		return refValue{authorized: *sv.B}, true
	}
}

// TestDecoder_RejectsForeignBodyShapes pins the refusal (with ErrBadValue)
// of body shapes that are close to, but not, the documented ones — each is
// a way a non-conforming contract could otherwise be mis-decoded.
func TestDecoder_RejectsForeignBodyShapes(t *testing.T) {
	d, _ := NewDecoder([]string{cWatched})
	u64v := xdr.Uint64(5)
	u64 := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u64v}
	u128 := xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &xdr.UInt128Parts{Lo: 5}}
	cases := []struct {
		name string
		ev   events.Event
		body xdr.ScVal
	}{
		{"transfer bare u128", transferEvent(t, cWatched, 0), u128},
		{"transfer map amount u64", transferEvent(t, cWatched, 0), transferMap(u64, true)},
		{"transfer map amount u128", transferEvent(t, cWatched, 0), transferMap(u128, false)},
		{"approve 3-arity vec", approveEvent(t, cWatched, 0, 0), vec(i128(5), u32(9), u32(9))},
		{"approve 1-arity vec", approveEvent(t, cWatched, 0, 0), vec(i128(5))},
		{"approve amount u64", approveEvent(t, cWatched, 0, 0), vec(u64, u32(9))},
		{"approve live_until u64", approveEvent(t, cWatched, 0, 0), vec(i128(5), u64)},
		{"approve bare i128", approveEvent(t, cWatched, 0, 0), i128(5)},
		{"set_admin symbol body", setAdminEvent(t, cWatched, true), sym("admin")},
		{"set_authorized u32 body", setAuthorizedEvent(t, cWatched, true), u32(1)},
	}
	for _, tc := range cases {
		tc.ev.Value = encScVal(t, tc.body)
		outs, err := d.Decode(tc.ev)
		if !errors.Is(err, ErrBadValue) {
			t.Errorf("%s: Decode = (%v, %v), want ErrBadValue", tc.name, outs, err)
		}
	}
}

// TestDecoder_VoidAddressTopicRejectedOutsideSetAdmin: a Void in a required
// address slot must fail the event, never decode to "" — an empty from_addr
// on a transfer is read back as a "self" movement by the explorer.
func TestDecoder_VoidAddressTopicRejectedOutsideSetAdmin(t *testing.T) {
	d, _ := NewDecoder([]string{cWatched})
	void := encScVal(t, xdr.ScVal{Type: xdr.ScValTypeScvVoid})
	cases := []struct {
		name string
		ev   events.Event
		idx  int
	}{
		{"transfer.from", transferEvent(t, cWatched, 3), 1},
		{"transfer.to", transferEvent(t, cWatched, 3), 2},
		{"approve.from", approveEvent(t, cWatched, 3, 9), 1},
		{"approve.spender", approveEvent(t, cWatched, 3, 9), 2},
		{"set_authorized.id", setAuthorizedEvent(t, cWatched, true), 1},
	}
	for _, tc := range cases {
		tc.ev.Topic[tc.idx] = void
		outs, err := d.Decode(tc.ev)
		if !errors.Is(err, scval.ErrScValType) {
			t.Errorf("%s Void: Decode = (%v, %v), want ErrScValType", tc.name, outs, err)
		}
	}
}

// TestDecoder_SetAdminVoidAdminTopicDecodesEmpty: set_admin's topic[1] is
// optional, so it is the one address slot where Void decodes to "".
func TestDecoder_SetAdminVoidAdminTopicDecodesEmpty(t *testing.T) {
	d, _ := NewDecoder([]string{cWatched})
	ev := setAdminEvent(t, cWatched, true)
	ev.Topic[1] = encScVal(t, xdr.ScVal{Type: xdr.ScValTypeScvVoid})
	out := decodeOne(t, d, ev)
	if out.FromAddr != "" || out.ToAddr != gTo {
		t.Fatalf("got from=%q to=%q, want \"\"/%q", out.FromAddr, out.ToAddr, gTo)
	}
}

// TestDecoder_MatchesOnlyContractEvents: a system/diagnostic event carrying
// a transfer topic from a watched contract id must not match.
func TestDecoder_MatchesOnlyContractEvents(t *testing.T) {
	d, _ := NewDecoder([]string{cWatched})
	for _, typ := range []string{"system", "diagnostic", ""} {
		ev := transferEvent(t, cWatched, 1)
		ev.Type = typ
		if d.Matches(ev) {
			t.Errorf("Matches(type=%q) = true, want false", typ)
		}
	}
}

// TestUngatedDecoder_NeverMatchesButDecodes: the lake-derive decoder must
// be unusable as a gated dispatcher decoder (Matches always false) while
// still decoding any contract's transfer exactly.
func TestUngatedDecoder_NeverMatchesButDecodes(t *testing.T) {
	d := NewUngatedDecoder()
	ev := transferEvent(t, cUnwatched, 0)
	ev.Value = encScVal(t, i128Parts(1, 7))
	if d.Matches(ev) {
		t.Fatalf("ungated decoder Matches = true, want false")
	}
	out := decodeOne(t, d, ev)
	if want := wantI128(1, 7); out.Amount.Cmp(want) != 0 || out.ContractID != cUnwatched {
		t.Fatalf("got amount=%s contract=%s, want %s/%s", out.Amount, out.ContractID, want, cUnwatched)
	}
}
