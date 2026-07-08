package timescale

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// TestSortTradesByConflictKey_FullKeyOrder is the regression test for
// the 2026-07-08 CEX batch-insert deadlock storm: two concurrent
// `BatchInsertTrades` callers whose batches share overlapping trades
// PK rows must acquire Postgres row locks in the SAME order, or a
// same-keys-different-order lock acquisition is a textbook AB/BA
// (40P01) deadlock. That requires sorting by the FULL conflict key —
// `(source, ledger, tx_hash, op_index, ts)`, matching `ON CONFLICT
// (source, ledger, tx_hash, op_index, ts)` in BatchInsertTrades — not
// just a prefix of it.
//
// This test builds a batch containing rows that tie on the first four
// columns (source, ledger, tx_hash, op_index — the exact shape of
// off-chain CEX/FX rows, which all share ledger=0 and often op_index=0)
// but differ on `ts`, the fifth column. It shuffles the input many
// times and asserts the sorted output order is IDENTICAL every time —
// proving `ts` is used as a real tiebreaker rather than left to
// sort.Slice's unspecified (non-stable) order for tied elements, which
// is exactly the gap the 2026-07-05 fix left open.
func TestSortTradesByConflictKey_FullKeyOrder(t *testing.T) {
	base := time.Date(2026, 7, 8, 18, 12, 0, 0, time.UTC)
	mk := func(source string, ledger uint32, txHash string, opIndex uint32, tsOffsetSeconds int) canonical.Trade {
		return canonical.Trade{
			Source:    source,
			Ledger:    ledger,
			TxHash:    txHash,
			OpIndex:   opIndex,
			Timestamp: base.Add(time.Duration(tsOffsetSeconds) * time.Second),
		}
	}

	// Rows that tie on (source, ledger, tx_hash, op_index) — the
	// CEX-shaped collision — but differ on ts. Includes multiple
	// distinct sources/tx_hash values too, so the test also exercises
	// the pre-existing prefix ordering.
	want := []canonical.Trade{
		mk("kraken", 0, "aaaa", 0, 0),
		mk("kraken", 0, "aaaa", 0, 1),
		mk("kraken", 0, "aaaa", 0, 2),
		mk("kraken", 0, "bbbb", 0, 0),
		mk("kraken", 0, "bbbb", 0, 5),
		mk("sdex", 100, "cccc", 0, 0),
		mk("sdex", 100, "cccc", 1, 0),
		mk("sdex", 200, "cccc", 0, 0),
	}

	keyOf := func(tr canonical.Trade) [5]string {
		return [5]string{tr.Source, itoa(tr.Ledger), tr.TxHash, itoa(tr.OpIndex), tr.Timestamp.String()}
	}
	wantKeys := make([][5]string, len(want))
	for i, tr := range want {
		wantKeys[i] = keyOf(tr)
	}

	rng := rand.New(rand.NewSource(1))
	for trial := range 50 {
		shuffled := append([]canonical.Trade(nil), want...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		sortTradesByConflictKey(shuffled)

		if len(shuffled) != len(wantKeys) {
			t.Fatalf("trial %d: length changed: got %d, want %d", trial, len(shuffled), len(wantKeys))
		}
		for i, tr := range shuffled {
			if got := keyOf(tr); got != wantKeys[i] {
				t.Fatalf("trial %d: position %d: got key %v, want %v (non-deterministic sort — ts tiebreak missing)",
					trial, i, got, wantKeys[i])
			}
		}
	}
}

// TestSortTradesByConflictKey_TimestampTiebreak isolates the exact
// bug: two rows identical on (source, ledger, tx_hash, op_index) must
// sort by `ts` ascending, not fall through to an unspecified order.
func TestSortTradesByConflictKey_TimestampTiebreak(t *testing.T) {
	later := time.Date(2026, 7, 8, 18, 12, 5, 0, time.UTC)
	earlier := time.Date(2026, 7, 8, 18, 12, 0, 0, time.UTC)

	trades := []canonical.Trade{
		{Source: "kraken", Ledger: 0, TxHash: "same", OpIndex: 0, Timestamp: later},
		{Source: "kraken", Ledger: 0, TxHash: "same", OpIndex: 0, Timestamp: earlier},
	}
	sortTradesByConflictKey(trades)

	if !trades[0].Timestamp.Equal(earlier) || !trades[1].Timestamp.Equal(later) {
		t.Fatalf("expected ts-ascending tiebreak, got order [%v, %v]", trades[0].Timestamp, trades[1].Timestamp)
	}
}

func itoa(u uint32) string {
	return fmt.Sprintf("%d", u)
}
