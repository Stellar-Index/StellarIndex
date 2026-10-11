package timescale

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestSortTradesByConflictKey_FullKeyOrder guards the CEX batch-insert deadlock: concurrent
// BatchInsertTrades callers with overlapping trades PK rows must take Postgres row locks in the
// SAME order, or they AB/BA deadlock (40P01). That needs the FULL conflict key
// (source, ledger, tx_hash, op_index, ts), not a prefix.
//
// The batch ties on the first four columns (the shape of off-chain CEX/FX rows, which share
// ledger=0) and differs only on ts; shuffled many times, the sorted order must be identical,
// so ts is a real tiebreaker and not sort.Slice's unspecified order for ties.
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

func itoa(u uint32) string {
	return fmt.Sprintf("%d", u)
}
