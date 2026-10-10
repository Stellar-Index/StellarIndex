package clickhouse

import (
	"context"
	"sync"

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
