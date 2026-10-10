package sep41_supply

import (
	"encoding/base64"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// FuzzSEP41Counterparty: the counterparty position is chosen by the TYPE
// of topic[2], never by topic count alone. For mint/clawback, topic[2] is
// the counterparty iff it is an Address (legacy admin-prefixed form);
// anything else there (the CAP-67 sep0011 String, garbage, or nothing)
// leaves the counterparty at topic[1]. burn is topic[1] in every shape.
// Whatever topic[2] holds, the decoded amount must be unaffected.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzSEP41Counterparty$ -fuzztime=60s -parallel=2 ./internal/sources/sep41_supply
func FuzzSEP41Counterparty(f *testing.F) {
	seed := func(sv xdr.ScVal) []byte {
		b, err := sv.MarshalBinary()
		if err != nil {
			f.Fatalf("marshal seed: %v", err)
		}
		return b
	}
	adminAddr := func() xdr.ScVal {
		raw := [32]byte{}
		pub := xdr.Uint256(raw)
		aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
		a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
		return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
	}()
	for k := uint8(0); k < 3; k++ {
		f.Add(k, true, seed(adminAddr))
		f.Add(k, true, seed(stringScVal("USDC:"+gAdmin)))
		f.Add(k, true, seed(symbolScVal("native")))
		f.Add(k, true, seed(i128ScVal(1)))
		f.Add(k, true, []byte{0xff})
		f.Add(k, false, []byte{})
	}

	d, err := NewDecoder([]string{cWatched})
	if err != nil {
		f.Fatalf("NewDecoder: %v", err)
	}
	kinds := []string{SymbolMint, SymbolBurn, SymbolClawback}

	f.Fuzz(func(t *testing.T, kindSel uint8, hasTopic2 bool, topic2 []byte) {
		kind := kinds[int(kindSel)%len(kinds)]
		ev := events.Event{
			Type:           "contract",
			ContractID:     cWatched,
			Ledger:         42,
			LedgerClosedAt: "2026-04-30T12:00:00Z",
			TxHash:         "abcd",
			Topic: []string{
				encodeScVal(t, symbolScVal(kind)),
				encodeScVal(t, addressScValG(t, gHolder)),
			},
			Value: encodeScVal(t, i128ScVal(777)),
		}
		if hasTopic2 {
			ev.Topic = append(ev.Topic, base64.StdEncoding.EncodeToString(topic2))
		}

		// Reference: which address the event names as counterparty.
		want, wantErr := gHolder, false
		if hasTopic2 && kind != SymbolBurn {
			// Parseability is scval.Parse's (shared) contract; the type
			// discriminator is what is under test.
			if sv, perr := scval.Parse(ev.Topic[2]); perr == nil && sv.Type == xdr.ScValTypeScvAddress {
				a, aerr := scval.AsAddressStrkey(sv)
				want, wantErr = a, aerr != nil
			}
		}

		outs, derr := d.Decode(ev)
		if wantErr {
			if derr == nil {
				t.Fatalf("%s: unencodable legacy topic[2] accepted: %+v", kind, outs)
			}
			return
		}
		if derr != nil {
			t.Fatalf("%s (topic2=%x, has=%v): Decode: %v", kind, topic2, hasTopic2, derr)
		}
		out := outs[0].(Event)
		if out.Counterparty != want {
			t.Fatalf("%s (topic2=%x, has=%v): Counterparty = %q, want %q", kind, topic2, hasTopic2, out.Counterparty, want)
		}
		if out.Kind != kind || out.Amount.Int64() != 777 || !out.Amount.IsInt64() {
			t.Fatalf("%s: kind/amount = %q/%s, want %q/777", kind, out.Kind, out.Amount, kind)
		}
	})
}
