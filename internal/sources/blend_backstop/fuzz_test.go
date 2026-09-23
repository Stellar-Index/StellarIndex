package blend_backstop

import (
	"encoding/base64"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

func i128FromParts(hi int64, lo uint64) *big.Int {
	n := new(big.Int).Lsh(big.NewInt(hi), 64)
	return n.Add(n, new(big.Int).SetUint64(lo))
}

func u64SV(n uint64) xdr.ScVal {
	x := xdr.Uint64(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &x}
}

// backstopCase is one event shape: its topics after topic[0], its body
// for amounts (a, b), and the promoted (Amount, Amount2) the decoder
// must produce.
type backstopCase struct {
	name     string
	topic0   string
	topics   func(t *testing.T) []string
	body     func(t *testing.T, a, b *big.Int) xdr.ScVal
	amounts  func(a, b *big.Int) (string, string)
	wantPool bool
	wantUser bool
}

func backstopCases() []backstopCase {
	pool := func(t *testing.T) string { return b64SV(t, contractAddrSV(t, contractStrkey(t, 0x11))) }
	user := func(t *testing.T) string { return b64SV(t, accountAddrSV(t, accountStrkey(t, 0x22))) }
	poolUser := func(t *testing.T) []string { return []string{pool(t), user(t)} }
	poolOnly := func(t *testing.T) []string { return []string{pool(t)} }
	userOnly := func(t *testing.T) []string { return []string{user(t)} }
	none := func(*testing.T) []string { return nil }
	pair := func(_ *testing.T, a, b *big.Int) xdr.ScVal { return vecSV(i128SV(a), i128SV(b)) }
	bare := func(_ *testing.T, a, _ *big.Int) xdr.ScVal { return i128SV(a) }
	ab := func(a, b *big.Int) (string, string) { return a.String(), b.String() }
	aOnly := func(a, _ *big.Int) (string, string) { return a.String(), "" }
	return []backstopCase{
		{"deposit", TopicSymbolDeposit, poolUser, pair, ab, true, true},
		// withdraw's wire order is (shares_burned, tokens_out); Amount is tokens.
		{
			"withdraw", TopicSymbolWithdraw, poolUser, pair,
			func(a, b *big.Int) (string, string) { return b.String(), a.String() }, true, true,
		},
		{"gulp_emissions_v2", TopicSymbolGulpEmissions, poolOnly, pair, ab, true, false},
		{"gulp_emissions_v1", TopicSymbolGulpEmissions, none, bare, aOnly, false, false},
		{"claim", TopicSymbolClaim, userOnly, bare, aOnly, false, true},
		{"donate", TopicSymbolDonate, func(t *testing.T) []string {
			return []string{pool(t), b64SV(t, contractAddrSV(t, contractStrkey(t, 0x33)))}
		}, bare, aOnly, true, false},
		{"distribute", TopicSymbolDistribute, none, bare, aOnly, false, false},
		{"dequeue_withdrawal", TopicSymbolDequeueWithdrawal, poolUser, bare, aOnly, true, true},
		{
			"queue_withdrawal", TopicSymbolQueueWithdrawal, poolUser,
			func(_ *testing.T, a, b *big.Int) xdr.ScVal { return vecSV(i128SV(a), u64SV(b.Uint64())) }, aOnly, true, true,
		},
		{
			"draw", TopicSymbolDraw, poolOnly,
			func(t *testing.T, a, _ *big.Int) xdr.ScVal {
				return vecSV(accountAddrSV(t, accountStrkey(t, 0x44)), i128SV(a))
			}, aOnly, true, false,
		},
	}
}

// FuzzBackstopAmounts routes every amount-bearing backstop shape
// through the dispatcher entry point and requires the promoted Amount /
// Amount2 to be the exact decimal of the wire i128 (ADR-0003: no
// int64/float truncation, sign preserved) in the documented slot.
func FuzzBackstopAmounts(f *testing.F) {
	f.Add(uint8(0), int64(0), uint64(179_414_602), int64(0), uint64(130_950_149))
	f.Add(uint8(1), int64(0), uint64(13_000_000), int64(0), uint64(13_030_672))
	f.Add(uint8(2), int64(1<<40), uint64(1), int64(-1), uint64(0))
	f.Add(uint8(8), int64(0), ^uint64(0), int64(0), uint64(1_700_000_000))
	f.Add(uint8(9), int64(-1<<63), uint64(0), int64(0), uint64(0))
	cases := backstopCases()
	f.Fuzz(func(t *testing.T, sel uint8, aHi int64, aLo uint64, bHi int64, bLo uint64) {
		c := cases[int(sel)%len(cases)]
		a, b := i128FromParts(aHi, aLo), i128FromParts(bHi, bLo)
		if c.name == "queue_withdrawal" {
			b = new(big.Int).SetUint64(bLo)
		}
		ev := events.Event{
			ContractID:     MainnetBackstopV2,
			Topic:          append([]string{c.topic0}, c.topics(t)...),
			Value:          b64SV(t, c.body(t, a, b)),
			LedgerClosedAt: "2025-05-14T12:00:00Z",
		}
		out, err := NewDecoder().Decode(ev)
		if err != nil {
			t.Fatalf("%s: Decode: %v", c.name, err)
		}
		if len(out) != 1 {
			t.Fatalf("%s: %d events, want 1", c.name, len(out))
		}
		got, ok := out[0].(Event)
		if !ok {
			t.Fatalf("%s: %T, want Event", c.name, out[0])
		}
		wantA, wantB := c.amounts(a, b)
		if got.Amount != wantA || got.Amount2 != wantB {
			t.Fatalf("%s: (Amount, Amount2)=(%q,%q) want (%q,%q)", c.name, got.Amount, got.Amount2, wantA, wantB)
		}
		if (got.Pool != "") != c.wantPool || (got.UserAddress != "") != c.wantUser {
			t.Fatalf("%s: pool=%q user=%q, want pool=%v user=%v", c.name, got.Pool, got.UserAddress, c.wantPool, c.wantUser)
		}
		if c.name == "queue_withdrawal" && got.Attributes["expiration"] != b.Uint64() {
			t.Fatalf("queue_withdrawal expiration=%v want %d", got.Attributes["expiration"], b.Uint64())
		}
	})
}

// FuzzBackstopDecodeArbitrary feeds arbitrary topic counts and body
// bytes for every backstop kind. The decoder must never panic (a short
// topic slice must be an error, not an index-out-of-range), and any
// promoted amount it does emit must be a canonical base-10 integer.
func FuzzBackstopDecodeArbitrary(f *testing.F) {
	pair, _ := vecSV(i128SV(big.NewInt(5)), i128SV(big.NewInt(6))).MarshalBinary()
	bare, _ := i128SV(big.NewInt(7)).MarshalBinary()
	f.Add(uint8(0), uint8(1), pair) // deposit with only [sym, pool]
	f.Add(uint8(0), uint8(0), pair) // deposit with only [sym]
	f.Add(uint8(4), uint8(1), pair) // withdraw with only [sym, pool]
	f.Add(uint8(3), uint8(1), pair) // queue_withdrawal with only [sym, pool]
	f.Add(uint8(7), uint8(1), bare) // dequeue_withdrawal with only [sym, pool]
	f.Add(uint8(2), uint8(1), bare) // donate with only [sym, pool]
	f.Add(uint8(8), uint8(0), pair) // draw with only [sym]
	f.Add(uint8(1), uint8(0), bare) // claim with only [sym]
	f.Add(uint8(6), uint8(3), []byte{0, 0, 0, 16, 0, 0, 0, 2})
	topic0s := []string{
		TopicSymbolDeposit, TopicSymbolClaim, TopicSymbolDonate, TopicSymbolQueueWithdrawal,
		TopicSymbolWithdraw, TopicSymbolDistribute, TopicSymbolGulpEmissions, TopicSymbolDequeueWithdrawal,
		TopicSymbolDraw, TopicSymbolRwZoneAdd, TopicSymbolRwZone, TopicSymbolRwZoneRemove,
	}
	f.Fuzz(func(t *testing.T, sel, nTopics uint8, body []byte) {
		topics := []string{topic0s[int(sel)%len(topic0s)]}
		for i := range int(nTopics % 4) {
			topics = append(topics, b64SV(t, contractAddrSV(t, contractStrkey(t, byte(0x50+i)))))
		}
		ev := events.Event{
			ContractID:     MainnetBackstopV2,
			Topic:          topics,
			Value:          base64.StdEncoding.EncodeToString(body),
			LedgerClosedAt: "2025-05-14T12:00:00Z",
		}
		out, err := NewDecoder().Decode(ev)
		if err != nil {
			return
		}
		got, ok := out[0].(Event)
		if !ok || got.Attributes == nil {
			t.Fatalf("Decode returned %#v", out)
		}
		for _, s := range []string{got.Amount, got.Amount2} {
			if s == "" {
				continue
			}
			n, ok := new(big.Int).SetString(s, 10)
			if !ok || n.String() != s {
				t.Fatalf("promoted amount %q is not a canonical integer", s)
			}
		}
	})
}
