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
	// audit W4-storage-1: the duplicate part must still collapse — now in Go.
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

// TestContractEventsRecent_ActiveLedgerBound — with the
// contract_active_ledgers index present, the reader walks the contract's
// recent active ledgers and bounds the events read to them (the
// quiet-contract fix, site audit 2026-08-07/08). A NON-EMPTY walk bounds
// the events read; an EMPTY walk is NOT authoritative (audit
// W1-chrollup-3) — see TestContractEventsRecent_EmptyWalkFallsThrough.
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

	rows, err := r.ContractEventsRecent(context.Background(), "CTESTCONTRACT", 100, ContractEventsCursor{})
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

// TestContractEventsRecent_EmptyWalkFallsThrough is the audit
// W1-chrollup-3 regression: the contract_active_ledgers probe is a
// LIMIT-1 table-global emptiness check that cannot see PARTIAL backfill
// coverage. When the index is present-but-still-backfilling, a quiet
// contract's per-contract walk comes back EMPTY even though its events
// exist in contract_events. The reader must NOT short-circuit to "no
// events" — it must fall through to the UNBOUNDED contract_events scan
// (the source of truth) and serve the real events.
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
	// audit W4-storage-1: ledger+tx_hash-scoped so FINAL stays cheap (partition +
	// primary-key-prefix bounded), matching the byte-twin OperationsByTx.
	if !strings.Contains(q, "FROM stellar.contract_events FINAL") {
		t.Fatalf("query = %q, want `FROM stellar.contract_events FINAL`", q)
	}
}

// TestSACAssetFromEvents_ActiveLedgerBound — the /wasm 404 path asks
// "is this contract a SAC?" for every contract with no captured
// instance, i.e. the COMMON case. Unbounded, that probe is the
// quiet-contract reverse-scan trap: `contract_id = ? ORDER BY
// ledger_seq DESC LIMIT 1` walks the whole key range backwards, cost
// ~0.34s idle and 8s (the request deadline) under a page's own
// concurrency — 23 of 25 cold random contract pages breached the 1s
// budget on this single call (sub-second audit 2026-08-13). With the
// index present the read MUST be bounded to the contract's own recent
// active ledgers.
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

// TestSACAssetFromEvents_NoActiveLedgersSkipsScan — a contract the
// index knows has NO active ledgers has no events to inspect, so the
// answer is authoritative and the events table must not be touched at
// all. Falling back to the unbounded scan here would reintroduce the
// full cost on exactly the contracts that are cheapest to answer.
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

// TestContractInteractions_BusyContractWindowNarrows — both halves of
// the interactions read scale with the ledger SPAN they cover, so over
// the default 90 days a busy contract cost 3-6s and was the slowest
// panel left on the contract page. The window is anchored to the
// contract's own recent activity instead, and the EFFECTIVE floor must
// be returned so the response's since_ledger describes what was
// actually served rather than what was asked for.
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

// TestContractInteractions_QuietContractKeepsFullWindow — most
// contracts have fewer active ledgers than the cap. Narrowing THEIR
// window would silently shrink the answer for no performance reason,
// and the floor must never widen past what the caller asked for.
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
