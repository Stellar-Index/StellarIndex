package aquarius

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Real mainnet reserves_sync body captured from the ClickHouse lake
// (pool CCRULRY3…, ledger 61,572,402, tx fbc6b2a0…). Body is a
// Vec<i128> — the SAME shape as update_reserves, so decodeReserves is
// reused with the EventReservesSync kind. Verified distinct from
// update_reserves on the same tx (see migration 0128 header).
const (
	realReservesSyncTopic0 = "AAAADwAAAA1yZXNlcnZlc19zeW5jAAAA" // Symbol("reserves_sync")
	realReservesSyncBody   = "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAAAApcwHiMAAAAKAAAAAAAAAAAAAAACmjvpKg=="
)

func TestDecodeReservesSync_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID:     "CCRULRY3VV6NVQHZ43KDKC75OHR6YEH7WCQBDTTU6JBIH75NC72VW6JJ",
		Ledger:         61_572_402,
		TxHash:         "fbc6b2a01d4c63eb5b6f2025b39d16895c778a816fb82967421596e794e65de9",
		OperationIndex: 0,
		EventIndex:     11,
		Topic:          []string{realReservesSyncTopic0},
		Value:          realReservesSyncBody,
	}
	// classify must resolve the reserves_sync topic, or the decoder
	// never fires on a real event.
	if got := classify(e); got != EventReservesSync {
		t.Fatalf("classify real reserves_sync = %q, want %q", got, EventReservesSync)
	}
	rv, err := decodeReserves(e, closedAtTest, EventReservesSync)
	if err != nil {
		t.Fatalf("decodeReserves: %v", err)
	}
	if rv.Kind != EventReservesSync {
		t.Errorf("Kind = %q, want %q", rv.Kind, EventReservesSync)
	}
	want := []string{"11126447651", "11177552170"}
	if len(rv.Reserves) != len(want) {
		t.Fatalf("reserves len = %d, want %d", len(rv.Reserves), len(want))
	}
	for i, w := range want {
		if rv.Reserves[i].String() != w {
			t.Errorf("reserve[%d] = %s, want %s", i, rv.Reserves[i], w)
		}
	}
	if rv.ContractID != e.ContractID || rv.Ledger != e.Ledger || rv.EventIndex != 11 {
		t.Errorf("identity mismatch: %+v", rv)
	}
}

// A registered pool's reserves_sync must Match (pool-gated, same as
// update_reserves) — a look-alike from an unregistered contract must not.
func TestMatches_reservesSyncPoolGated(t *testing.T) {
	d := NewDecoder()
	pool := makeContractStrkey(t, 0xC1) // seed → registered
	d.reg.Seed(pool, MainnetRouter, 61_572_402)
	unregistered := makeContractStrkey(t, 0xEE) // never seeded

	ev := events.Event{ContractID: pool, Topic: []string{realReservesSyncTopic0}, Value: realReservesSyncBody}
	if !d.Matches(ev) {
		t.Error("reserves_sync from a registered pool: Matches=false, want true")
	}
	foreign := events.Event{ContractID: unregistered, Topic: []string{realReservesSyncTopic0}, Value: realReservesSyncBody}
	if d.Matches(foreign) {
		t.Error("reserves_sync from an unregistered contract: Matches=true, want false (injection guard)")
	}
}
