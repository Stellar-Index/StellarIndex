package phoenix

import (
	"encoding/base64"
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Boundary tests for the branches the package suite left unpinned: the
// sweep-rescue slot set, and the factory-announcement gate (topic pair,
// emitter identity, announced-address kind).

// TestRawSwap_Decodable_requiresEveryConsumedSlot: an aged-out group missing
// any slot decodeSwap reads must stay an orphan (never reach decodeSwap and
// its nil dereference); a group missing only an unread slot is rescuable.
func TestRawSwap_Decodable_requiresEveryConsumedSlot(t *testing.T) {
	ev := &events.Event{}
	for _, tc := range []struct {
		topic     string
		decodable bool
	}{
		{TopicSymbolSender, false},
		{TopicSymbolSellToken, false},
		{TopicSymbolOfferAmount, false},
		{TopicSymbolBuyToken, false},
		{TopicSymbolReturnAmount, false},
		{TopicSymbolActualReceived, true},
		{TopicSymbolSpreadAmount, true},
		{TopicSymbolReferralFee, true},
	} {
		var r RawSwap
		for _, f := range stringSwapTopics {
			if f != tc.topic {
				if err := r.assign(ev, f); err != nil {
					t.Fatal(err)
				}
			}
		}
		if got := r.Decodable(); got != tc.decodable {
			t.Errorf("missing %q: Decodable() = %v, want %v", tc.topic, got, tc.decodable)
		}
		if !tc.decodable {
			if _, err := decodeSwap(&r); !errors.Is(err, ErrIncompleteSwap) {
				t.Errorf("missing %q: decodeSwap err = %v, want ErrIncompleteSwap", tc.topic, err)
			}
		}
	}
}

// TestDecoder_SweepOrphansGroupMissingOfferAmount drives the same property
// through the production path: a 7-field group lacking offer_amount ages out
// as an orphan, emitting no trade.
func TestDecoder_SweepOrphansGroupMissingOfferAmount(t *testing.T) {
	d := newTestDecoder()
	sell, buy := makeC(t, 0x51), makeC(t, 0x52)
	senderVal, _ := accountVal(t, 0x53)
	bodies := map[string]string{
		TopicSymbolSender:         b64Marshal(t, senderVal),
		TopicSymbolSellToken:      b64Marshal(t, contractVal(t, sell)),
		TopicSymbolActualReceived: b64Marshal(t, i128HiLo(0, 10)),
		TopicSymbolBuyToken:       b64Marshal(t, contractVal(t, buy)),
		TopicSymbolReturnAmount:   b64Marshal(t, i128HiLo(0, 20)),
		TopicSymbolSpreadAmount:   b64Marshal(t, i128HiLo(0, 1)),
		TopicSymbolReferralFee:    b64Marshal(t, i128HiLo(0, 0)),
	}
	for topic, body := range bodies {
		if out, err := d.Decode(makeFieldEventAt(t, topic, body, "old", "2026-04-23T12:00:00Z")); err != nil || len(out) != 0 {
			t.Fatalf("buffering %q: out=%v err=%v", topic, out, err)
		}
	}
	// Ten minutes later: the old group is past defaultOrphanMaxAge.
	out, err := d.Decode(makeFieldEventAt(t, TopicSymbolSender, bodies[TopicSymbolSender], "new", "2026-04-23T12:10:00Z"))
	if err != nil || len(out) != 0 {
		t.Fatalf("sweep: out=%v err=%v, want no trade", out, err)
	}
	if got := d.EvictedOrphans(); got != 1 {
		t.Fatalf("EvictedOrphans = %d, want 1", got)
	}
}

// realCreateBody is a real factory ("create","liquidity_pool") body from
// test/fixtures/phoenix/factory-create (ledger 51572026).
const realCreateBody = "AAAAEgAAAAFOKMq33nPyGnLDFPIU2W2jUUiShHdABJPjEW7pvCVp4A=="

func createEvent(emitter, topic1, body string) events.Event {
	return events.Event{
		Topic:          []string{TopicSymbolCreate, topic1},
		Value:          body,
		Ledger:         51_572_026,
		TxHash:         "02cea787b98e0b3d426ea36d9510e62b1d125a16162059d13a2895531f0887b9",
		EventIndex:     3,
		ContractID:     emitter,
		LedgerClosedAt: "2024-05-07T20:27:59Z",
	}
}

func TestDecodeAnnouncedPool_realBodyAndNonContractRejected(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(realCreateBody)
	if err != nil {
		t.Fatal(err)
	}
	want, err := strkey.Encode(strkey.VersionByteContract, raw[8:])
	if err != nil {
		t.Fatal(err)
	}
	ev := createEvent(MainnetFactory, TopicCreateLiquidityPool, realCreateBody)
	got, err := decodeAnnouncedPool(&ev)
	if err != nil || got != want {
		t.Fatalf("decodeAnnouncedPool(real) = %q, %v; want %q", got, err, want)
	}

	acct, _ := accountVal(t, 0x61)
	ev.Value = b64Marshal(t, acct)
	if got, err := decodeAnnouncedPool(&ev); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("decodeAnnouncedPool(account) = %q, %v; want ErrMalformedPayload", got, err)
	}
}

// TestDecoder_CreatePoolGate pins the three legs of pool admission: only the
// ("create","liquidity_pool") pair classifies, only a FACTORY emitter matches
// (a curated pool republishing the topics must not), and a matched
// announcement admits the pool into the gate.
func TestDecoder_CreatePoolGate(t *testing.T) {
	if a, _ := classifyAny(&events.Event{Topic: []string{TopicSymbolCreate, TopicSymbolSender}}); a != actionUnknown {
		t.Fatalf(`classifyAny(("create","sender")) = %v, want actionUnknown`, a)
	}

	d := NewDecoder()
	curated := MainnetPools[0]
	if d.Matches(createEvent(curated, TopicCreateLiquidityPool, realCreateBody)) {
		t.Fatal("Matches accepted a create announcement from a curated pool, not the factory")
	}
	if d.Matches(createEvent(MainnetFactory, TopicSymbolSender, realCreateBody)) {
		t.Fatal(`Matches accepted a factory ("create","sender") event`)
	}

	// A pool outside the curated seed, so admission is observable.
	ann := createEvent(MainnetFactory, TopicCreateLiquidityPool, b64Marshal(t, contractVal(t, makeC(t, 0x77))))
	if !d.Matches(ann) {
		t.Fatal("Matches rejected the factory's create announcement")
	}
	pool, err := decodeAnnouncedPool(&ann)
	if err != nil {
		t.Fatal(err)
	}
	swapFromPool := events.Event{Topic: []string{TopicSymbolSwap, TopicSymbolSender}, ContractID: pool}
	if d.Matches(swapFromPool) {
		t.Fatal("announced pool matched before its announcement was decoded")
	}
	if out, err := d.Decode(ann); err != nil || len(out) != 0 {
		t.Fatalf("Decode(create) = %v, %v; want no events, no error", out, err)
	}
	if !d.Matches(swapFromPool) {
		t.Fatal("announced pool not admitted after Decode(create)")
	}
}
