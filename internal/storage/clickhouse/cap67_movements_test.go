package clickhouse

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// cap67AdvanceConn answers cap67AdvanceProven's three reads: the watermark,
// the ledger contiguity count and the window event census. The embedded
// driver.Conn panics on Exec, so a refused advance provably writes nothing.
func cap67AdvanceConn(wm uint32, ledgers, expected, present uint64) *stubConn {
	return &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "stellar.cap67_movements_watermark"):
			return &stubRows{data: [][]any{{wm}}}, nil
		case strings.Contains(q, "stellar.contract_events"):
			return &stubRows{data: [][]any{{expected, present}}}, nil
		case strings.Contains(q, "count(DISTINCT ledger_seq)"):
			return &stubRows{data: [][]any{{ledgers}}}, nil
		}
		return nil, errors.New("unexpected query: " + q)
	}}
}

// TestCap67AdvanceProven_RefusesAnEventShortfall: a window whose contract_events fall short of the summed
// soroban_event_count must not advance, since the watermark never revisits it and the derive saw no
// movements.
func TestCap67AdvanceProven_RefusesAnEventShortfall(t *testing.T) {
	const from, thru = uint32(101), uint32(110)
	conn := cap67AdvanceConn(100, 10, 42, 0)
	advance, err := cap67AdvanceProven(context.Background(), conn, from, thru)
	if !errors.Is(err, ErrCap67MovementsEventShortfall) {
		t.Fatalf("cap67AdvanceProven over a contiguous window missing its events = (%v, %v), want ErrCap67MovementsEventShortfall", advance, err)
	}
	if advance {
		t.Fatal("advance = true alongside a refusal")
	}
	censused := false
	for i, q := range conn.queries {
		if !strings.Contains(q, "stellar.contract_events") {
			continue
		}
		censused = true
		if got := conn.args[i]; len(got) != 4 || got[0] != from || got[1] != thru || got[2] != from || got[3] != thru {
			t.Fatalf("event census bound %v, want [from thru from thru] = [%d %d %d %d]", got, from, thru, from, thru)
		}
	}
	if !censused {
		t.Fatal("no event census query issued")
	}
}

// TestCap67AdvanceProven_EventCensusPassesCompleteAndDuplicated pins the
// non-vacuous side: complete events advance, and unmerged RMT duplicates
// (present above expected) are not read as a shortfall.
func TestCap67AdvanceProven_EventCensusPassesCompleteAndDuplicated(t *testing.T) {
	for _, present := range []uint64{42, 50} {
		advance, err := cap67AdvanceProven(context.Background(), cap67AdvanceConn(100, 10, 42, present), 101, 110)
		if err != nil || !advance {
			t.Fatalf("present=%d of 42 expected: (%v, %v), want (true, nil)", present, advance, err)
		}
	}
}

func TestCap67WindowEventCensusQuery_Shape(t *testing.T) {
	for _, want := range []string{
		"argMax(soroban_event_count, ingested_at)",
		"GROUP BY ledger_seq",
		"FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?",
		"FROM stellar.contract_events WHERE ledger_seq BETWEEN ? AND ?",
	} {
		if !strings.Contains(cap67WindowEventCensusQuery, want) {
			t.Errorf("cap67WindowEventCensusQuery missing %q", want)
		}
	}
}

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
	return &stubRow{data: []any{c.wm, uint32(0), uint32(0)}}
}

func (c *wmConn) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestCap67WatermarkCache_WaiterHonoursItsContext: a slow watermark round-trip must not queue concurrent
// /movements requests behind a context-blind mutex; a waiter past its deadline returns without a second
// query.
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
