package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Query-shape regressions for the wave-4 ch-query-semantics findings (CHQ-1,
// CHQ-2). Both are ReplacingMergeTree over-count / mis-cap defects whose live
// proof against real un-merged parts lives in test/integration (tagged
// integration); the stubConn harness cannot model RMT merge semantics, so
// these assert the emitted SQL carries the dedup / distinct-cap construct the
// fix installs — the same query-SHAPE idiom the W4-storage-1 tests use.

// TestContractActivitySummaryFor_DedupsRMTCount — audit CHQ-2:
// ContractActivitySummaryFor read stellar.contract_active_ledgers (an RMT)
// with a bare count(), so overlapping-backfill duplicate parts inflated
// ActiveLedgersTotal (a headline card number) and the daily bars up to ~2x
// until a background merge. Both the total and the per-day aggregate must
// count DISTINCT ledger_seq (uniqExact), matching the sibling reader
// contractActiveLedgers' SELECT DISTINCT on this same table.
func TestContractActivitySummaryFor_DedupsRMTCount(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "contract_active_ledgers LIMIT 1"): // probe
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case strings.Contains(q, "min(close_time)"): // bounds + total
			if strings.Contains(q, "count()") {
				t.Fatalf("total query uses bare count() over un-merged RMT parts: %s", q)
			}
			if !strings.Contains(q, "uniqExact(ledger_seq)") {
				t.Fatalf("total query = %q, want toUInt64(uniqExact(ledger_seq))", q)
			}
			return &stubRows{data: [][]any{{
				time.Unix(1700000000, 0).UTC(), time.Unix(1700100000, 0).UTC(), uint64(7),
			}}}, nil
		default: // daily series
			if strings.Contains(q, "count()") {
				t.Fatalf("daily series uses bare count() over un-merged RMT parts: %s", q)
			}
			if !strings.Contains(q, "uniqExact(ledger_seq)") {
				t.Fatalf("daily query = %q, want toUInt64(uniqExact(ledger_seq)) per day", q)
			}
			return &stubRows{data: [][]any{
				{time.Unix(1700000000, 0).UTC(), uint64(3)},
			}}, nil
		}
	}
	r := &ExplorerReader{conn: conn}

	s, ok, err := r.ContractActivitySummaryFor(context.Background(), "CTESTCONTRACT", 30)
	if err != nil || !ok {
		t.Fatalf("ContractActivitySummaryFor: ok=%v err=%v", ok, err)
	}
	if s.ActiveLedgersTotal != 7 {
		t.Fatalf("ActiveLedgersTotal = %d, want 7", s.ActiveLedgersTotal)
	}
	if len(s.Daily) != 1 || s.Daily[0].ActiveLedgers != 3 {
		t.Fatalf("daily = %+v, want one day with 3 active ledgers", s.Daily)
	}
}

// TestContractInteractions_CapsDistinctTxsNotEventRows — audit CHQ-1: the
// subject-tx cap subquery had no DISTINCT, so the 50k LIMIT was consumed by
// EVENT rows, not transactions — a contract emitting ~10-20 events/tx sampled
// only ~2.5-5k txs from the newest handful of ledgers, undercounting the
// shared-tx ranking. The inner (ledger_seq, tx_hash) collection must be
// SELECT DISTINCT so the cap counts transactions, as its comment and the
// shared_txs column name have always claimed.
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
