package clickhouse

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

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
