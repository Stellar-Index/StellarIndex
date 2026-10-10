package clickhouse

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Once a CH outage backs the buffer past flushEvery, every Add flushes
// inline; when CH recovers, that inline flush writes the whole backlog. It
// must credit `written`, or buffered − written reads as a permanent backlog.
func TestLiveSink_WrittenCreditedOnInlineAddFlush(t *testing.T) {
	conn := &fakeOrderConn{failTable: "ledgers"}
	l := newLiveSink(&Sink{conn: conn, flushEvery: 3, maxBufferLedgers: 10},
		slog.New(slog.DiscardHandler), LiveSinkOptions{
			BufferSize:    8,
			FlushInterval: time.Hour,
			WriteTimeout:  time.Second,
		})

	// Ledgers 3, 4 and 5 each take Add's inline flush, which fails.
	for seq := uint32(1); seq <= 5; seq++ {
		l.add(LedgerExtract{Ledger: LedgerRow{LedgerSeq: seq}})
	}
	if got := l.WrittenCount(); got != 0 {
		t.Fatalf("WrittenCount during outage = %d, want 0", got)
	}

	conn.failTable = "" // ClickHouse recovers.
	l.add(LedgerExtract{Ledger: LedgerRow{LedgerSeq: 6}})
	l.doFlush()

	if got := l.sink.BufferedLedgers(); got != 0 {
		t.Fatalf("BufferedLedgers after recovery = %d, want 0", got)
	}
	if got := l.WrittenCount(); got != 6 {
		t.Errorf("WrittenCount = %d, want 6: the inline flush made all six ledgers durable", got)
	}
	if got := l.BufferedCount(); got != 6 {
		t.Errorf("BufferedCount = %d, want 6: an extract whose inline flush failed is still buffered", got)
	}
	if got := l.ErroredCount(); got != 3 {
		t.Errorf("ErroredCount = %d, want 3 failed inline flushes", got)
	}
}

// A producer still pushing after Stop must be counted as a drop, not left
// in a channel the exited worker never reads.
func TestLiveSink_PushAfterStopCountsDrop(t *testing.T) {
	l := newLiveSink(&Sink{conn: stalledConn{}, flushEvery: 1000},
		slog.New(slog.DiscardHandler), LiveSinkOptions{
			BufferSize:    8,
			FlushInterval: time.Hour,
			WriteTimeout:  time.Second,
		})
	l.Start()
	l.Stop()

	l.PushLedger(LedgerExtract{Ledger: LedgerRow{LedgerSeq: 9}})

	if got := l.DroppedCount(); got != 1 {
		t.Fatalf("DroppedCount after a post-Stop push = %d, want 1", got)
	}
	if got := len(l.ch); got != 0 {
		t.Errorf("post-Stop push left %d extract(s) in the unread channel", got)
	}
}

// A buffer-full drop leaves a lake hole the watermark stalls on; the WARN
// must name the dropped ledger, or nothing says which ledger to re-derive.
func TestLiveSink_PushLedgerDropLogsLedger(t *testing.T) {
	var buf bytes.Buffer
	l := newLiveSink(&Sink{conn: stalledConn{}, flushEvery: 1000},
		slog.New(slog.NewTextHandler(&buf, nil)), LiveSinkOptions{
			BufferSize:    1,
			FlushInterval: time.Hour,
			WriteTimeout:  time.Second,
		})
	// Not started: the one-slot buffer holds ledger 7 and ledger 8 drops.
	l.PushLedger(LedgerExtract{Ledger: LedgerRow{LedgerSeq: 7}})
	l.PushLedger(LedgerExtract{Ledger: LedgerRow{LedgerSeq: 8}})

	if got := l.DroppedCount(); got != 1 {
		t.Fatalf("DroppedCount = %d, want 1", got)
	}
	if out := buf.String(); !strings.Contains(out, "ledger=8") || !strings.Contains(out, "DROPPED") {
		t.Errorf("drop log = %q, want a WARN naming ledger=8", out)
	}
}

// TestLiveSink_StopBoundedByStopTimeout pins that Stop is bounded. An unbounded Stop would run the final
// flush and then Close (which flushes again), each on a fresh WriteTimeout,
// after any flush already in flight: up to 3 × WriteTimeout (90s in
// production) against a wedged ClickHouse, past systemd's stop timeout.
func TestLiveSink_StopBoundedByStopTimeout(t *testing.T) {
	l := newLiveSink(&Sink{conn: stalledConn{}, flushEvery: 1000},
		slog.New(slog.DiscardHandler), LiveSinkOptions{
			BufferSize:    8,
			FlushInterval: time.Hour,
			WriteTimeout:  2 * time.Second,
			StopTimeout:   200 * time.Millisecond,
		})
	l.Start()
	l.PushLedger(LedgerExtract{Ledger: LedgerRow{LedgerSeq: 7}})

	start := time.Now()
	l.Stop()
	elapsed := time.Since(start)

	// Unbounded, the final flush plus Close's flush take 2 × WriteTimeout = 4s.
	if elapsed > time.Second {
		t.Fatalf("Stop took %v; want it bounded by StopTimeout (200ms), not a WriteTimeout per phase", elapsed)
	}
	if got := l.ErroredCount(); got == 0 {
		t.Errorf("ErroredCount = 0; the unflushed shutdown ledger must be counted, not discarded silently")
	}
}
