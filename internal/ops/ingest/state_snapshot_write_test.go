package ingest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// recordingInserter stands in for clickhouse.InsertEntryChanges.
type recordingInserter struct{ calls, rows int }

func (r *recordingInserter) insert(_ context.Context, _ string, rows []clickhouse.LedgerEntryChangeRow, _ time.Duration) (int, error) {
	r.calls++
	r.rows += len(rows)
	return len(rows), nil
}

// TestWriteSnapshot_RefusesLimitTruncatedRead: the default -limit stops the
// bucket-list walk at 2M of ~48M entries. Writing that prefix published ~4%
// of the checkpoint as the whole and printed a green checkmark (#1183); the
// write must be refused before a single row reaches ClickHouse.
func TestWriteSnapshot_RefusesLimitTruncatedRead(t *testing.T) {
	t.Parallel()
	tally := &snapTally{total: 2_000_000, partial: true, rows: make([]clickhouse.LedgerEntryChangeRow, 3)}
	var ins recordingInserter
	err := writeSnapshot(context.Background(), ins.insert, tally, 2_000_000, "all", "127.0.0.1:9300", 0)
	if err == nil {
		t.Fatal("writeSnapshot accepted a -limit-truncated read")
	}
	if ins.calls != 0 {
		t.Fatalf("inserter called %d time(s) with %d row(s) from a partial read; want none", ins.calls, ins.rows)
	}
	if !strings.Contains(err.Error(), "-limit 0") {
		t.Fatalf("err = %v, want the -limit 0 remedy named", err)
	}
}

func TestWriteSnapshot_WritesCompleteRead(t *testing.T) {
	t.Parallel()
	tally := &snapTally{total: 3, rows: make([]clickhouse.LedgerEntryChangeRow, 3)}
	var ins recordingInserter
	if err := writeSnapshot(context.Background(), ins.insert, tally, 0, "all", "127.0.0.1:9300", 0); err != nil {
		t.Fatalf("complete read refused: %v", err)
	}
	if ins.calls != 1 || ins.rows != 3 {
		t.Fatalf("inserter calls=%d rows=%d, want 1 call with 3 rows", ins.calls, ins.rows)
	}
}
