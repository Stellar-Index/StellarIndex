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

// TestContractEventsRecent_FastPathKeepsReadInOrder — the fast query must
// carry NEITHER `FINAL` (defeats the contract_id bloom skip-index for
// quiet contracts) NOR `LIMIT 1 BY` (disables the reverse read-in-order
// early exit for busy ones — measured 16.3s vs 0.16s on r1, 2026-08-06,
// the CCW5IBJ7… contract-page 503). The W4-storage-1 dedup moved to
// Go-side adjacent-row collapse.
func TestContractEventsRecent_FastPathKeepsReadInOrder(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if !strings.Contains(q, "FROM stellar.contract_events") {
			t.Fatalf("unexpected query: %s", q)
		}
		// Duplicate row-identity (un-merged RMT part) + a distinct row.
		return &stubRows{data: [][]any{
			contractEventRecentRowFor(100, 0, 0),
			contractEventRecentRowFor(100, 0, 0),
			contractEventRecentRowFor(99, 0, 0),
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.ContractEventsRecent(context.Background(), "CTESTCONTRACT", 100, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	// audit W4-storage-1: the duplicate part must still collapse — now in Go.
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (adjacent duplicate collapsed)", len(rows))
	}
	if len(conn.queries) != 1 {
		t.Fatalf("issued %d queries, want 1 (no fallback on a clean page)", len(conn.queries))
	}
	q := conn.queries[len(conn.queries)-1]
	if strings.Contains(q, "LIMIT 1 BY") {
		t.Fatalf("query = %q, must NOT use LIMIT 1 BY (disables reverse read-in-order early exit)", q)
	}
	if strings.Contains(q, "stellar.contract_events FINAL") {
		t.Fatalf("query = %q, must NOT use FINAL on the bloom-probed scan", q)
	}
	if !strings.Contains(q, "ORDER BY ledger_seq DESC, tx_hash DESC, op_index DESC, event_index DESC LIMIT ?") {
		t.Fatalf("query = %q, want plain ORDER BY … LIMIT ? (read-in-order eligible)", q)
	}
}

// TestContractEventsRecent_DupStormFallsBackToInCHDedup — when the raw
// fetch returns the full over-fetch and dedup still can't fill the page,
// a short page would falsely read as "last page" (the handler keys
// next_cursor on a full page). The reader must then issue the legacy
// LIMIT 1 BY shape.
func TestContractEventsRecent_DupStormFallsBackToInCHDedup(t *testing.T) {
	const limit = 2
	fetch := limit + contractEventsDedupHeadroom
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "LIMIT 1 BY") {
			// Fallback path: in-CH dedup returns a clean full page.
			return &stubRows{data: [][]any{
				contractEventRecentRowFor(100, 0, 0),
				contractEventRecentRowFor(99, 0, 0),
			}}, nil
		}
		// Fast path: every fetched row is the SAME identity — dedup
		// collapses to 1 < limit while raw == fetch.
		data := make([][]any, fetch)
		for i := range data {
			data[i] = contractEventRecentRowFor(100, 0, 0)
		}
		return &stubRows{data: data}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.ContractEventsRecent(context.Background(), "CTESTCONTRACT", limit, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(conn.queries) != 2 {
		t.Fatalf("issued %d queries, want 2 (fast path + dedup fallback)", len(conn.queries))
	}
	if !strings.Contains(conn.queries[1], "LIMIT 1 BY ledger_seq, tx_hash, op_index, event_index LIMIT ?") {
		t.Fatalf("fallback query = %q, want the in-CH LIMIT 1 BY dedup shape", conn.queries[1])
	}
	if len(rows) != limit {
		t.Fatalf("rows = %d, want %d from the fallback", len(rows), limit)
	}
}

func TestContractEventsRecent_CursorShapes(t *testing.T) {
	// Both shapes must keyset-page by the FULL row-identity tuple.
	for name, q := range map[string]string{
		"fast":  contractEventsRecentQuery(true),
		"dedup": contractEventsRecentDedupQuery(true),
	} {
		if !strings.Contains(q, "(ledger_seq, tx_hash, op_index, event_index) < (?, ?, ?, ?)") {
			t.Fatalf("%s cursor query = %q, lost its keyset predicate", name, q)
		}
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
