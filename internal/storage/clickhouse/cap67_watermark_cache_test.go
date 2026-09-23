package clickhouse

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// wmConn answers the cap67 watermark QueryRow. The first call blocks until
// release is closed when block is set; every call counts.
type wmConn struct {
	driver.Conn
	mu      sync.Mutex
	calls   int
	errs    []error // per-call error, consumed in order; nil/empty = success
	wm      uint32
	block   bool
	release chan struct{}
	entered chan struct{}
}

func (c *wmConn) QueryRow(_ context.Context, _ string, _ ...any) driver.Row {
	c.mu.Lock()
	c.calls++
	first := c.calls == 1
	var err error
	if len(c.errs) > 0 {
		err, c.errs = c.errs[0], c.errs[1:]
	}
	c.mu.Unlock()
	if first && c.block {
		c.entered <- struct{}{}
		<-c.release
	}
	if err != nil {
		return &stubRow{err: err}
	}
	return &stubRow{data: []any{c.wm}}
}

func (c *wmConn) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestCap67WatermarkCache_WaiterHonoursItsContext — a slow watermark
// round-trip must not queue every concurrent /movements request behind a
// context-blind mutex: a waiter whose deadline passes returns, and it
// does not issue a second query against the struggling store.
func TestCap67WatermarkCache_WaiterHonoursItsContext(t *testing.T) {
	conn := &wmConn{
		wm: 70_000_000, block: true,
		release: make(chan struct{}), entered: make(chan struct{}, 1),
	}
	r := &ExplorerReader{conn: conn}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		if wm, err := r.Cap67MovementsWatermark(context.Background()); err != nil || wm != 70_000_000 {
			t.Errorf("owner: wm=%d err=%v, want 70000000 nil", wm, err)
		}
	}()
	<-conn.entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	waiterDone := make(chan error, 1)
	go func() {
		_, err := r.Cap67MovementsWatermark(ctx)
		waiterDone <- err
	}()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("waiter err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		close(conn.release)
		t.Fatal("waiter blocked past its own deadline behind an in-flight watermark query")
	}
	close(conn.release)
	<-firstDone

	if wm, err := r.Cap67MovementsWatermark(context.Background()); err != nil || wm != 70_000_000 {
		t.Fatalf("cached read: wm=%d err=%v", wm, err)
	}
	if n := conn.callCount(); n != 1 {
		t.Errorf("QueryRow called %d times, want 1 — concurrent callers must share one flight", n)
	}
}

// TestCap67WatermarkCache_FailureBacksOff — a failed read is negatively
// cached for cap67WMRetryAfter, so an outage costs one query per window
// instead of one per request; after the window the store is retried.
func TestCap67WatermarkCache_FailureBacksOff(t *testing.T) {
	boom := errors.New("TOO_MANY_SIMULTANEOUS_QUERIES")
	conn := &wmConn{wm: 70_000_123, errs: []error{boom}}
	r := &ExplorerReader{conn: conn}

	for i := 0; i < 5; i++ {
		wm, err := r.Cap67MovementsWatermark(context.Background())
		if !errors.Is(err, boom) || wm != 0 {
			t.Fatalf("call %d: wm=%d err=%v, want 0 and the backend error", i, wm, err)
		}
	}
	if n := conn.callCount(); n != 1 {
		t.Fatalf("QueryRow called %d times during back-off, want 1", n)
	}

	r.cap67WMMu.Lock()
	r.cap67WMErrAt = r.cap67WMErrAt.Add(-cap67WMRetryAfter)
	r.cap67WMMu.Unlock()

	wm, err := r.Cap67MovementsWatermark(context.Background())
	if err != nil || wm != 70_000_123 {
		t.Fatalf("after back-off: wm=%d err=%v, want 70000123 nil", wm, err)
	}
	if n := conn.callCount(); n != 2 {
		t.Errorf("QueryRow called %d times, want 2 (one retry after the window)", n)
	}
}

// TestCap67WatermarkCache_CallerCancelDoesNotPoison — a client disconnect
// (context.Canceled on the owner's own ctx) says nothing about the store,
// so it must not arm the back-off for everyone else.
func TestCap67WatermarkCache_CallerCancelDoesNotPoison(t *testing.T) {
	conn := &wmConn{wm: 70_000_456, errs: []error{context.Canceled}}
	r := &ExplorerReader{conn: conn}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Cap67MovementsWatermark(ctx); err == nil {
		t.Fatal("cancelled caller got nil error")
	}
	wm, err := r.Cap67MovementsWatermark(context.Background())
	if err != nil || wm != 70_000_456 {
		t.Fatalf("next caller: wm=%d err=%v, want 70000456 nil — a caller's cancel poisoned the cache", wm, err)
	}
}
