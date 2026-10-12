package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Regression tests: ContractEventsRecent and EventsByTx read a ReplacingMergeTree, so the SQL must carry
// FINAL / LIMIT 1 BY / uniqExact to avoid serving duplicates mid-merge. The stub cannot merge parts, so
// these assert the SQL; the live proof is TestClickHouseContractEventsRMTDedup.

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

// TestContractEventsRecent_FastPathKeepsReadInOrder: the fast query must carry neither `FINAL` (defeats
// the contract_id bloom skip-index) nor `LIMIT 1 BY` (disables reverse read-in-order early exit, 16.3s vs
// 0.16s); dedup is a Go-side adjacent-row collapse.
func TestContractEventsRecent_FastPathKeepsReadInOrder(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		// Ledger-index probe: empty → index unavailable → legacy path.
		if strings.Contains(q, "contract_active_ledgers") {
			return &stubRows{}, nil
		}
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
	// The duplicate part must still collapse — now in Go.
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (adjacent duplicate collapsed)", len(rows))
	}
	if len(conn.queries) != 2 {
		t.Fatalf("issued %d queries, want 2 (index probe + events; no fallback on a clean page)", len(conn.queries))
	}
	q := conn.queries[len(conn.queries)-1]
	if strings.Contains(q, "ledger_seq IN") {
		t.Fatalf("query = %q, must NOT carry a ledger bound when the index is unavailable", q)
	}
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

// TestContractEventsRecent_DupStormFallsBackToInCHDedup: when the raw fetch returns the full
// over-fetch and dedup still cannot fill the page, a short page would falsely read as "last page"
// (the handler keys next_cursor on a full page), so the reader must issue the LIMIT 1 BY shape.
func TestContractEventsRecent_DupStormFallsBackToInCHDedup(t *testing.T) {
	const limit = 2
	fetch := limit + contractEventsDedupHeadroom
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			return &stubRows{}, nil // index unavailable → legacy path
		}
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
	if len(conn.queries) != 3 {
		t.Fatalf("issued %d queries, want 3 (index probe + fast path + dedup fallback)", len(conn.queries))
	}
	if !strings.Contains(conn.queries[2], "LIMIT 1 BY ledger_seq, tx_hash, op_index, event_index LIMIT ?") {
		t.Fatalf("fallback query = %q, want the in-CH LIMIT 1 BY dedup shape", conn.queries[2])
	}
	if len(rows) != limit {
		t.Fatalf("rows = %d, want %d from the fallback", len(rows), limit)
	}
}

func TestContractEventsRecent_CursorShapes(t *testing.T) {
	// Both shapes must keyset-page by the FULL row-identity tuple, with
	// and without the active-ledger bound.
	for name, q := range map[string]string{
		"fast":          contractEventsRecentQuery(true, false),
		"dedup":         contractEventsRecentDedupQuery(true, false),
		"fast+ledgers":  contractEventsRecentQuery(true, true),
		"dedup+ledgers": contractEventsRecentDedupQuery(true, true),
	} {
		if !strings.Contains(q, "(ledger_seq, tx_hash, op_index, event_index) < (?, ?, ?, ?)") {
			t.Fatalf("%s cursor query = %q, lost its keyset predicate", name, q)
		}
		if strings.Contains(name, "ledgers") && !strings.Contains(q, "ledger_seq IN (?)") {
			t.Fatalf("%s query = %q, lost its active-ledger bound", name, q)
		}
	}
}

// TestContractEventsRecent_ActiveLedgerBound: with the contract_active_ledgers index present, a non-empty
// walk of the contract's active ledgers bounds the events read; an empty walk is not authoritative (see
// ...EmptyWalkFallsThrough).
func TestContractEventsRecent_ActiveLedgerBound(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{data: [][]any{{uint32(100)}, {uint32(99)}}}, nil
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil // probe: non-empty
		}
		if !strings.Contains(q, "ledger_seq IN (?)") {
			t.Fatalf("events query lost the ledger bound: %s", q)
		}
		return &stubRows{data: [][]any{
			contractEventRecentRowFor(100, 0, 0),
			contractEventRecentRowFor(99, 0, 0),
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	// limit 2 == rows served: the bounded page is full, so it is trusted.
	rows, err := r.ContractEventsRecent(context.Background(), "CTESTCONTRACT", 2, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if len(conn.queries) != 3 {
		t.Fatalf("issued %d queries, want 3 (probe + ledgers walk + bounded events)", len(conn.queries))
	}
}

// TestContractEventsRecent_PartialWalkShortPageFallsThrough pins that a short bounded page is re-read
// unbounded: a non-empty walk can be truncated by a partial backfill, and older events must still be served.
func TestContractEventsRecent_PartialWalkShortPageFallsThrough(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{data: [][]any{{uint32(100)}}}, nil
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		}
		if strings.Contains(q, "ledger_seq IN (?)") {
			return &stubRows{data: [][]any{contractEventRecentRowFor(100, 0, 0)}}, nil
		}
		return &stubRows{data: [][]any{
			contractEventRecentRowFor(100, 0, 0),
			contractEventRecentRowFor(50, 0, 0),
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.ContractEventsRecent(context.Background(), "CTESTCONTRACT", 2, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 — the older event the partial walk omitted", len(rows))
	}
}

// TestContractEventsRecent_EmptyWalkFallsThrough: the contract_active_ledgers probe cannot see partial
// backfill coverage, so an empty walk must not short-circuit to "no events"; it falls through to the
// unbounded scan.
func TestContractEventsRecent_EmptyWalkFallsThrough(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{}, nil // partial backfill: empty per-contract walk
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil // probe: globally non-empty
		}
		// The events table DOES hold this contract's events.
		if strings.Contains(q, "ledger_seq IN (?)") {
			t.Fatalf("empty walk must NOT bound the scan by (unknown) ledgers: %s", q)
		}
		return &stubRows{data: [][]any{
			contractEventRecentRowFor(5_000_000, 0, 0),
		}}, nil
	}
	r := &ExplorerReader{conn: conn}

	rows, err := r.ContractEventsRecent(context.Background(), "CQUIETCONTRACT", 100, ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent(empty walk): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 — the real event served via the unbounded fallthrough, "+
			"not a confidently-wrong empty page", len(rows))
	}
	// Probe + empty walk + unbounded events scan = 3 queries; the events
	// query must have run (no short-circuit to nil,nil).
	if len(conn.queries) != 3 {
		t.Fatalf("issued %d queries, want 3 (probe + walk + unbounded events scan)", len(conn.queries))
	}
	last := conn.queries[len(conn.queries)-1]
	if !strings.Contains(last, "FROM stellar.contract_events") || strings.Contains(last, "ledger_seq IN") {
		t.Fatalf("final query = %q, want the UNBOUNDED contract_events scan", last)
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
	// ledger+tx_hash-scoped so FINAL stays cheap (partition +
	// primary-key-prefix bounded), matching the byte-twin OperationsByTx.
	if !strings.Contains(q, "FROM stellar.contract_events FINAL") {
		t.Fatalf("query = %q, want `FROM stellar.contract_events FINAL`", q)
	}
}

// TestSACAssetFromEvents_ActiveLedgerBound: the /wasm 404 path probes SAC-ness for every contract without a
// captured instance. An unbounded `ORDER BY ledger_seq DESC LIMIT 1` reverse-scans the key range and times
// out, so with the index present the read must be bounded to the contract's recent active ledgers.
func TestSACAssetFromEvents_ActiveLedgerBound(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{data: [][]any{{uint32(100)}, {uint32(99)}}}, nil
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil // probe: index present
		}
		if !strings.Contains(q, "ledger_seq IN (?)") {
			t.Errorf("SAC events probe lost its ledger bound: %s", q)
		}
		return &stubRows{data: [][]any{}}, nil
	}
	r := &ExplorerReader{conn: conn}

	if _, _, err := r.SACAssetFromEvents(context.Background(), "CTESTCONTRACT"); err != nil {
		t.Fatalf("SACAssetFromEvents: %v", err)
	}
}

// TestSACAssetFromEvents_NoActiveLedgersSkipsScan: a contract the index knows has NO active
// ledgers has no events to inspect, so the answer is authoritative and the events table must not
// be touched; an unbounded fallback would cost most on the cheapest contracts to answer.
func TestSACAssetFromEvents_NoActiveLedgersSkipsScan(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{data: [][]any{}}, nil // no active ledgers
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil // probe: index present
		}
		t.Errorf("queried contract_events despite an empty active-ledger walk: %s", q)
		return &stubRows{data: [][]any{}}, nil
	}
	r := &ExplorerReader{conn: conn}

	name, found, err := r.SACAssetFromEvents(context.Background(), "CTESTCONTRACT")
	if err != nil {
		t.Fatalf("SACAssetFromEvents: %v", err)
	}
	if found || name != "" {
		t.Fatalf("got (%q, %v), want the empty authoritative answer", name, found)
	}
}

// TestContractInteractions_BusyContractWindowNarrows: the read scales with ledger span, so a busy contract
// is slow over the default window. The window is anchored to the contract's recent activity, and the
// effective floor is returned so since_ledger describes what was served, not what was asked for.
func TestContractInteractions_BusyContractWindowNarrows(t *testing.T) {
	const window = 500
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				// Exactly the cap = busy: newest first, oldest last.
				data := make([][]any, window)
				for i := range data {
					data[i] = []any{uint32(9000 - i)}
				}
				return &stubRows{data: data}, nil
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		}
		return &stubRows{data: [][]any{}}, nil
	}
	r := &ExplorerReader{conn: conn}

	_, effective, err := r.ContractInteractions(context.Background(), "CTESTCONTRACT", 50, 100)
	if err != nil {
		t.Fatalf("ContractInteractions: %v", err)
	}
	if want := uint32(9000 - (window - 1)); effective != want {
		t.Fatalf("effective floor = %d, want %d (the oldest of the contract's most recent %d active ledgers)",
			effective, want, window)
	}
	if effective <= 100 {
		t.Fatal("effective floor did not narrow the requested window")
	}
}

// TestContractInteractions_QuietContractKeepsFullWindow: most contracts have fewer active ledgers than the
// cap, so narrowing their window shrinks the answer for no gain, and the floor never widens the request.
func TestContractInteractions_QuietContractKeepsFullWindow(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		if strings.Contains(q, "contract_active_ledgers") {
			if strings.Contains(q, "WHERE contract_id") {
				return &stubRows{data: [][]any{{uint32(9000)}, {uint32(8000)}}}, nil
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		}
		return &stubRows{data: [][]any{}}, nil
	}
	r := &ExplorerReader{conn: conn}

	_, effective, err := r.ContractInteractions(context.Background(), "CTESTCONTRACT", 50, 100)
	if err != nil {
		t.Fatalf("ContractInteractions: %v", err)
	}
	if effective != 100 {
		t.Fatalf("effective floor = %d, want the requested 100 kept intact for a quiet contract", effective)
	}
}

// TestContractInteractions_CapsDistinctTxsNotEventRows: the inner (ledger_seq, tx_hash) cap subquery must be
// SELECT DISTINCT, or the LIMIT is consumed by event rows and a multi-event contract undercounts shared_txs.
func TestContractInteractions_CapsDistinctTxsNotEventRows(t *testing.T) {
	if !strings.Contains(contractInteractionsQuery,
		"SELECT DISTINCT ledger_seq, tx_hash FROM stellar.contract_events") {
		t.Fatalf("contractInteractionsQuery inner subquery must SELECT DISTINCT (ledger_seq, tx_hash) "+
			"so the subjectTxCap caps transactions, not event rows; got:\n%s", contractInteractionsQuery)
	}

	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "contract_active_ledgers"):
			if strings.Contains(q, "SELECT DISTINCT ledger_seq FROM") { // per-contract walk
				return &stubRows{}, nil // quiet: keep the full requested window
			}
			return &stubRows{data: [][]any{{uint32(1)}}}, nil // probe present
		default:
			// Outer interactions scan: the DISTINCT-capped IN set must be present.
			if !strings.Contains(q, "SELECT DISTINCT ledger_seq, tx_hash") {
				t.Fatalf("interactions query lost its DISTINCT tx cap: %s", q)
			}
			return &stubRows{data: [][]any{{"CPEER", int64(4)}}}, nil
		}
	}
	r := &ExplorerReader{conn: conn}

	edges, _, err := r.ContractInteractions(context.Background(), "CTESTCONTRACT", 50, 100)
	if err != nil {
		t.Fatalf("ContractInteractions: %v", err)
	}
	if len(edges) != 1 || edges[0].SharedTxs != 4 {
		t.Fatalf("edges = %+v, want one peer with 4 shared txs", edges)
	}
}
