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
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
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
// pins the lease. Without it probeSchema would LATCH a positive verdict for the process
// lifetime, so the requireRows guard protects only a cold start: an
// operator TRUNCATEing (or DROP+recreating) stellar.tx_hash_index ahead of
// a ch-txindex-backfill leaves every already-running API process holding
// settled=true/present=true, every index lookup misses, and every miss is
// served as an AUTHORITATIVE 404 for a transaction that exists — until a
// restart. The verdict is a lease: the SAME reader, with no restart,
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
	if n := countQueries(conn.queries, isIndexProbe) - countQueries(conn.queries, isCoverageProbe); n != 2 {
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
// the lease: it must not put a probe on every request. Inside one lease
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

// TestProbeSchema_LatchedAbsenceIsExported pins: an API that starts
// before the lake DDL latches the probe false for its whole lifetime and
// serves the slow fallback — the gauge is the only place that shows.
func TestProbeSchema_LatchedAbsenceIsExported(t *testing.T) {
	conn := &probeConn{results: []error{
		&clickhouse.Exception{Code: 60, Name: "UNKNOWN_TABLE", Message: "Table stellar.tx_hash_index does not exist"},
	}}
	gauge := obs.CHSchemaProbePresent.WithLabelValues("tx_hash_index")
	gauge.Set(-1) // a fresh gauge reads 0 already; -1 proves the probe wrote it
	r := newExplorerReader(conn)
	ctx := context.Background()

	if r.txHashIndexAvailable(ctx) || r.txHashIndexAvailable(ctx) {
		t.Fatal("tx_hash_index reported available after UNKNOWN_TABLE")
	}
	if conn.calls != 1 {
		t.Fatalf("conn.Query called %d times, want 1 (absence latches)", conn.calls)
	}
	if got := testutil.ToFloat64(gauge); got != 0 {
		t.Errorf("stellarindex_ch_schema_probe_present{probe=tx_hash_index} = %v, want 0", got)
	}
}

// TestProbeSchema_PresentAndEmptyAreExported — the gauge follows every
// ANSWER, including a row-requiring probe revoked by an emptied table.
func TestProbeSchema_PresentAndEmptyAreExported(t *testing.T) {
	gauge := obs.CHSchemaProbePresent.WithLabelValues("contracts_census_daily")
	gauge.Set(-1)
	r := newExplorerReader(&probeConn{})
	if !r.censusAvailable(context.Background()) {
		t.Fatal("census probe = false on a table with rows")
	}
	if got := testutil.ToFloat64(gauge); got != 1 {
		t.Fatalf("present gauge after a row = %v, want 1", got)
	}

	r = newExplorerReader(emptyProbeConn{})
	if r.censusAvailable(context.Background()) {
		t.Fatal("census probe = true on an empty table")
	}
	if got := testutil.ToFloat64(gauge); got != 0 {
		t.Errorf("present gauge after an empty answer = %v, want 0", got)
	}
}

// TestProbeSchema_NonAnswerIsCountedNotExported — a transport failure says
// nothing about the object: it must not move the gauge, and must count.
func TestProbeSchema_NonAnswerIsCountedNotExported(t *testing.T) {
	const probe = "ledger_entries_current_version"
	gauge := obs.CHSchemaProbePresent.WithLabelValues(probe)
	gauge.Set(1)
	unanswered := obs.CHSchemaProbeUnansweredTotal.WithLabelValues(probe)
	before := testutil.ToFloat64(unanswered)

	r := newExplorerReader(&probeConn{results: []error{
		&net.OpError{Op: "read", Err: errors.New("connection reset by peer")},
	}})
	if r.ledgerEntriesVersioned(context.Background()) {
		t.Fatal("probe = true with no answer and no prior verdict")
	}
	if got := testutil.ToFloat64(unanswered) - before; got != 1 {
		t.Errorf("unanswered counter delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(gauge); got != 1 {
		t.Errorf("present gauge moved on a non-answer: %v, want 1", got)
	}
}

// TestProbeSchema_DefinitiveAnswersAreCached — the caching half must
// survive: once the SERVER has answered, the probe stops querying. A
// re-probe on every read would put an extra round-trip on the hot path.
func TestProbeSchema_DefinitiveAnswersAreCached(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		// A ClickHouse server exception IS an answer: the server parsed
		// the query and rejected it (unknown identifier `version`).
		conn := &probeConn{results: []error{
			&clickhouse.Exception{Code: 47, Name: "UNKNOWN_IDENTIFIER", Message: "Unknown identifier `version`"},
		}}
		r := &ExplorerReader{conn: conn}
		for i := 0; i < 3; i++ {
			if r.ledgerEntriesVersioned(context.Background()) {
				t.Fatalf("probe %d = true, want false", i)
			}
		}
		if conn.calls != 1 {
			t.Errorf("conn.Query called %d times, want 1 (a server rejection is definitive)", conn.calls)
		}
	})

	t.Run("present", func(t *testing.T) {
		conn := &probeConn{}
		r := &ExplorerReader{conn: conn}
		for i := 0; i < 3; i++ {
			if !r.txHashIndexAvailable(context.Background()) {
				t.Fatalf("probe %d = false, want true", i)
			}
		}
		if conn.calls != 1 {
			t.Errorf("conn.Query called %d times, want 1 (success is definitive)", conn.calls)
		}
	})
}

// TestProbeSchema_NonAnswerDoesNotLatch pins that a probe error which is not
// a schema verdict is retried. With a plain sync.Once the FIRST call's outcome
// is final for the process lifetime: a transport reset, an expired request
// deadline on a fresh process, or a RESOURCE exception (ClickHouse raises
// *clickhouse.Exception for overload too, not just schema verdicts) would latch
// the probe false. For lecVersionProbe that means falling back to ledger_seq as
// the RMT version key forever, serving non-final intra-ledger balances until
// someone restarts the API. No error, no metric, no self-heal.
func TestProbeSchema_NonAnswerDoesNotLatch(t *testing.T) {
	lecVersioned := func(r *ExplorerReader) bool { return r.ledgerEntriesVersioned(context.Background()) }
	txIndex := func(r *ExplorerReader) bool { return r.txHashIndexAvailable(context.Background()) }
	resource := func(code int32, name string) error {
		return &clickhouse.Exception{Code: code, Name: name, Message: name}
	}
	cases := []struct {
		name  string
		err   error
		txIdx bool
		why   string
	}{
		{
			"transport", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, false,
			"a transient failure must not latch the fallback for the process lifetime",
		},
		{
			"deadline", context.DeadlineExceeded, true,
			"one expired request deadline must not disable the tx-hash index for the life of the process",
		},
		{"TOO_MANY_SIMULTANEOUS_QUERIES", resource(202, "TOO_MANY_SIMULTANEOUS_QUERIES"), false, ""},
		{"TIMEOUT_EXCEEDED", resource(159, "TIMEOUT_EXCEEDED"), false, ""},
		{"MEMORY_LIMIT_EXCEEDED", resource(241, "MEMORY_LIMIT_EXCEEDED"), false, ""},
		{"SOCKET_TIMEOUT", resource(209, "SOCKET_TIMEOUT"), false, ""},
		{"UNKNOWN_EXCEPTION", resource(1002, "UNKNOWN_EXCEPTION"), false, ""}, // catch-all: not an answer either
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &probeConn{results: []error{tc.err, nil}}
			r := &ExplorerReader{conn: conn}
			probe := lecVersioned
			if tc.txIdx {
				r.txIndexProbe = schemaProbe{retryAfter: -1}
				probe = txIndex
			} else {
				r.lecVersionProbe = schemaProbe{retryAfter: -1}
			}
			why := tc.why
			if why == "" {
				why = "a RESOURCE condition, not a schema verdict, must not latch the fallback for the process lifetime"
			}

			if probe(r) {
				t.Fatalf("first probe = true, want false while the store is returning %v — no answer yet", tc.err)
			}
			if !probe(r) {
				t.Errorf("second probe = false after the store recovered — %s (conn saw %d queries)", why, conn.calls)
			}
			if conn.calls != 2 {
				t.Errorf("conn.Query called %d times, want 2 (re-probe after a non-answer)", conn.calls)
			}
		})
	}
}

// TestProbeSchema_NegativeCacheRateLimitsProbes — an unanswered probe must
// not turn every subsequent read into an extra query while ClickHouse is
// down. The probe backs off for schemaProbeRetryAfter, then retries.
func TestProbeSchema_NegativeCacheRateLimitsProbes(t *testing.T) {
	conn := &probeConn{results: []error{
		&clickhouse.Exception{Code: 202, Name: "TOO_MANY_SIMULTANEOUS_QUERIES"},
	}}
	// Default (positive) retry window: the second call is inside it.
	r := &ExplorerReader{conn: conn}

	if r.ledgerEntriesVersioned(context.Background()) {
		t.Fatal("first probe = true, want false")
	}
	for i := 0; i < 5; i++ {
		if r.ledgerEntriesVersioned(context.Background()) {
			t.Fatalf("probe %d = true, want false", i)
		}
	}
	if conn.calls != 1 {
		t.Errorf("conn.Query called %d times, want 1 — an unanswered probe must back off, "+
			"not add a query to every read during an outage", conn.calls)
	}
}

// TestProbeSchema_QueryRunsOutsideTheLock — sync.Mutex is not context-aware,
// so holding it across the probe's network round-trip queues every
// concurrent reader behind one slow probe and serialises the explorer read
// path. A second caller must be able to proceed while the first is still in
// flight.
func TestProbeSchema_QueryRunsOutsideTheLock(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	conn := &blockingProbeConn{release: release, entered: entered}
	r := &ExplorerReader{conn: conn, lecVersionProbe: schemaProbe{retryAfter: -1}}

	go func() { _ = r.ledgerEntriesVersioned(context.Background()) }()
	<-entered // the first probe is inside conn.Query

	done := make(chan struct{})
	go func() {
		_ = r.ledgerEntriesVersioned(context.Background())
		close(done)
	}()

	select {
	case <-done:
		// Good: the second caller was not blocked by the first.
	case <-time.After(2 * time.Second):
		t.Fatal("a second caller blocked behind an in-flight probe — the mutex is held " +
			"across the network round-trip, serialising the explorer read path")
	}
	close(release)
}
