package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// fakeClock is a hand-advanced clock for schemaProbe.now, so a lease can be
// run out without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
}

// liveTxLake models ONE ClickHouse under ONE long-lived reader: a base
// stellar.transactions table that always holds the transaction, and a
// stellar.tx_hash_index whose contents the test flips between populated,
// emptied (TRUNCATE / MV drop) and dropped (the DROP half of the
// DROP+recreate idiom). probeErr, when set, is what the availability probe
// alone returns — the store refusing to answer about the index.
type liveTxLake struct {
	indexRows   bool
	indexExists bool
	probeErr    error
}

func (l *liveTxLake) respond(q string) (driver.Rows, error) {
	unknownTable := &clickhouse.Exception{
		Code: 60, Name: "UNKNOWN_TABLE",
		Message: "Table stellar.tx_hash_index does not exist",
	}
	switch {
	case isIndexProbe(q):
		switch {
		case l.probeErr != nil:
			return nil, l.probeErr
		case !l.indexExists:
			return nil, unknownTable
		case l.indexRows:
			return probeHit(), nil
		}
		return &stubRows{}, nil
	case isIndexLookup(q):
		switch {
		case !l.indexExists:
			return nil, unknownTable
		case l.indexRows:
			return &stubRows{data: [][]any{{uint32(62_000_001)}}}, nil
		}
		return &stubRows{}, nil // emptied: every hash misses
	case isLedgerScopedRead(q):
		return &stubRows{data: [][]any{txRowFor(62_000_001, testTxHash)}}, nil
	case isBloomScan(q):
		return &stubRows{data: [][]any{{uint32(62_000_001)}}}, nil
	default:
		return nil, fmt.Errorf("unexpected query: %s", q)
	}
}

// TestTransactionByHash_IndexEmptiedUnderLiveReaderStopsBeingAuthoritative
// pins F119. probeSchema used to LATCH a positive verdict for the process
// lifetime, so the requireRows guard protected only a cold start: an
// operator TRUNCATEing (or DROP+recreating) stellar.tx_hash_index ahead of
// a ch-txindex-backfill left every already-running API process holding
// settled=true/present=true, every index lookup missed, and every miss was
// served as an AUTHORITATIVE 404 for a transaction that exists — until a
// restart. The verdict is now a lease: the SAME reader, with no restart,
// must stop treating misses as authoritative once the lease runs out.
func TestTransactionByHash_IndexEmptiedUnderLiveReaderStopsBeingAuthoritative(t *testing.T) {
	lake := &liveTxLake{indexExists: true, indexRows: true}
	conn := &stubConn{respond: lake.respond}
	clk := newFakeClock()
	r := &ExplorerReader{conn: conn, txIndexProbe: schemaProbe{now: clk.now}}
	ctx := context.Background()

	// Populated index: the fast path serves the hit and settles the probe.
	if _, found, err := r.TransactionByHash(ctx, testTxHash); err != nil || !found {
		t.Fatalf("populated index: (found=%v, err=%v), want hit", found, err)
	}
	if n := countQueries(conn.queries, isBloomScan); n != 0 {
		t.Fatalf("populated index: %d bloom scan(s), want 0", n)
	}

	// The index EMPTIES under the live reader; the transaction still exists
	// in stellar.transactions.
	lake.indexRows = false
	clk.advance(schemaProbeLease + time.Second)

	tx, found, err := r.TransactionByHash(ctx, testTxHash)
	if err != nil {
		t.Fatalf("emptied index: err = %v", err)
	}
	if !found {
		t.Fatalf("emptied index: found=false — a REAL transaction was served as an authoritative 404 "+
			"because the positive probe verdict is latched for the process lifetime (queries: %v)", conn.queries)
	}
	if tx.Seq != 62_000_001 || tx.TxHash != testTxHash {
		t.Fatalf("emptied index: unexpected summary %+v, want seq 62000001 via the scan", tx)
	}
	if n := countQueries(conn.queries, isBloomScan); n != 1 {
		t.Fatalf("emptied index: bloom scans = %d, want 1 — the miss must fall to the scan", n)
	}
	if n := countQueries(conn.queries, isIndexLookup); n != 1 {
		t.Fatalf("index lookups = %d, want 1 — the emptied index must not be consulted at all", n)
	}
	if n := countQueries(conn.queries, isIndexProbe); n != 2 {
		t.Fatalf("probes = %d, want 2 (cold start + one lease renewal)", n)
	}

	// Repopulated (backfill finished): the reader picks the index back up,
	// again with no restart, once the empty verdict's back-off has passed.
	lake.indexRows = true
	clk.advance(schemaProbeRetryAfter + time.Second)
	if _, found, err := r.TransactionByHash(ctx, testTxHash); err != nil || !found {
		t.Fatalf("repopulated index: (found=%v, err=%v), want hit", found, err)
	}
	if n := countQueries(conn.queries, isBloomScan); n != 1 {
		t.Fatalf("repopulated index: bloom scans = %d, want still 1 — the fast path must be back", n)
	}
}

// TestProbeSchema_PositiveLeaseIsNotAPerRequestProbe is the cost half of
// F119: the lease must not put a probe on every request. Inside one lease
// window any number of reads share ONE probe; each further window costs
// exactly one more.
func TestProbeSchema_PositiveLeaseIsNotAPerRequestProbe(t *testing.T) {
	conn := &probeConn{}
	clk := newFakeClock()
	r := &ExplorerReader{conn: conn, txIndexProbe: schemaProbe{now: clk.now}}
	ctx := context.Background()

	for i := range 1000 {
		if !r.txHashIndexAvailable(ctx) {
			t.Fatalf("read %d = false, want true", i)
		}
		if i == 499 {
			clk.advance(schemaProbeLease - time.Second) // still inside the lease
		}
	}
	if conn.calls != 1 {
		t.Fatalf("conn.Query called %d times across 1000 reads inside one lease, want 1", conn.calls)
	}

	clk.advance(2 * time.Second) // the lease has now run out
	for i := range 1000 {
		if !r.txHashIndexAvailable(ctx) {
			t.Fatalf("post-renewal read %d = false, want true", i)
		}
	}
	if conn.calls != 2 {
		t.Fatalf("conn.Query called %d times, want 2 — one renewal per lease window, not per read", conn.calls)
	}
}

// TestProbeSchema_UnleasedPositiveVerdictStaysLatched — only requireRows
// verdicts are leased. A column/table EXISTENCE probe (lecVersionProbe)
// cannot be emptied, and re-asking it would buy nothing.
func TestProbeSchema_UnleasedPositiveVerdictStaysLatched(t *testing.T) {
	conn := &probeConn{}
	clk := newFakeClock()
	r := &ExplorerReader{conn: conn, lecVersionProbe: schemaProbe{now: clk.now}}

	for range 3 {
		if !r.ledgerEntriesVersioned(context.Background()) {
			t.Fatal("ledgerEntriesVersioned = false, want true")
		}
		clk.advance(10 * schemaProbeLease)
	}
	if conn.calls != 1 {
		t.Fatalf("conn.Query called %d times, want 1 — an existence verdict is not leased", conn.calls)
	}
}

// TestProbeSchema_UnansweredRenewalIsBoundedStale — a renewal probe that
// gets NO answer (the renewing request was cancelled, the store is shedding
// load) must not instantly push every reader onto the scan/503 arm, and
// must not re-create the latch either: the last verdict is honoured only
// up to schemaProbeStaleLeases lease lengths from the last observed row.
func TestProbeSchema_UnansweredRenewalIsBoundedStale(t *testing.T) {
	lake := &liveTxLake{indexExists: true, indexRows: true}
	conn := &stubConn{respond: lake.respond}
	clk := newFakeClock()
	r := &ExplorerReader{conn: conn, txIndexProbe: schemaProbe{now: clk.now}}
	ctx := context.Background()

	if !r.txHashIndexAvailable(ctx) {
		t.Fatal("cold probe = false, want true")
	}

	lake.probeErr = &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}
	clk.advance(schemaProbeLease + time.Second)
	for i := range 5 {
		if !r.txHashIndexAvailable(ctx) {
			t.Fatalf("read %d after one unanswered renewal = false, want the last verdict honoured "+
				"inside the stale bound", i)
		}
	}
	if n := countQueries(conn.queries, isIndexProbe); n != 2 {
		t.Fatalf("probes = %d, want 2 — an unanswered renewal must back off, not probe per read", n)
	}

	// Still unanswered past the bound: the verdict is no longer honoured.
	clk.advance(schemaProbeStaleLeases * schemaProbeLease)
	if r.txHashIndexAvailable(ctx) {
		t.Fatal("verdict still honoured past the stale bound with the store not answering — " +
			"that is the latch again")
	}

	// The store answers again: authority returns.
	lake.probeErr = nil
	clk.advance(schemaProbeRetryAfter + time.Second)
	if !r.txHashIndexAvailable(ctx) {
		t.Fatal("probe = false after the store recovered with rows present, want true")
	}
}

// TestProbeSchema_RenewalInDropRecreateGapDoesNotLatchAbsent — the lease
// introduces a re-probe, and a re-probe can land between the DROP and the
// CREATE of the recreate idiom. UNKNOWN_TABLE there is NOT a deployment
// shape: latching it would pin the bloom scan on every hash lookup until a
// restart. A process that has seen the object keeps re-probing instead.
func TestProbeSchema_RenewalInDropRecreateGapDoesNotLatchAbsent(t *testing.T) {
	lake := &liveTxLake{indexExists: true, indexRows: true}
	conn := &stubConn{respond: lake.respond}
	clk := newFakeClock()
	r := &ExplorerReader{conn: conn, txIndexProbe: schemaProbe{now: clk.now}}
	ctx := context.Background()

	if !r.txHashIndexAvailable(ctx) {
		t.Fatal("cold probe = false, want true")
	}

	lake.indexExists = false // DROP TABLE
	clk.advance(schemaProbeLease + time.Second)
	if _, found, err := r.TransactionByHash(ctx, testTxHash); err != nil || !found {
		t.Fatalf("dropped index: (found=%v, err=%v), want the scan to serve the transaction", found, err)
	}
	if n := countQueries(conn.queries, isIndexLookup); n != 0 {
		t.Fatalf("index lookups = %d, want 0 — a dropped index must not be consulted", n)
	}

	lake.indexExists, lake.indexRows = true, true // CREATE + backfill
	clk.advance(schemaProbeRetryAfter + time.Second)
	if !r.txHashIndexAvailable(ctx) {
		t.Fatal("probe = false after the index was recreated — the mid-recreate UNKNOWN_TABLE latched " +
			"absent for the process lifetime")
	}
}
