package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Regression tests for audit W4-storage-1 (ContractEventsRecent + EventsByTx
// read stellar.contract_events — a ReplacingMergeTree — with no FINAL /
// LIMIT 1 BY / uniqExact, so during a merge window an un-merged duplicate part
// was served as a duplicate EVENT). Same class as DAT-10, same query-SHAPE
// proof idiom: the stubConn/stubRows harness (tx_hash_index_test.go) does not
// implement real ReplacingMergeTree semantics, so these assert the emitted SQL
// carries the dedup construct the siblings use — the live-ClickHouse proof that
// the merge actually collapses the duplicate is
// TestClickHouseContractEventsRMTDedup (test/integration, tagged integration).

// contractEventRecentRowFor builds the 9-column ContractActivityRow that
// ContractEventsRecent's scan expects (ledger_seq, close_time, tx_hash,
// op_index, event_index, event_type, topic_0_sym, topics_xdr, data_xdr).
func contractEventRecentRowFor(seq uint32, opIndex, eventIndex uint32) []any {
	return []any{
		seq, time.Unix(1700000000, 0).UTC(), "txhash", opIndex, eventIndex,
		"contract", "transfer",
		[]string{"AAAA"},
		"",
	}
}

func TestContractEventsRecent_DedupsPerPrimaryKey(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if !strings.Contains(q, "FROM stellar.contract_events") {
			t.Fatalf("unexpected query: %s", q)
		}
		return &stubRows{data: [][]any{contractEventRecentRowFor(100, 0, 0)}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.ContractEventsRecent(context.Background(), "CTESTCONTRACT", 100, ExplorerCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if len(conn.queries) != 1 {
		t.Fatalf("issued %d queries, want 1", len(conn.queries))
	}
	q := conn.queries[len(conn.queries)-1]
	// audit W4-storage-1: without a dedup construct a re-ingested event leaves an
	// un-merged duplicate part and this activity feed serves it twice.
	if !strings.Contains(q, "LIMIT 1 BY ledger_seq, tx_hash, op_index, event_index") {
		t.Fatalf("query = %q, want a `LIMIT 1 BY ledger_seq, tx_hash, op_index, event_index` dedup clause", q)
	}
	// Must stay a cheap bloom-probed scan: NOT FINAL (would defeat the
	// contract_id skip-index and force a full-table merge — see the method note).
	if strings.Contains(q, "stellar.contract_events FINAL") {
		t.Fatalf("query = %q, must NOT use FINAL on the bloom-probed scan", q)
	}
	// The LIMIT 1 BY clause must come AFTER the existing ORDER BY and BEFORE the
	// page-size LIMIT (ClickHouse clause order), so a duplicate part cannot eat a
	// page slot before the page is cut.
	if !strings.Contains(q, "ORDER BY ledger_seq DESC, op_index DESC, event_index DESC LIMIT 1 BY") {
		t.Fatalf("query = %q, want LIMIT 1 BY to follow the existing ORDER BY", q)
	}
	if !strings.Contains(q, "event_index LIMIT ?") {
		t.Fatalf("query = %q, want the page-size LIMIT ? after the dedup clause", q)
	}
}

func TestContractEventsRecent_DedupSurvivesCursor(t *testing.T) {
	// The keyset-paged variant must ALSO carry the dedup: the merge-window dup
	// concentrates in recent ledgers, exactly what a cursor pages back through.
	q := contractEventsRecentQuery(true)
	if !strings.Contains(q, "LIMIT 1 BY ledger_seq, tx_hash, op_index, event_index LIMIT ?") {
		t.Fatalf("cursor query = %q, want the dedup clause before the page LIMIT", q)
	}
	if !strings.Contains(q, "(ledger_seq, op_index, event_index) < (?, ?, ?)") {
		t.Fatalf("cursor query = %q, lost its keyset predicate", q)
	}
}

func TestEventsByTx_UsesFinal(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		return &stubRows{data: [][]any{{
			uint32(0), uint32(0), "CTESTCONTRACT", "contract", "transfer",
		}}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.EventsByTx(context.Background(), 100, "abchash")
	if err != nil {
		t.Fatalf("EventsByTx: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	q := conn.queries[len(conn.queries)-1]
	// audit W4-storage-1: ledger+tx_hash-scoped so FINAL stays cheap (partition +
	// primary-key-prefix bounded), matching the byte-twin OperationsByTx.
	if !strings.Contains(q, "FROM stellar.contract_events FINAL") {
		t.Fatalf("query = %q, want `FROM stellar.contract_events FINAL`", q)
	}
}
