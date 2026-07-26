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

// probeRows is a driver.Rows stub — probeSchema only ever closes it.
type probeRows struct{ driver.Rows }

func (probeRows) Close() error { return nil }

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
	return probeRows{}, nil
}

// TestProbeSchema_TransientErrorDoesNotLatch pins C1-048
// (audit-2026-07-23). The schema probes were a plain sync.Once, so the
// FIRST call's outcome was final for the process lifetime: a transient
// ClickHouse error at that instant — a restart mid-deploy, a reset
// connection, a request-context deadline — latched the probe to false and
// silently degraded every subsequent read until the process was
// restarted. No error, no metric, no self-heal.
func TestProbeSchema_TransientErrorDoesNotLatch(t *testing.T) {
	conn := &probeConn{results: []error{
		// 1st: a transport failure — the server never answered.
		&net.OpError{Op: "read", Err: errors.New("connection reset by peer")},
		// 2nd: healthy again.
		nil,
	}}
	r := &ExplorerReader{conn: conn, lecVersionProbe: schemaProbe{retryAfter: -1}}
	ctx := context.Background()

	if got := r.ledgerEntriesVersioned(ctx); got {
		t.Fatalf("first probe = true, want false — the transient error means we have no answer yet")
	}
	if got := r.ledgerEntriesVersioned(ctx); !got {
		t.Errorf("second probe = false, want true — a transient failure must not latch the fallback "+
			"for the process lifetime (conn saw %d queries)", conn.calls)
	}
	if conn.calls != 2 {
		t.Errorf("conn.Query called %d times, want 2 (re-probe after a non-answer)", conn.calls)
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

// TestProbeSchema_ContextDeadlineDoesNotLatch — the concrete production
// trigger: the very first read of a fresh process runs under a request
// deadline that expires. That must not permanently downgrade the reader.
func TestProbeSchema_ContextDeadlineDoesNotLatch(t *testing.T) {
	conn := &probeConn{results: []error{context.DeadlineExceeded, nil}}
	r := &ExplorerReader{conn: conn, txIndexProbe: schemaProbe{retryAfter: -1}}

	if r.txHashIndexAvailable(context.Background()) {
		t.Fatal("first probe = true, want false on a deadline")
	}
	if !r.txHashIndexAvailable(context.Background()) {
		t.Error("second probe = false, want true — one expired request deadline must not " +
			"disable the tx-hash index for the life of the process")
	}
}

// TestProbeSchema_ResourceExceptionDoesNotLatch pins the second-review half
// of C1-048. ClickHouse raises an Exception for plain RESOURCE conditions —
// 202 TOO_MANY_SIMULTANEOUS_QUERIES, 159 TIMEOUT_EXCEEDED, 241
// MEMORY_LIMIT_EXCEEDED, 209 SOCKET_TIMEOUT — not just for schema verdicts.
// Treating "any *clickhouse.Exception" as a definitive answer meant one
// overload blip latched the probe false for the PROCESS LIFETIME. For
// lecVersionProbe that means falling back to ledger_seq as the RMT version
// key forever, i.e. serving non-final intra-ledger balances until someone
// restarts the API — the exact silent-degradation shape the sync.Once fix
// was supposed to remove.
func TestProbeSchema_ResourceExceptionDoesNotLatch(t *testing.T) {
	resourceCodes := []struct {
		code int32
		name string
	}{
		{202, "TOO_MANY_SIMULTANEOUS_QUERIES"},
		{159, "TIMEOUT_EXCEEDED"},
		{241, "MEMORY_LIMIT_EXCEEDED"},
		{209, "SOCKET_TIMEOUT"},
		{1002, "UNKNOWN_EXCEPTION"}, // catch-all: not an answer either
	}
	for _, rc := range resourceCodes {
		t.Run(rc.name, func(t *testing.T) {
			conn := &probeConn{results: []error{
				&clickhouse.Exception{Code: rc.code, Name: rc.name, Message: rc.name},
				nil, // the store recovers
			}}
			r := &ExplorerReader{conn: conn, lecVersionProbe: schemaProbe{retryAfter: -1}}

			if r.ledgerEntriesVersioned(context.Background()) {
				t.Fatalf("first probe = true, want false while the store is returning %s", rc.name)
			}
			if !r.ledgerEntriesVersioned(context.Background()) {
				t.Errorf("second probe = false after the store recovered — code %d (%s) is a "+
					"RESOURCE condition, not a schema verdict, and must not latch the fallback "+
					"for the process lifetime (conn saw %d queries)", rc.code, rc.name, conn.calls)
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
	return probeRows{}, nil
}
