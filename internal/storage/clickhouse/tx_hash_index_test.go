package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// stubRows is a minimal driver.Rows over in-memory rows. The embedded
// interface panics on any method the fast path should never touch.
//
// streamErr, when set, is what Err() reports once the rows are exhausted —
// modelling a stream that TRUNCATES mid-flight (a dropped connection, a
// server-side memory limit). A reader that returns rows.Err()'s nil-by-default
// without checking it reports a partial result as a complete one, which for a
// backfill or reconcile is a silent under-count; leaving this field zero is
// byte-for-byte the original always-nil behaviour.
type stubRows struct {
	driver.Rows
	data      [][]any
	i         int
	streamErr error
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
func (r *stubRows) Err() error   { return r.streamErr }

// stubConn records every query (and its bound args, positionally aligned with
// queries) and routes it to a per-shape responder. The embedded driver.Conn
// panics on anything but Query — TransactionByHash must only ever Query.
type stubConn struct {
	driver.Conn
	queries []string
	args    [][]any
	respond func(query string) (driver.Rows, error)
}

func (c *stubConn) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	c.args = append(c.args, args)
	return c.respond(query)
}

// stubRow is the single-row analogue of stubRows for readers that use
// conn.QueryRow (e.g. ContractActivitySummaryFor's bounds/total). It routes
// through the same per-shape responder and scans the FIRST returned row.
type stubRow struct {
	driver.Row
	data []any
	err  error
}

func (r *stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.data) {
		return fmt.Errorf("stub scan: %d dests for %d columns", len(dest), len(r.data))
	}
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(r.data[i]))
	}
	return nil
}

func (r *stubRow) Err() error { return r.err }

func (c *stubConn) QueryRow(_ context.Context, query string, args ...any) driver.Row {
	c.queries = append(c.queries, query)
	c.args = append(c.args, args)
	rows, err := c.respond(query)
	if err != nil {
		return &stubRow{err: err}
	}
	sr, ok := rows.(*stubRows)
	if !ok || len(sr.data) == 0 {
		return &stubRow{err: sql.ErrNoRows}
	}
	return &stubRow{data: sr.data[0]}
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

// probeHit is the availability probe's answer for a healthy, NON-EMPTY
// index: `SELECT ledger_seq … LIMIT 1` returning one row. The probe
// requires a row (requireRows) — an empty result means the index is
// treated as unavailable (see TestTransactionByHashEmptyIndexFallsBackToScan).
func probeHit() *stubRows {
	return &stubRows{data: [][]any{{uint32(1)}}}
}

func TestTransactionByHashFastPath(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isIndexProbe(q):
			return probeHit(), nil
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

func TestTransactionByHashIndexMissIsAuthoritativeNotFound(t *testing.T) {
	// 2026-07-30 (account-filter class audit): the NON-EMPTY index covers
	// genesis→tip, so an INDEX miss is an authoritative "no such hash" —
	// the reader must NOT fall through to the 10.5B-row bloom scan
	// (which turned every garbage hash into an unauthenticated
	// multi-second probe). The scan remains reachable only for an
	// index-path ERROR, an index/base inconsistency, or an EMPTY index
	// (see the sibling tests).
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isIndexProbe(q):
			return probeHit(), nil // the index has rows: misses carry authority
		case isIndexLookup(q):
			return &stubRows{}, nil // no index row: the hash does not exist
		case isBloomScan(q):
			return nil, fmt.Errorf("bloom scan must not run on an authoritative index miss: %s", q)
		default:
			return nil, fmt.Errorf("unexpected query: %s", q)
		}
	}
	r := &ExplorerReader{conn: conn}

	_, found, err := r.TransactionByHash(context.Background(), testTxHash)
	if err != nil || found {
		t.Fatalf("TransactionByHash = (found=%v, err=%v), want authoritative not-found with no error", found, err)
	}
	if n := countQueries(conn.queries, isBloomScan); n != 0 {
		t.Fatalf("index miss issued %d bloom scan(s); want 0 (queries: %v)", n, conn.queries)
	}
}

func TestTransactionByHashIndexTableAbsent(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isIndexProbe(q):
			// A ClickHouse SERVER exception — the shape clickhouse-go
			// actually returns (*clickhouse.Exception). It is what makes
			// the absence DEFINITIVE, so the probe caches it and stops
			// asking; a bare error would mean "no answer" and re-probe
			// (C1-048).
			return nil, &clickhouse.Exception{
				Code: 60, Name: "UNKNOWN_TABLE",
				Message: "Table stellar.tx_hash_index does not exist",
			}
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

func TestTransactionByHashEmptyIndexFallsBackToScan(t *testing.T) {
	// The MV-drop / TRUNCATE pathology (CI-red 2026-07-30): the index table
	// EXISTS but is EMPTY while stellar.transactions keeps flowing. If the
	// probe granted authority off mere existence, every real hash would 404
	// authoritatively. An empty index must instead read as index-UNAVAILABLE:
	// the lookup takes the bloom-scan path and the index is never consulted.
	// Emptiness is also NOT a settled verdict — the index may be repopulated
	// (backfill / MV re-attach), so a later call must re-probe and pick it
	// back up (same "empty table is not a definitive answer" convention as
	// DailyActivityAvailable).
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isIndexProbe(q):
			return &stubRows{}, nil // table exists, ZERO rows
		case isIndexLookup(q):
			return nil, fmt.Errorf("the empty index must not be consulted: %s", q)
		case isBloomScan(q):
			return &stubRows{data: [][]any{{uint32(64_000_000)}}}, nil
		case isLedgerScopedRead(q):
			return &stubRows{data: [][]any{txRowFor(64_000_000, testTxHash)}}, nil
		default:
			return nil, fmt.Errorf("unexpected query: %s", q)
		}
	}
	r := &ExplorerReader{conn: conn, txIndexProbe: schemaProbe{retryAfter: -1}}

	for i := range 2 {
		tx, found, err := r.TransactionByHash(context.Background(), testTxHash)
		if err != nil || !found {
			t.Fatalf("call %d: TransactionByHash = (found=%v, err=%v), want scan hit despite empty index", i, found, err)
		}
		if tx.Seq != 64_000_000 {
			t.Fatalf("call %d: tx.Seq = %d, want 64000000 (via the bloom scan)", i, tx.Seq)
		}
	}
	if n := countQueries(conn.queries, isIndexLookup); n != 0 {
		t.Fatalf("index lookups = %d, want 0 — an empty index must not answer (queries: %v)", n, conn.queries)
	}
	if n := countQueries(conn.queries, isBloomScan); n != 2 {
		t.Fatalf("bloom scans = %d, want 2 (one per lookup)", n)
	}
	// Emptiness must not latch: with the negative cache disabled, the second
	// lookup re-probed rather than trusting a cached "unavailable".
	if n := countQueries(conn.queries, isIndexProbe); n != 2 {
		t.Fatalf("probes = %d, want 2 — empty is not a settled verdict; the index must be re-probed", n)
	}
}

func TestTransactionByHashRepopulatedIndexRegainsAuthority(t *testing.T) {
	// The recovery half of the emptiness probe: once the truncated index is
	// repopulated, a re-probe finds rows, settles true, and a per-hash miss
	// is authoritative again (no bloom scan).
	conn := &stubConn{}
	probeCalls := 0
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isIndexProbe(q):
			probeCalls++
			if probeCalls == 1 {
				return &stubRows{}, nil // still empty
			}
			return probeHit(), nil // repopulated
		case isIndexLookup(q):
			return &stubRows{}, nil // hash genuinely absent
		case isBloomScan(q):
			return &stubRows{}, nil // scan path (first call only): unknown hash
		default:
			return nil, fmt.Errorf("unexpected query: %s", q)
		}
	}
	r := &ExplorerReader{conn: conn, txIndexProbe: schemaProbe{retryAfter: -1}}

	if _, found, err := r.TransactionByHash(context.Background(), testTxHash); err != nil || found {
		t.Fatalf("call 1 (empty index) = (found=%v, err=%v), want scan-path not-found", found, err)
	}
	if _, found, err := r.TransactionByHash(context.Background(), testTxHash); err != nil || found {
		t.Fatalf("call 2 (repopulated) = (found=%v, err=%v), want authoritative not-found", found, err)
	}
	if n := countQueries(conn.queries, isBloomScan); n != 1 {
		t.Fatalf("bloom scans = %d, want 1 — after repopulation the index miss is authoritative again", n)
	}
	if n := countQueries(conn.queries, isIndexLookup); n != 1 {
		t.Fatalf("index lookups = %d, want 1 (second call only)", n)
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
			return probeHit(), nil
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
