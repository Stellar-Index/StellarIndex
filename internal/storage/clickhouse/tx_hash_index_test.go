package clickhouse

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// stubRows is a minimal driver.Rows over in-memory rows. The embedded
// interface panics on any method the fast path should never touch.
type stubRows struct {
	driver.Rows
	data [][]any
	i    int
}

func (r *stubRows) Next() bool {
	if r.i >= len(r.data) {
		return false
	}
	r.i++
	return true
}

func (r *stubRows) Scan(dest ...any) error {
	row := r.data[r.i-1]
	if len(dest) != len(row) {
		return fmt.Errorf("stub scan: %d dests for %d columns", len(dest), len(row))
	}
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(row[i]))
	}
	return nil
}

func (r *stubRows) Close() error { return nil }
func (r *stubRows) Err() error   { return nil }

// stubConn records every query and routes it to a per-shape responder. The
// embedded driver.Conn panics on anything but Query — TransactionByHash must
// only ever Query.
type stubConn struct {
	driver.Conn
	queries []string
	respond func(query string) (driver.Rows, error)
}

func (c *stubConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	return c.respond(query)
}

// Query-shape classifiers (match the exact tables/clauses the reader emits).
func isIndexProbe(q string) bool {
	return strings.Contains(q, "stellar.tx_hash_index") && !strings.Contains(q, "WHERE")
}

func isIndexLookup(q string) bool {
	return strings.Contains(q, "stellar.tx_hash_index") && strings.Contains(q, "WHERE tx_hash = ?")
}

func isLedgerScopedRead(q string) bool {
	return strings.Contains(q, "stellar.transactions") && strings.Contains(q, "WHERE ledger_seq = ? AND tx_hash = ?")
}

func isBloomScan(q string) bool {
	return strings.Contains(q, "stellar.transactions") && strings.Contains(q, "WHERE tx_hash = ?") &&
		!strings.Contains(q, "ledger_seq = ?")
}

func countQueries(qs []string, match func(string) bool) int {
	n := 0
	for _, q := range qs {
		if match(q) {
			n++
		}
	}
	return n
}

// txRowFor builds the 12-column stellar.transactions row scanTxSummaries expects.
func txRowFor(seq uint32, hash string) []any {
	return []any{
		seq, time.Unix(1700000000, 0).UTC(), hash, uint32(3), "GSOURCE",
		int64(100), int64(200), uint16(1), uint8(1), int32(0), "text", "hi",
	}
}

const testTxHash = "ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34"

func TestTransactionByHashFastPath(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isIndexProbe(q):
			return &stubRows{}, nil
		case isIndexLookup(q):
			return &stubRows{data: [][]any{{uint32(62_000_001)}}}, nil
		case isLedgerScopedRead(q):
			return &stubRows{data: [][]any{txRowFor(62_000_001, testTxHash)}}, nil
		default:
			return nil, fmt.Errorf("unexpected query: %s", q)
		}
	}
	r := &ExplorerReader{conn: conn}

	tx, found, err := r.TransactionByHash(context.Background(), testTxHash)
	if err != nil || !found {
		t.Fatalf("TransactionByHash = (found=%v, err=%v), want hit", found, err)
	}
	if tx.Seq != 62_000_001 || tx.TxHash != testTxHash || !tx.Successful {
		t.Fatalf("unexpected summary: %+v", tx)
	}
	if n := countQueries(conn.queries, isBloomScan); n != 0 {
		t.Fatalf("fast path issued %d bloom scan(s); want 0 (queries: %v)", n, conn.queries)
	}
}

func TestTransactionByHashIndexMissFallsBackToScan(t *testing.T) {
	// txByHashScan is now TWO steps (audit DAT-10, deterministic ingested_at
	// tiebreak): step 1 (isBloomScan) only locates the ledger; step 2
	// (isLedgerScopedRead, FINAL) reads the authoritative row.
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isIndexProbe(q):
			return &stubRows{}, nil
		case isIndexLookup(q):
			return &stubRows{}, nil // pre-backfill history: not in the index
		case isBloomScan(q):
			return &stubRows{data: [][]any{{uint32(50_000_000)}}}, nil // step 1: locate the ledger only
		case isLedgerScopedRead(q):
			return &stubRows{data: [][]any{txRowFor(50_000_000, testTxHash)}}, nil // step 2: FINAL point read
		default:
			return nil, fmt.Errorf("unexpected query: %s", q)
		}
	}
	r := &ExplorerReader{conn: conn}

	tx, found, err := r.TransactionByHash(context.Background(), testTxHash)
	if err != nil || !found {
		t.Fatalf("TransactionByHash = (found=%v, err=%v), want scan fallback hit", found, err)
	}
	if tx.Seq != 50_000_000 {
		t.Fatalf("tx.Seq = %d, want the scan's 50000000", tx.Seq)
	}
	if n := countQueries(conn.queries, isBloomScan); n != 1 {
		t.Fatalf("scan fallback issued %d bloom scan(s); want 1 (queries: %v)", n, conn.queries)
	}
	if n := countQueries(conn.queries, isLedgerScopedRead); n != 1 {
		t.Fatalf("scan fallback issued %d ledger-scoped FINAL read(s); want 1 (queries: %v)", n, conn.queries)
	}
}

func TestTransactionByHashIndexTableAbsent(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isIndexProbe(q):
			return nil, fmt.Errorf("code: 60, UNKNOWN_TABLE") // table absent on this deployment
		case isBloomScan(q):
			return &stubRows{}, nil // unknown hash
		default:
			return nil, fmt.Errorf("unexpected query: %s", q)
		}
	}
	r := &ExplorerReader{conn: conn}

	for i := range 2 { // second call must NOT re-probe (probe-once)
		_, found, err := r.TransactionByHash(context.Background(), testTxHash)
		if err != nil || found {
			t.Fatalf("call %d: TransactionByHash = (found=%v, err=%v), want clean not-found", i, found, err)
		}
	}
	if n := countQueries(conn.queries, isIndexProbe); n != 1 {
		t.Fatalf("probe ran %d times across two lookups; want once", n)
	}
	if n := countQueries(conn.queries, isIndexLookup); n != 0 {
		t.Fatalf("index lookup issued despite absent table (queries: %v)", conn.queries)
	}
	if n := countQueries(conn.queries, isBloomScan); n != 2 {
		t.Fatalf("bloom scans = %d, want 2 (one per lookup)", n)
	}
}

func TestTransactionByHashIndexRowWithoutBaseRowFallsBack(t *testing.T) {
	// An index row whose ledger-scoped read comes up empty (shouldn't happen,
	// but e.g. a partial re-derive) must fall through to the scan rather than
	// report not-found off the index alone. Both txByHashIndexed's read AND
	// the scan fallback's step 2 share the SAME isLedgerScopedRead query
	// shape (txByLedgerAndHash) — a counter tells the first (empty, forcing
	// the fallback) from the second (the scan fallback's real answer) apart.
	conn := &stubConn{}
	lecCalls := 0
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isIndexProbe(q):
			return &stubRows{}, nil
		case isIndexLookup(q):
			return &stubRows{data: [][]any{{uint32(61_000_000)}}}, nil
		case isLedgerScopedRead(q):
			lecCalls++
			if lecCalls == 1 {
				return &stubRows{}, nil // txByHashIndexed's read: base row missing
			}
			return &stubRows{data: [][]any{txRowFor(61_000_000, testTxHash)}}, nil // scan fallback's step 2
		case isBloomScan(q):
			return &stubRows{data: [][]any{{uint32(61_000_000)}}}, nil // scan fallback's step 1: locate the ledger
		default:
			return nil, fmt.Errorf("unexpected query: %s", q)
		}
	}
	r := &ExplorerReader{conn: conn}

	_, found, err := r.TransactionByHash(context.Background(), testTxHash)
	if err != nil || !found {
		t.Fatalf("TransactionByHash = (found=%v, err=%v), want scan fallback hit", found, err)
	}
	if n := countQueries(conn.queries, isBloomScan); n != 1 {
		t.Fatalf("bloom scans = %d, want 1 (queries: %v)", n, conn.queries)
	}
	if lecCalls != 2 {
		t.Fatalf("ledger-scoped reads = %d, want 2 (indexed path + scan-fallback step 2)", lecCalls)
	}
}

// TestTxByLedgerAndHash_FinalNotIngestedAtTiebreak proves the DAT-10 fix:
// the ledger-scoped read that both txByHashIndexed and txByHashScan's step 2
// share is FINAL-deduped, not an `ORDER BY ingested_at DESC LIMIT 1` — which
// silently picked an UNSPECIFIED row whenever two ReplacingMergeTree parts
// for the same key tied on ingested_at (DateTime is one-second resolution).
// Proven red: reverting explorer_reader.go's txByLedgerAndHash to the old
// `ORDER BY ingested_at DESC LIMIT 1` query makes this test fail (see the
// fixer's remediation notes) — this test pins the corrected query text.
func TestTxByLedgerAndHash_FinalNotIngestedAtTiebreak(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if isLedgerScopedRead(q) {
			return &stubRows{data: [][]any{txRowFor(70_000_000, testTxHash)}}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", q)
	}
	r := &ExplorerReader{conn: conn}

	tx, found, err := r.txByLedgerAndHash(context.Background(), 70_000_000, testTxHash)
	if err != nil || !found {
		t.Fatalf("txByLedgerAndHash = (found=%v, err=%v), want hit", found, err)
	}
	if tx.Seq != 70_000_000 {
		t.Fatalf("tx.Seq = %d, want 70000000", tx.Seq)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("issued %d queries, want 1", len(conn.queries))
	}
	q := conn.queries[0]
	if !strings.Contains(q, "stellar.transactions FINAL") {
		t.Fatalf("query = %q, want a `stellar.transactions FINAL` scoped read", q)
	}
	if strings.Contains(q, "ingested_at") {
		t.Fatalf("query = %q, must NOT tiebreak on ingested_at (DAT-10: DateTime ties pick an unspecified row)", q)
	}
}

// TestTxHashIndexBackfillQuery_UsesFinal proves the DAT-10 fix on
// BackfillTxHashIndex's per-window INSERT…SELECT: it must read
// stellar.transactions FINAL-deduped so a re-derive backfill window doesn't
// enqueue an un-merged duplicate (or, worse, a stale pre-correction) row into
// stellar.tx_hash_index. BackfillTxHashIndex dials a real ClickHouse
// connection via openRead (not an injectable field), so — like
// StreamEntryChanges/CountOpScopedEntryChanges — it cannot be driven
// end-to-end with the stubConn harness in this package; the query was
// extracted to this package-level const specifically so its SQL text stays
// independently testable.
func TestTxHashIndexBackfillQuery_UsesFinal(t *testing.T) {
	if !strings.Contains(txHashIndexBackfillQuery, "FROM stellar.transactions FINAL") {
		t.Fatalf("txHashIndexBackfillQuery = %q, want `FROM stellar.transactions FINAL`", txHashIndexBackfillQuery)
	}
	// The window predicate that bounds FINAL's cost must survive unchanged.
	if !strings.Contains(txHashIndexBackfillQuery, "WHERE ledger_seq >= ? AND ledger_seq <= ?") {
		t.Fatalf("txHashIndexBackfillQuery = %q, want the ledger-window predicate preserved", txHashIndexBackfillQuery)
	}
}
