package cctp

import (
	"encoding/base64"
	"math/big"
	"testing"

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

func rawI128(hi int64, lo uint64) xdr.ScVal {
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

// assertDecimal checks a decimal-string money column against the
// reference value exactly: the string must parse back to the same
// *big.Int (ADR-0003 — these land in NUMERIC columns).
func assertDecimal(t *testing.T, col, got string, want *big.Int) {
	t.Helper()
	v, ok := new(big.Int).SetString(got, 10)
	if !ok || v.Cmp(want) != 0 {
		t.Fatalf("%s = %q, want %s", col, got, want)
	}
}

// reversed returns entries in reverse order — decode is by field NAME,
// so the order the Map carries them in must not matter.
func reversed(on bool, entries ...xdr.ScMapEntry) xdr.ScVal {
	if on {
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}
	return scMap(entries...)
}

func decodeOneCCTP(t *testing.T, ev events.Event) Event {
	t.Helper()
	d := NewDecoder()
	if !d.Matches(ev) {
		t.Fatalf("%s from %s not matched", Classify(&ev), ev.ContractID)
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

// FuzzMoneyColumnsAreExact drives every CCTP event that carries money —
// deposit_for_burn (amount, max_fee), mint_and_withdraw (amount,
// fee_collected), mint_and_forward (amount) and
// set_burn_limit_per_message (burn_limit_per_message) — through the
// production Matches + Decode over the whole i128 × i128 domain and
// asserts each promoted column is the exact value of its OWN named field:
// no int64 truncation, no amount/fee cross-wiring, whatever order the
// Map carries the fields in. It also pins EventIndex, the cctp_events PK
// discriminator, to the source event's index, and the identity gate: the
// same bytes from a non-CCTP contract are never claimed.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzMoneyColumnsAreExact$ -fuzztime=60s -parallel=2 ./internal/sources/cctp/
func FuzzMoneyColumnsAreExact(f *testing.F) {
	// The adapter fixture (12,345,678 / 500), the int64 / uint64 edges and
	// the i128 extremes.
	f.Add(int64(0), uint64(12_345_678), int64(0), uint64(500), false, uint16(0))
	f.Add(int64(0), uint64(1)<<63, int64(0), ^uint64(0), true, uint16(1))
	f.Add(int64(1), uint64(0), int64(-1), ^uint64(0), false, uint16(4))
	f.Add(int64(^uint64(0)>>1), ^uint64(0), int64(-1)<<63, uint64(0), true, uint16(65535))
	f.Add(int64(0), uint64(7), int64(0), uint64(7), false, uint16(2)) // equal amount and fee

	f.Fuzz(func(t *testing.T, ah int64, al uint64, fh int64, fl uint64, rev bool, evIdx uint16) {
		amt, fee := i128Want(ah, al), i128Want(fh, fl)
		token := makeContractStrkey(t, 0x10)
		acct := makeAccountStrkey(t, 0x20)
		withMeta := func(contract string, topics []string, body xdr.ScVal) events.Event {
			return events.Event{
				Type:           "contract",
				Ledger:         62_700_000,
				LedgerClosedAt: "2026-05-20T14:00:00Z",
				ContractID:     contract,
				OperationIndex: 1,
				EventIndex:     int(evIdx),
				TxHash:         "abc123",
				Topic:          topics,
				Value:          b64(t, body),
			}
		}

		// deposit_for_burn
		dfb := withMeta(MainnetTokenMessengerMinter, []string{
			TopicSymbolDepositForBurn,
			b64(t, contractAddrFromStrkey(t, token)),
			b64(t, accountAddrFromStrkey(t, acct)),
			b64(t, u32(2000)),
		}, reversed(rev,
			xdr.ScMapEntry{Key: symbol("amount"), Val: rawI128(ah, al)},
			xdr.ScMapEntry{Key: symbol("destination_caller"), Val: scBytes(makeBytesN32(0x50))},
			xdr.ScMapEntry{Key: symbol("destination_domain"), Val: u32(6)},
			xdr.ScMapEntry{Key: symbol("destination_token_messenger"), Val: scBytes(makeBytesN32(0x40))},
			xdr.ScMapEntry{Key: symbol("hook_data"), Val: scBytes(nil)},
			xdr.ScMapEntry{Key: symbol("max_fee"), Val: rawI128(fh, fl)},
			xdr.ScMapEntry{Key: symbol("mint_recipient"), Val: scBytes(makeBytesN32(0x30))},
		))
		got := decodeOneCCTP(t, dfb)
		assertDecimal(t, "deposit_for_burn.Amount", got.Amount, amt)
		assertDecimal(t, "deposit_for_burn.Fee", got.Fee, fee)
		if got.Token != token || got.EventIndex != uint32(evIdx) {
			t.Fatalf("deposit_for_burn token/event index = %s/%d, want %s/%d", got.Token, got.EventIndex, token, evIdx)
		}
		if got.CounterpartyDomain == nil || *got.CounterpartyDomain != 6 {
			t.Fatalf("deposit_for_burn destination domain = %v, want 6", got.CounterpartyDomain)
		}

		// The identity gate: identical bytes from a foreign contract.
		foreign := dfb
		foreign.ContractID = makeContractStrkey(t, 0x99)
		if NewDecoder().Matches(foreign) {
			t.Fatal("deposit_for_burn from a non-CCTP contract matched")
		}
		if out, err := NewDecoder().Decode(foreign); err != nil || len(out) != 0 {
			t.Fatalf("Decode of a foreign contract = %d events, err %v; want none", len(out), err)
		}

		// mint_and_withdraw
		mint := makeContractStrkey(t, 0x11)
		got = decodeOneCCTP(t, withMeta(MainnetTokenMessengerMinter, []string{
			TopicSymbolMintAndWithdraw,
			b64(t, accountAddrFromStrkey(t, acct)),
			b64(t, contractAddrFromStrkey(t, mint)),
		}, reversed(rev,
			xdr.ScMapEntry{Key: symbol("amount"), Val: rawI128(ah, al)},
			xdr.ScMapEntry{Key: symbol("fee_collected"), Val: rawI128(fh, fl)},
		)))
		assertDecimal(t, "mint_and_withdraw.Amount", got.Amount, amt)
		assertDecimal(t, "mint_and_withdraw.Fee", got.Fee, fee)
		if got.Token != mint || got.Attributes["mint_recipient"] != acct || got.EventIndex != uint32(evIdx) {
			t.Fatalf("mint_and_withdraw token/recipient/event index = %s/%v/%d", got.Token, got.Attributes["mint_recipient"], got.EventIndex)
		}

		// mint_and_forward
		got = decodeOneCCTP(t, withMeta(MainnetCctpForwarder, []string{TopicSymbolMintAndForward}, reversed(rev,
			xdr.ScMapEntry{Key: symbol("amount"), Val: rawI128(ah, al)},
			xdr.ScMapEntry{Key: symbol("forward_recipient"), Val: accountAddrFromStrkey(t, acct)},
			xdr.ScMapEntry{Key: symbol("token"), Val: contractAddrFromStrkey(t, token)},
		)))
		assertDecimal(t, "mint_and_forward.Amount", got.Amount, amt)
		if got.Fee != "" || got.Token != token || got.Attributes["forward_recipient"] != acct {
			t.Fatalf("mint_and_forward fee/token/recipient = %q/%s/%v", got.Fee, got.Token, got.Attributes["forward_recipient"])
		}

		// set_burn_limit_per_message — a limit, so it is NOT an Amount.
		got = decodeOneCCTP(t, withMeta(MainnetTokenMessengerMinter, []string{
			TopicSymbolSetBurnLimitPerMessage,
			b64(t, contractAddrFromStrkey(t, token)),
		}, scMap(xdr.ScMapEntry{Key: symbol("burn_limit_per_message"), Val: rawI128(fh, fl)})))
		limit, _ := got.Attributes["burn_limit_per_message"].(string)
		assertDecimal(t, "set_burn_limit_per_message", limit, fee)
		if got.Amount != "" || got.Fee != "" {
			t.Fatalf("burn limit leaked into Amount/Fee: %q/%q", got.Amount, got.Fee)
		}
	})
}

// FuzzDecodeArbitraryBody feeds arbitrary body bytes under every CCTP
// topic symbol from a CCTP contract. The decoder must never panic (the
// dispatcher would recover and lose the event silently), and whatever it
// accepts must be exactly one Event of the classified type.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzDecodeArbitraryBody$ -fuzztime=60s -parallel=2 ./internal/sources/cctp/
func FuzzDecodeArbitraryBody(f *testing.F) {
	syms := []string{
		TopicSymbolDepositForBurn, TopicSymbolMintAndWithdraw, TopicSymbolMessageSent,
		TopicSymbolMessageReceived, TopicSymbolMintAndForward, TopicSymbolOwnershipTransfer,
		TopicSymbolOwnershipTransferCompleted, TopicSymbolAdminChanged, TopicSymbolRemoteTokenMessengerAdded,
		TopicSymbolTokenPairLinked, TopicSymbolAdminChangeStarted, TopicSymbolAttesterEnabled,
		TopicSymbolAttesterManagerUpdated, TopicSymbolDenylisted, TopicSymbolDenylisterChanged,
		TopicSymbolFeeRecipientSet, TopicSymbolMaxMessageBodySizeUpdated, TopicSymbolMinFeeControllerSet,
		TopicSymbolPauserChanged, TopicSymbolRescuerChanged, TopicSymbolSetBurnLimitPerMessage,
		TopicSymbolSetTokenController, TopicSymbolSignatureThresholdUpdated, TopicSymbolSwapMinterConfigSet,
		TopicSymbolTokenDecimalConfigAdded, TopicSymbolUnDenylisted,
	}
	enc := func(sv xdr.ScVal) []byte {
		b, err := sv.MarshalBinary()
		if err != nil {
			f.Fatal(err)
		}
		return b
	}
	var cid xdr.ContractId
	cid[0] = 0x10
	var pub xdr.Uint256
	pub[0] = 0x20
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
	cAddr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	gAddr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	seedEv := events.Event{
		Type:           "contract",
		Ledger:         62_700_000,
		LedgerClosedAt: "2026-05-20T14:00:00Z",
		ContractID:     MainnetTokenMessengerMinter,
		TxHash:         "abc123",
		Topic: []string{
			TopicSymbolDepositForBurn,
			base64.StdEncoding.EncodeToString(enc(xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &cAddr})),
			base64.StdEncoding.EncodeToString(enc(xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &gAddr})),
			base64.StdEncoding.EncodeToString(enc(u32(2000))),
		},
	}
	// A well-formed deposit_for_burn body, plus the two shapes other
	// kinds carry, so the mutator starts from real structure.
	seedBodies := [][]byte{
		enc(scMap(
			xdr.ScMapEntry{Key: symbol("amount"), Val: i128(big.NewInt(12_345_678))},
			xdr.ScMapEntry{Key: symbol("destination_caller"), Val: scBytes(makeBytesN32(0x50))},
			xdr.ScMapEntry{Key: symbol("destination_domain"), Val: u32(0)},
			xdr.ScMapEntry{Key: symbol("destination_token_messenger"), Val: scBytes(makeBytesN32(0x40))},
			xdr.ScMapEntry{Key: symbol("hook_data"), Val: scBytes([]byte("hook"))},
			xdr.ScMapEntry{Key: symbol("max_fee"), Val: i128(big.NewInt(500))},
			xdr.ScMapEntry{Key: symbol("mint_recipient"), Val: scBytes(makeBytesN32(0x30))},
		)),
		enc(scMap(
			xdr.ScMapEntry{Key: symbol("amount"), Val: i128(big.NewInt(1))},
			xdr.ScMapEntry{Key: symbol("fee_collected"), Val: i128(big.NewInt(0))},
		)),
		enc(scBytes(makeBytesN32(0x01))),
	}
	for i := range syms {
		f.Add(uint8(i), seedBodies[i%len(seedBodies)], uint8(i))
	}
	for i := range syms {
		f.Add(uint8(i), seedBodies[0], uint8(3))
	}
	f.Add(uint8(2), []byte{}, uint8(1))
	f.Add(uint8(20), []byte{0, 0, 0, 17, 0, 0, 0, 1}, uint8(2))

	f.Fuzz(func(t *testing.T, sel uint8, body []byte, nTopics uint8) {
		sym := syms[int(sel)%len(syms)]
		pool := []string{sym, seedEv.Topic[1], seedEv.Topic[2], seedEv.Topic[3]}
		topics := pool[:1+int(nTopics)%len(pool)]
		ev := seedEv
		ev.Topic = topics
		ev.Value = base64.StdEncoding.EncodeToString(body)
		want := Classify(&ev)
		if want == "" {
			t.Fatalf("topic %q not classified", sym)
		}
		out, err := NewDecoder().Decode(ev)
		if err != nil {
			return
		}
		if len(out) != 1 {
			t.Fatalf("accepted body produced %d events, want 1", len(out))
		}
		if got := out[0].(Event).EventType; got != want {
			t.Fatalf("EventType = %s, want %s", got, want)
		}
	})
}
