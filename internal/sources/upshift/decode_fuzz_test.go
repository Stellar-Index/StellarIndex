package upshift

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// i128Want is the exact value an Int128Parts wire value carries,
// computed independently of the decoder: (hi << 64) + lo, two's
// complement.
func i128Want(hi int64, lo uint64) *big.Int {
	v := new(big.Int).Lsh(big.NewInt(hi), 64)
	return v.Add(v, new(big.Int).SetUint64(lo))
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

// fuzzMap builds a Map body from (name, value) pairs in the order given.
func fuzzMap(names []string, vals []xdr.ScVal) xdr.ScVal {
	m := make(xdr.ScMap, len(names))
	for i := range names {
		sym := xdr.ScSymbol(names[i])
		m[i] = xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, Val: vals[i]}
	}
	pm := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}
}

// fuzzAccount returns a distinct G-strkey per seed and its topic encoding.
func fuzzAccount(t *testing.T, seed byte) (string, string) {
	t.Helper()
	var pub xdr.Uint256
	pub[0], pub[31] = seed, ^seed
	s, err := strkey.Encode(strkey.VersionByteAccountID, pub[:])
	if err != nil {
		t.Fatal(err)
	}
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	return s, fuzzB64(t, xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr})
}

func fuzzEvent(topics []string, body string, op, ev uint16) events.Event {
	return events.Event{
		Type:           "contract",
		ContractID:     MainnetGatedSet()[0],
		Ledger:         62938336,
		LedgerClosedAt: "2026-06-08T14:40:29Z",
		TxHash:         "e86b9a910f5e61db9b98fb60086dd495e6b2f2846cb806b2801a6f6d2d6c16f3",
		OperationIndex: int(op),
		EventIndex:     int(ev),
		Topic:          topics,
		Value:          body,
	}
}

func decodeOneFuzz(t *testing.T, d *Decoder, ev events.Event) Event {
	t.Helper()
	if !d.Matches(ev) {
		t.Fatal("event from a gated vault not matched")
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode returned %d events, want 1", len(out))
	}
	return out[0].(Event)
}

// FuzzTransferBodyShapes is the SEP-41 two-shape rule (AGENTS.md: "ALWAYS
// type-test a SEP-41 transfer body before MustI128()") over the whole
// i128 domain: a bare i128 and a CAP-67 Map{amount, to_muxed_id} —
// with the Map's fields in either order — must decode to the SAME exact
// share amount, and the same 128 bits wrapped as u128 (bare, or as the
// Map's amount) must be refused rather than read as a magnitude.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzTransferBodyShapes$ -fuzztime=60s -parallel=2 ./internal/sources/upshift/
func FuzzTransferBodyShapes(f *testing.F) {
	f.Add(int64(0), uint64(125_000_000_000), false, uint16(0), uint16(1))
	f.Add(int64(0), uint64(0), true, uint16(0), uint16(0))
	f.Add(int64(0), uint64(1)<<63, false, uint16(2), uint16(5))
	f.Add(int64(1), ^uint64(0), true, uint16(1), uint16(0))
	f.Add(int64(^uint64(0)>>1), ^uint64(0), false, uint16(7), uint16(3)) // max i128
	f.Add(int64(-1)<<63, uint64(0), true, uint16(0), uint16(9))          // min i128
	f.Add(int64(-1), ^uint64(0), false, uint16(3), uint16(3))            // -1

	f.Fuzz(func(t *testing.T, hi int64, lo uint64, muxFirst bool, op, evIdx uint16) {
		want := i128Want(hi, lo)
		from, fromTopic := fuzzAccount(t, 0x11)
		to, toTopic := fuzzAccount(t, 0x22)
		topics := []string{TopicSymbolTransfer, fromTopic, toTopic}
		void := xdr.ScVal{Type: xdr.ScValTypeScvVoid}

		names, vals := []string{"amount", "to_muxed_id"}, []xdr.ScVal{fuzzI128(hi, lo), void}
		if muxFirst {
			names, vals = []string{"to_muxed_id", "amount"}, []xdr.ScVal{void, fuzzI128(hi, lo)}
		}
		d := NewDecoder()
		for _, shape := range []struct {
			name string
			body xdr.ScVal
		}{
			{"bare i128", fuzzI128(hi, lo)},
			{"CAP-67 map", fuzzMap(names, vals)},
		} {
			got := decodeOneFuzz(t, d, fuzzEvent(topics, fuzzB64(t, shape.body), op, evIdx))
			if got.Shares.BigInt().Cmp(want) != 0 {
				t.Fatalf("%s: shares = %s, want %s", shape.name, got.Shares, want)
			}
			if got.Caller != from || got.Receiver != to {
				t.Fatalf("%s: from/to = %s/%s, want %s/%s", shape.name, got.Caller, got.Receiver, from, to)
			}
			if got.OpIndex != uint32(op) || got.EventIndex != uint32(evIdx) {
				t.Fatalf("%s: op/event index = %d/%d, want %d/%d", shape.name, got.OpIndex, got.EventIndex, op, evIdx)
			}
		}

		u := xdr.UInt128Parts{Hi: xdr.Uint64(uint64(hi)), Lo: xdr.Uint64(lo)} //nolint:gosec // bit pattern reuse
		u128 := xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &u}
		for _, foreign := range []xdr.ScVal{u128, fuzzMap([]string{"amount", "to_muxed_id"}, []xdr.ScVal{u128, void})} {
			out, err := d.Decode(fuzzEvent(topics, fuzzB64(t, foreign), op, evIdx))
			if err == nil {
				t.Fatalf("u128-typed transfer amount decoded (%v); must be refused", out)
			}
			if !errors.Is(err, ErrMalformedPayload) {
				t.Fatalf("foreign transfer error %v does not wrap ErrMalformedPayload", err)
			}
		}
	})
}

// FuzzFlowAndDeployedAmountsByName asserts deposit/withdraw {assets,
// shares} and deployed_assets_changed {old_amount, new_amount} decode by
// field NAME at full i128 width: whatever order the Map carries them in
// (deployed_assets_changed puts new_amount FIRST on the wire), each
// output column is the exact value of its own named field.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzFlowAndDeployedAmountsByName$ -fuzztime=60s -parallel=2 ./internal/sources/upshift/
func FuzzFlowAndDeployedAmountsByName(f *testing.F) {
	// Real earnUSDC deposit (golden_realbytes_test.go): assets=2,000,000,
	// shares=2,000,000,000,000 (0x1d1a94a2000).
	f.Add(int64(0), uint64(2_000_000), int64(0), uint64(0x1d1a94a2000), false, false, uint16(0), uint16(1))
	f.Add(int64(0), uint64(1)<<63, int64(1), uint64(0), true, true, uint16(2), uint16(9))
	f.Add(int64(-1), uint64(5), int64(^uint64(0)>>1), ^uint64(0), false, true, uint16(0), uint16(0))
	f.Add(int64(7), uint64(7), int64(7), uint64(7), true, false, uint16(4), uint16(4))

	f.Fuzz(func(t *testing.T, ah int64, al uint64, bh int64, bl uint64, reverse, withdraw bool, op, evIdx uint16) {
		a, b := i128Want(ah, al), i128Want(bh, bl)
		caller, cTopic := fuzzAccount(t, 0x31)
		receiver, rTopic := fuzzAccount(t, 0x32)
		owner, oTopic := fuzzAccount(t, 0x33)
		order := func(n1, n2 string) ([]string, []xdr.ScVal) {
			if reverse {
				return []string{n2, n1}, []xdr.ScVal{fuzzI128(bh, bl), fuzzI128(ah, al)}
			}
			return []string{n1, n2}, []xdr.ScVal{fuzzI128(ah, al), fuzzI128(bh, bl)}
		}
		d := NewDecoder()

		sym, kind := TopicSymbolDeposit, EventDeposit
		if withdraw {
			sym, kind = TopicSymbolWithdraw, EventWithdraw
		}
		names, vals := order("assets", "shares")
		flow := decodeOneFuzz(t, d, fuzzEvent([]string{sym, cTopic, rTopic, oTopic}, fuzzB64(t, fuzzMap(names, vals)), op, evIdx))
		if flow.Kind != kind {
			t.Fatalf("kind = %s, want %s", flow.Kind, kind)
		}
		if flow.Assets.BigInt().Cmp(a) != 0 || flow.Shares.BigInt().Cmp(b) != 0 {
			t.Fatalf("assets/shares = %s/%s, want %s/%s", flow.Assets, flow.Shares, a, b)
		}
		if flow.Caller != caller || flow.Receiver != receiver || flow.Owner != owner {
			t.Fatalf("caller/receiver/owner mis-slotted: %s/%s/%s", flow.Caller, flow.Receiver, flow.Owner)
		}
		if flow.OpIndex != uint32(op) || flow.EventIndex != uint32(evIdx) {
			t.Fatalf("op/event index = %d/%d, want %d/%d", flow.OpIndex, flow.EventIndex, op, evIdx)
		}

		names, vals = order("old_amount", "new_amount")
		dep := decodeOneFuzz(t, d, fuzzEvent([]string{TopicSymbolDeployedAssetsChanged, cTopic}, fuzzB64(t, fuzzMap(names, vals)), op, evIdx))
		if dep.OldAmount.BigInt().Cmp(a) != 0 || dep.NewAmount.BigInt().Cmp(b) != 0 {
			t.Fatalf("old/new = %s/%s, want %s/%s", dep.OldAmount, dep.NewAmount, a, b)
		}
		if dep.Caller != caller {
			t.Fatalf("operator = %s, want %s", dep.Caller, caller)
		}

		// A flow body missing its named field is refused, never zero-filled.
		short := fuzzMap([]string{"assets"}, []xdr.ScVal{fuzzI128(ah, al)})
		if _, err := d.Decode(fuzzEvent([]string{sym, cTopic, rTopic, oTopic}, fuzzB64(t, short), op, evIdx)); !errors.Is(err, ErrMalformedPayload) {
			t.Fatalf("flow body without shares: err = %v, want ErrMalformedPayload", err)
		}
	})
}
