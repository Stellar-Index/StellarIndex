package clickhouse

import (
	"context"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

func fakeLookup(idx map[ledgerTx]uint32, calls *[][]uint32) txIndexLookup {
	return func(_ context.Context, ledgers []uint32) (map[ledgerTx]uint32, error) {
		*calls = append(*calls, append([]uint32(nil), ledgers...))
		return idx, nil
	}
}

func collect(t *testing.T, o *applyOrderer, in []events.Event) {
	t.Helper()
	for _, ev := range in {
		if err := o.add(ev); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	if err := o.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// Q040: the lake streams a ledger by tx_hash, so a pool's first trade whose
// hash sorts lexically before its add_pool arrives first. The orderer must
// deliver the add_pool (applied first on-chain) before the trade.
func TestApplyOrderer_SameLedgerAddPoolPrecedesTrade(t *testing.T) {
	const L = 64_000_000
	trade := events.Event{Ledger: L, TxHash: "a_trade", OperationIndex: 0, EventIndex: 0, Topic: []string{"trade"}}
	addPool := events.Event{Ledger: L, TxHash: "b_add_pool", OperationIndex: 0, EventIndex: 0, Topic: []string{"add_pool"}}
	idx := map[ledgerTx]uint32{{L, "b_add_pool"}: 0, {L, "a_trade"}: 1}

	var calls [][]uint32
	var got []string
	o := newApplyOrderer(context.Background(), fakeLookup(idx, &calls), func(ev events.Event) error {
		got = append(got, ev.TxHash)
		return nil
	})
	collect(t, o, []events.Event{trade, addPool}) // lake (tx_hash) order

	if strings.Join(got, ",") != "b_add_pool,a_trade" {
		t.Fatalf("delivery order = %v, want add_pool before trade (apply order)", got)
	}
}

// Within a tx the lake order (op_index, event_index) is kept, exact duplicate
// rows stay adjacent, ledgers stay ascending and never split across batches.
func TestApplyOrderer_StableAcrossBatches(t *testing.T) {
	idx := map[ledgerTx]uint32{}
	var in []events.Event
	n := uint32(applyOrderLedgerBatch*2 + 3)
	for l := uint32(1); l <= n; l++ {
		idx[ledgerTx{l, "x"}] = 1
		idx[ledgerTx{l, "y"}] = 0
		in = append(in,
			events.Event{Ledger: l, TxHash: "x", OperationIndex: 0, EventIndex: 0},
			events.Event{Ledger: l, TxHash: "x", OperationIndex: 0, EventIndex: 0}, // unmerged duplicate
			events.Event{Ledger: l, TxHash: "x", OperationIndex: 1, EventIndex: 0},
			events.Event{Ledger: l, TxHash: "y", OperationIndex: 0, EventIndex: 2},
		)
	}
	var calls [][]uint32
	var got []events.Event
	o := newApplyOrderer(context.Background(), fakeLookup(idx, &calls), func(ev events.Event) error {
		got = append(got, ev)
		return nil
	})
	collect(t, o, in)

	if len(got) != len(in) {
		t.Fatalf("delivered %d events, want %d", len(got), len(in))
	}
	for i := 0; i < len(got); i += 4 {
		l := got[i].Ledger
		want := []struct {
			h      string
			op, ev int
		}{{"y", 0, 2}, {"x", 0, 0}, {"x", 0, 0}, {"x", 1, 0}}
		for j, w := range want {
			e := got[i+j]
			if e.Ledger != l || e.TxHash != w.h || e.OperationIndex != w.op || e.EventIndex != w.ev {
				t.Fatalf("ledger %d pos %d = %+v, want %+v", l, j, e, w)
			}
		}
		if i > 0 && got[i-1].Ledger >= l {
			t.Fatalf("ledgers not ascending at %d", i)
		}
	}
	seen := map[uint32]bool{}
	for _, c := range calls {
		if len(c) > applyOrderLedgerBatch {
			t.Fatalf("lookup batch of %d ledgers exceeds %d", len(c), applyOrderLedgerBatch)
		}
		for _, l := range c {
			if seen[l] {
				t.Fatalf("ledger %d split across lookup batches", l)
			}
			seen[l] = true
		}
	}
}

func TestApplyOrderer_UnresolvedTxFailsStream(t *testing.T) {
	var calls [][]uint32
	o := newApplyOrderer(context.Background(), fakeLookup(map[ledgerTx]uint32{}, &calls), func(events.Event) error {
		t.Fatal("event delivered with unknown apply order")
		return nil
	})
	if err := o.add(events.Event{Ledger: 7, TxHash: "missing"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := o.flush(); err == nil || !strings.Contains(err.Error(), "apply order unresolved") {
		t.Fatalf("flush err = %v, want apply-order-unresolved error", err)
	}
}

func TestTxIndexByLedgersQuery(t *testing.T) {
	q := txIndexByLedgersQuery([]uint32{5, 9})
	for _, want := range []string{"FROM stellar.transactions", "tx_index", "inner_tx_hash", "ledger_seq IN (5,9)"} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q:\n%s", want, q)
		}
	}
}
