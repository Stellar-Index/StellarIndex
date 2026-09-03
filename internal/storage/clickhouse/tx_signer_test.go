package clickhouse

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TestTxSignersForLedgerRange_ReadsTheWindowInBindOrder pins the shape the
// signer back-tagger depends on.
//
// Why each fragment is load-bearing:
//
//   - `stellar.transactions FINAL`: the table is a ReplacingMergeTree, and a
//     re-ingest (retry, or a decode-fix re-derive) can leave un-merged
//     duplicate parts for one (ledger_seq, tx_index). Without FINAL the sweep
//     can pick up a STALE pre-correction source_account and stamp it onto the
//     trade as its signer.
//   - `ledger_seq >= ? AND ledger_seq <= ?`: ledger_seq is the table's primary
//     key, so this is an index-pruned range read. A function-wrapped or
//     reordered predicate turns a few-minute sweep into a full-table scan.
//   - the non-empty source_account predicate: a blank signer is not a signer;
//     writing it would overwrite a real one with nothing.
func TestTxSignersForLedgerRange_ReadsTheWindowInBindOrder(t *testing.T) {
	conn := &stubConn{respond: func(string) (driver.Rows, error) { return &stubRows{}, nil }}
	r := &ExplorerReader{conn: conn}

	if _, err := r.TxSignersForLedgerRange(t.Context(), 60_000_000, 60_000_100); err != nil {
		t.Fatalf("TxSignersForLedgerRange: %v", err)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("issued %d queries, want 1", len(conn.queries))
	}
	q := conn.queries[0]
	for _, s := range []string{
		"stellar.transactions FINAL",
		"ledger_seq >= ?",
		"ledger_seq <= ?",
		"source_account != ''",
	} {
		if !strings.Contains(q, s) {
			t.Errorf("TxSignersForLedgerRange query missing %q:\n%s", s, q)
		}
	}
	// Clause order must match the (min, max) bind order — a swapped pair
	// reads an empty range and the sweep silently tags nothing.
	lo, hi := strings.Index(q, "ledger_seq >= ?"), strings.Index(q, "ledger_seq <= ?")
	if lo > hi {
		t.Errorf("the >= clause is spelled after the <= clause but args bind (min,max):\n%s", q)
	}
	if got, want := conn.args[0], []any{uint32(60_000_000), uint32(60_000_100)}; !reflect.DeepEqual(got, want) {
		t.Errorf("bound args = %v, want %v", got, want)
	}
	// The four projected columns, in the order Scan reads them.
	if !strings.Contains(q, "SELECT ledger_seq, tx_hash, source_account, close_time") {
		t.Errorf("projection reordered — Scan is positional:\n%s", q)
	}
}

// TestTxSignersForLedgerRange_InvertedRangeIsANoOpNotAQuery — the sweeper
// derives its range from the set of AMM trades still needing a signer; when
// that set is empty the derived max can fall below the min. Issuing the query
// anyway would read the whole table backwards (an empty but unbounded scan).
func TestTxSignersForLedgerRange_InvertedRangeIsANoOpNotAQuery(t *testing.T) {
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		t.Fatalf("an inverted range must not reach ClickHouse; query was: %s", q)
		return nil, nil
	}}
	r := &ExplorerReader{conn: conn}

	got, err := r.TxSignersForLedgerRange(t.Context(), 100, 99)
	if err != nil {
		t.Fatalf("TxSignersForLedgerRange(inverted) = %v, want a nil-error no-op", err)
	}
	if got != nil {
		t.Errorf("result = %v, want nil", got)
	}
}

// TestTxSignersForLedgerRange_ScansEveryRow.
func TestTxSignersForLedgerRange_ScansEveryRow(t *testing.T) {
	at := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	conn := &stubConn{respond: func(string) (driver.Rows, error) {
		return &stubRows{data: [][]any{
			{uint32(10), "aa", testSource, at},
			{uint32(11), "bb", testDest, at},
		}}, nil
	}}
	r := &ExplorerReader{conn: conn}

	got, err := r.TxSignersForLedgerRange(t.Context(), 10, 11)
	if err != nil {
		t.Fatalf("TxSignersForLedgerRange: %v", err)
	}
	want := []TxSigner{
		{Ledger: 10, TxHash: "aa", Signer: testSource, CloseTime: at},
		{Ledger: 11, TxHash: "bb", Signer: testDest, CloseTime: at},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("signers = %+v, want %+v", got, want)
	}
}

// TestTxSignersForLedgerRange_TruncatedStreamIsAnError — a partial signer set
// would leave the sweeper's remaining trades untagged while it advanced past
// them, so a truncated read must not look like "no more signers in range".
func TestTxSignersForLedgerRange_TruncatedStreamIsAnError(t *testing.T) {
	truncated := errors.New("read timed out mid-stream")
	conn := &stubConn{respond: func(string) (driver.Rows, error) {
		return &stubRows{
			data:      [][]any{{uint32(10), "aa", testSource, time.Unix(0, 0).UTC()}},
			streamErr: truncated,
		}, nil
	}}
	r := &ExplorerReader{conn: conn}
	if _, err := r.TxSignersForLedgerRange(t.Context(), 10, 11); !errors.Is(err, truncated) {
		t.Fatalf("err = %v, want it to wrap %v", err, truncated)
	}
}
