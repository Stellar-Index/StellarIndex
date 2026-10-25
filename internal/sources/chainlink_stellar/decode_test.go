package chainlink_stellar

import (
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Real mainnet FeedUpdated events from the cache contract: BTC at ledger 64545407 under the
// first WASM (4d2e0f84…), XAUm and LINK at 64957541/64957558 under the upgrade (59ccbf5a…).
const (
	topicBTCID  = "AAAADQAAACABoLTZIAADMgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	valueBTC    = "AAAAEQAAAAEAAAAFAAAADwAAAAZhbnN3ZXIAAAAAAAwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAEi3J+ItL8DFVAAAAAA8AAAAKbGVkZ2VyX3NlcQAAAAAAAwPY4n8AAAAPAAAAB3ByaW1hcnkAAAAAAAAAAAEAAAAPAAAACHJvdW5kX2lkAAAABQAAAAAAAAABAAAADwAAAAl0aW1lc3RhbXAAAAAAAAAFAAAAAGqxbXU="
	topicXAUmID = "AAAADQAAACABl6q/nAADMgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	valueXAUm   = "AAAAEQAAAAEAAAAFAAAADwAAAAZhbnN3ZXIAAAAAAAwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAOHiwdyLodqAAAAAAA8AAAAKbGVkZ2VyX3NlcQAAAAAAAwPfLGUAAAAPAAAAB3ByaW1hcnkAAAAAAAAAAAEAAAAPAAAACHJvdW5kX2lkAAAABQAAAAAAAAikAAAADwAAAAl0aW1lc3RhbXAAAAAAAAAFAAAAAGrQ3wI="
	topicLINKID = "AAAADQAAACABARmbOwADMgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	valueLINK   = "AAAAEQAAAAEAAAAFAAAADwAAAAZhbnN3ZXIAAAAAAAwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACoZQPYot7QAAAAAA8AAAAKbGVkZ2VyX3NlcQAAAAAAAwPfLHYAAAAPAAAAB3ByaW1hcnkAAAAAAAAAAAEAAAAPAAAACHJvdW5kX2lkAAAABQAAAAAAAAWnAAAADwAAAAl0aW1lc3RhbXAAAAAAAAAFAAAAAGrQ300="
)

func feedEvent(topic1, value string) events.Event {
	return events.Event{
		Type:           "contract",
		Ledger:         64957558,
		LedgerClosedAt: "2026-10-15T14:12:34Z",
		ContractID:     MainnetCacheContract,
		TxHash:         "aa",
		OperationIndex: 0,
		EventIndex:     2,
		Topic:          []string{TopicSymbolFeedUpdated, topic1},
		Value:          value,
	}
}

func TestDecoder_DecodesRealFeedUpdated(t *testing.T) {
	d := NewDecoder(MainnetCacheContract)
	cases := []struct {
		name, topic, value, asset, price string
		ts                               int64
	}{
		{"BTC first WASM", topicBTCID, valueBTC, "crypto:BTC", "85847253710324660000000", 1790012789},
		{"XAUm", topicXAUmID, valueXAUm, "rwa:XAUm", "4166857000000000000000", 1792073474},
		{"LINK", topicLINKID, valueLINK, "crypto:LINK", "12134109000000000000", 1792073549},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := feedEvent(tc.topic, tc.value)
			if !d.Matches(ev) {
				t.Fatal("Matches = false for a cache FeedUpdated")
			}
			out, err := d.Decode(ev)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("got %d events, want 1", len(out))
			}
			u := out[0].(UpdateEvent).Update
			if u.Asset.String() != tc.asset || u.Quote.String() != "fiat:USD" {
				t.Errorf("pair = %s/%s, want %s/fiat:USD", u.Asset, u.Quote, tc.asset)
			}
			if u.Price.String() != tc.price || u.Decimals != 18 {
				t.Errorf("price = %s e-%d, want %s e-18", u.Price, u.Decimals, tc.price)
			}
			if !u.Timestamp.Equal(time.Unix(tc.ts, 0)) {
				t.Errorf("timestamp = %s, want unix %d", u.Timestamp, tc.ts)
			}
			if u.Source != SourceName || u.OpIndex != 2 {
				t.Errorf("source/opIndex = %s/%d", u.Source, u.OpIndex)
			}
		})
	}
}

func TestDecoder_GateRejectsForeignContract(t *testing.T) {
	d := NewDecoder(MainnetCacheContract)
	ev := feedEvent(topicLINKID, valueLINK)
	ev.ContractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	if d.Matches(ev) {
		t.Fatal("Matches = true for a FeedUpdated from a foreign contract")
	}
}

// The census fails recognition for any topic on the cache that no decoder matches, so every
// topic the cache emits on mainnet must match; only FeedUpdated writes rows.
func TestDecoder_RecognisesAdminTopicsAsNoOps(t *testing.T) {
	d := NewDecoder(MainnetCacheContract)
	for _, sym := range []string{
		"StaleReport", "InvalidUpdatePermission", "FeedConfigSet", "FeedAdminRemoved",
		"Upgraded", "ownership_transfer", "ownership_transfer_completed",
	} {
		ev := feedEvent(topicLINKID, valueLINK)
		ev.Topic[0] = encodeSymbol(t, sym)
		if !d.Matches(ev) {
			t.Errorf("Matches = false for %s", sym)
			continue
		}
		out, err := d.Decode(ev)
		if err != nil || len(out) != 0 {
			t.Errorf("Decode(%s) = %d events, %v; want none, nil", sym, len(out), err)
		}
	}
}

func TestDecoder_IgnoresUnknownTopics(t *testing.T) {
	d := NewDecoder(MainnetCacheContract)
	ev := feedEvent(topicLINKID, valueLINK)
	ev.Topic[0] = encodeSymbol(t, "SomethingNew")
	if d.Matches(ev) {
		t.Fatal("Matches = true for an unknown topic; the census must still flag it")
	}
	ev.Topic[0] = encodeSymbol(t, "StaleReport")
	ev.ContractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	if d.Matches(ev) {
		t.Fatal("Matches = true for an admin topic from a foreign contract")
	}
}

func TestDecode_UnknownDataIDLandsRaw(t *testing.T) {
	id := make([]byte, 32)
	id[0], id[1] = 0x01, 0xff
	ev := feedEvent(encodeBytes(t, id), valueLINK)
	out, err := NewDecoder(MainnetCacheContract).Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	u := out[0].(UpdateEvent).Update
	if want := "raw:01ff" + strings.Repeat("0", 60); u.Asset.String() != want || u.Asset.IsMapped() {
		t.Errorf("asset = %s, want an unmapped raw:01ff… row", u.Asset)
	}
}

func TestDecode_RejectsBadPayloads(t *testing.T) {
	neg := encodeBody(t, big.NewInt(-1), 1792073549)
	zero := encodeBody(t, big.NewInt(0), 1792073549)
	cases := []struct {
		name string
		ev   func() events.Event
		want error
	}{
		{"negative answer", func() events.Event { return feedEvent(topicLINKID, neg) }, ErrNonPositiveAnswer},
		{"zero answer", func() events.Event { return feedEvent(topicLINKID, zero) }, ErrNonPositiveAnswer},
		{"short data_id", func() events.Event { return feedEvent(encodeBytes(t, []byte{1, 2}), valueLINK) }, ErrMalformedPayload},
		{"missing data_id", func() events.Event {
			ev := feedEvent(topicLINKID, valueLINK)
			ev.Topic = ev.Topic[:1]
			return ev
		}, ErrMalformedPayload},
		{"body not a map", func() events.Event { return feedEvent(topicLINKID, TopicSymbolFeedUpdated) }, ErrMalformedPayload},
		{"event index overflow", func() events.Event {
			ev := feedEvent(topicLINKID, valueLINK)
			ev.EventIndex = eventFanoutStride
			return ev
		}, ErrEventIndexOverflow},
		{"operation index overflow", func() events.Event {
			ev := feedEvent(topicLINKID, valueLINK)
			ev.OperationIndex = opIndexFanoutMax
			return ev
		}, ErrOperationIndexOverflow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDecoder(MainnetCacheContract).Decode(tc.ev())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFeedRegistry_EveryEntryIsMappedAndUSDQuoted(t *testing.T) {
	for id, e := range feedRegistry {
		if len(id) != 64 {
			t.Errorf("%s: data_id hex length %d, want 64", id, len(id))
		}
		if e.Quote.String() != "fiat:USD" {
			t.Errorf("%s: quote %s", id, e.Quote)
		}
		if b := e.Base.String(); b == "raw:FLHY" || b == "raw:USPC" {
			continue
		}
		if !e.Base.IsMapped() {
			t.Errorf("%s: base %s unmapped", id, e.Base)
		}
	}
}

// USPC NAV (FeedConfigSet at ledger 65079956) has no canonical asset on Stellar, so it is named raw.
func TestResolveFeed_USPCNavIsNamedRaw(t *testing.T) {
	id, err := hex.DecodeString("016e06407e000332000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	e, err := resolveFeed(id)
	if err != nil {
		t.Fatal(err)
	}
	if e.Base.String() != "raw:USPC" || e.Base.IsMapped() || e.Quote.String() != "fiat:USD" {
		t.Errorf("got %s/%s, want raw:USPC/fiat:USD", e.Base, e.Quote)
	}
}

func encodeSymbol(t *testing.T, s string) string {
	t.Helper()
	sym := xdr.ScSymbol(s)
	return marshal(t, xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym})
}

func encodeBytes(t *testing.T, b []byte) string {
	t.Helper()
	bs := xdr.ScBytes(b)
	return marshal(t, xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &bs})
}

func encodeBody(t *testing.T, answer *big.Int, ts uint64) string {
	t.Helper()
	// Two's complement over 256 bits, split into four big-endian 64-bit words.
	u := new(big.Int).Set(answer)
	if u.Sign() < 0 {
		u.Add(u, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	var w [32]byte
	u.FillBytes(w[:])
	word := func(i int) uint64 {
		var v uint64
		for _, c := range w[i*8 : i*8+8] {
			v = v<<8 | uint64(c)
		}
		return v
	}
	parts := xdr.Int256Parts{
		HiHi: xdr.Int64(int64(word(0))), // #nosec G115 -- two's complement reinterpretation
		HiLo: xdr.Uint64(word(1)),
		LoHi: xdr.Uint64(word(2)),
		LoLo: xdr.Uint64(word(3)),
	}
	tsv := xdr.Uint64(ts)
	sym := func(s string) xdr.ScVal {
		v := xdr.ScSymbol(s)
		return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &v}
	}
	m := xdr.ScMap{
		{Key: sym("answer"), Val: xdr.ScVal{Type: xdr.ScValTypeScvI256, I256: &parts}},
		{Key: sym("timestamp"), Val: xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &tsv}},
	}
	mp := &m
	return marshal(t, xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp})
}

func marshal(t *testing.T, v xdr.ScVal) string {
	t.Helper()
	s, err := xdr.MarshalBase64(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
