package aquarius

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// kill/unkill events carry a single topic (Symbol(action)) and an
// SCV_VOID body ("AAAAAQ==") — verified in the lake. emitKill reads
// only the action + identity, so the body is irrelevant to the decode.
func TestDecode_killSwitch(t *testing.T) {
	d := NewDecoder()
	ev := events.Event{
		ContractID:     "CCRULRY3VV6NVQHZ43KDKC75OHR6YEH7WCQBDTTU6JBIH75NC72VW6JJ",
		Ledger:         61_000_000,
		TxHash:         "killtx",
		OperationIndex: 0,
		EventIndex:     2,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
		Topic:          []string{TopicSymbolKillDeposit},
		Value:          "AAAAAQ==", // SCV_VOID — unused by emitKill
	}
	if got := classify(&ev); got != EventKillDeposit {
		t.Fatalf("classify = %q, want %q", got, EventKillDeposit)
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 event, got %d", len(out))
	}
	ke, ok := out[0].(KillEvent)
	if !ok {
		t.Fatalf("want KillEvent, got %T", out[0])
	}
	if ke.Action != EventKillDeposit || ke.EventIndex != 2 || ke.ContractID != ev.ContractID {
		t.Errorf("KillEvent wrong: %+v", ke)
	}
}

// All eight kill/unkill kinds are pool-gated (same trust root as
// trade/reserves) — a registered pool matches, a look-alike does not.
func TestMatches_killSwitchPoolGated(t *testing.T) {
	d := NewDecoder()
	pool := makeContractStrkey(t, 0xA1)
	d.reg.Seed(pool, MainnetRouter, 61_000_000)
	unregistered := makeContractStrkey(t, 0xAE)

	for _, topic := range []string{
		TopicSymbolKillDeposit, TopicSymbolUnkillDeposit,
		TopicSymbolKillSwap, TopicSymbolUnkillSwap,
		TopicSymbolKillClaim, TopicSymbolUnkillClaim,
		TopicSymbolKillGaugesClaim, TopicSymbolUnkillGaugesClaim,
	} {
		if !d.Matches(events.Event{ContractID: pool, Topic: []string{topic}}) {
			t.Errorf("kill topic %s from registered pool: Matches=false, want true", topic)
		}
		if d.Matches(events.Event{ContractID: unregistered, Topic: []string{topic}}) {
			t.Errorf("kill topic %s from unregistered contract: Matches=true, want false", topic)
		}
	}
}
