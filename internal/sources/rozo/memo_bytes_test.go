package rozo

import (
	"math/big"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// paymentEventWithMemo is the live long-topic payment fixture with a
// caller-chosen memo — the one field of a v1 event whose bytes the
// payer picks freely.
func paymentEventWithMemo(t *testing.T, memo string) events.Event {
	t.Helper()
	from := makeAccountStrkey(t, 0x20)
	dest := makeAccountStrkey(t, 0x30)
	ev := paymentEventLongTopic(t, MainnetPaymentContract)
	ev.Value = b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount"), Val: i128(big.NewInt(1))},
		xdr.ScMapEntry{Key: symbol("destination"), Val: accountAddrFromStrkey(t, dest)},
		xdr.ScMapEntry{Key: symbol("from"), Val: accountAddrFromStrkey(t, from)},
		xdr.ScMapEntry{Key: symbol("memo"), Val: scString(memo)},
	))
	return ev
}

// textColumnSafe is the Postgres `text` acceptance rule: valid UTF-8
// and no NUL. Anything else is refused with SQLSTATE 22021.
func textColumnSafe(s string) bool {
	return utf8.ValidString(s) && !strings.Contains(s, "\x00")
}

// TestDecoder_Decode_MemoIsBytesNotText is the F052 regression guard.
//
// The memo is an ScString: the payer chooses its BYTES, for one stroop.
// The decoder used to hand those bytes straight to the rozo_events.memo
// `text` column, where a NUL or an invalid UTF-8 sequence is refused on
// every attempt — a permanently un-ingestible event.
//
// Pins, through the production Decoder.Decode entry point:
//   - the emitted Memo is text-column-safe for every hostile shape;
//   - it is the EXACT expected value (deterministic — a re-derive
//     writes the same row), not merely "something valid";
//   - no byte is lost: scval.FromText recovers the on-chain memo;
//   - ordinary memos are byte-identical to what was stored before.
func TestDecoder_Decode_MemoIsBytesNotText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		memo string
		want string
	}{
		{"ordinary tag", "binance-tag-42", "binance-tag-42"},
		{"empty memo stays empty", "", ""},
		{"multibyte utf8", "commande-№42-日本", "commande-№42-日本"},
		{"NUL", "tag\x00tail", `\x746167007461696c`},
		{"invalid utf8", "tag\xff\xfe", `\x746167fffe`},
		{"truncated multibyte", "ab\xe2\x82", `\x6162e282`},
		{"encoded surrogate", "\xed\xa0\x80", `\xeda080`},
		{"literal that mimics an encoding", `\x00`, `\x5c783030`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := NewDecoder().Decode(paymentEventWithMemo(t, tc.memo))
			if err != nil {
				t.Fatalf("Decode: %v — a hostile memo must not cost the event", err)
			}
			if len(out) != 1 {
				t.Fatalf("Decode emitted %d events, want 1", len(out))
			}
			ev, ok := out[0].(Event)
			if !ok {
				t.Fatalf("emitted %T, want rozo.Event", out[0])
			}
			if ev.Memo == nil {
				t.Fatal("payment Memo is nil")
			}
			if !textColumnSafe(*ev.Memo) {
				t.Fatalf("Memo %q is not text-column-safe: Postgres refuses it (22021) and the row is lost", *ev.Memo)
			}
			if *ev.Memo != tc.want {
				t.Fatalf("Memo = %q, want %q", *ev.Memo, tc.want)
			}
			back, err := scval.FromText(*ev.Memo)
			if err != nil {
				t.Fatalf("FromText(%q): %v", *ev.Memo, err)
			}
			if string(back) != tc.memo {
				t.Fatalf("memo bytes lost: on-chain %q, recovered %q", tc.memo, back)
			}
		})
	}
}

// TestDecoder_Decode_EveryPersistedStringIsTextSafe covers the class
// rather than the one field the finding named: with the most hostile
// memo, EVERY string a payment or flush row binds to a text column must
// be acceptable to Postgres.
func TestDecoder_Decode_EveryPersistedStringIsTextSafe(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, ev Event) {
		t.Helper()
		fields := map[string]string{
			"ContractID": ev.ContractID, "TxHash": ev.TxHash, "EventType": ev.EventType,
			"Amount": ev.Amount, "Destination": ev.Destination,
		}
		for name, p := range map[string]*string{"From": ev.From, "Memo": ev.Memo, "Token": ev.Token} {
			if p != nil {
				fields[name] = *p
			}
		}
		for name, v := range fields {
			if !textColumnSafe(v) {
				t.Errorf("%s = %q is not text-column-safe", name, v)
			}
		}
	}

	out, err := NewDecoder().Decode(paymentEventWithMemo(t, "\x00\xff\xc0\x80\xed\xa0\x80"))
	if err != nil || len(out) != 1 {
		t.Fatalf("payment Decode = %d events, %v", len(out), err)
	}
	check(t, out[0].(Event))

	sweptContract := makeContractStrkey(t, 0x41)
	dest := makeAccountStrkey(t, 0x30)
	flush := paymentEventLongTopic(t, MainnetPaymentContract)
	flush.Topic = []string{TopicSymbolFlushEvent}
	flush.Value = b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount"), Val: i128(big.NewInt(7))},
		xdr.ScMapEntry{Key: symbol("destination"), Val: accountAddrFromStrkey(t, dest)},
		xdr.ScMapEntry{Key: symbol("token"), Val: contractAddrFromStrkey(t, sweptContract)},
	))
	out, err = NewDecoder().Decode(flush)
	if err != nil || len(out) != 1 {
		t.Fatalf("flush Decode = %d events, %v", len(out), err)
	}
	check(t, out[0].(Event))
}
