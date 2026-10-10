package clickhouse

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// probeRows is a driver.Rows stub with one row — probeSchema closes it,
// and (for requireRows probes like txIndexProbe) iterates it once.
type probeRows struct {
	driver.Rows
	consumed bool
}

func (r *probeRows) Next() bool {
	if r.consumed {
		return false
	}
	r.consumed = true
	return true
}

func (r *probeRows) Err() error   { return nil }
func (r *probeRows) Close() error { return nil }

// probeConn returns a scripted result per Query call so a probe can be
// driven through a transient failure and then a success.
type probeConn struct {
	driver.Conn
	results []error // nil = the query succeeded
	calls   int
}

func (c *probeConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	i := c.calls
	c.calls++
	if i < len(c.results) && c.results[i] != nil {
		return nil, c.results[i]
	}
	return &probeRows{}, nil
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

// blockingProbeConn blocks the FIRST Query until release is closed; later
// Queries return immediately.
type blockingProbeConn struct {
	driver.Conn
	mu      sync.Mutex
	calls   int
	release chan struct{}
	entered chan struct{}
}

func (c *blockingProbeConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	c.mu.Lock()
	c.calls++
	first := c.calls == 1
	c.mu.Unlock()
	if first {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		<-c.release
	}
	return &probeRows{}, nil
}
