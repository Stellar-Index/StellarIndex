package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Query-shape regressions for two ch-query-semantics defects.
// Both are ReplacingMergeTree over-count / mis-cap defects whose live
// proof against real un-merged parts lives in test/integration (tagged
// integration); the stubConn harness cannot model RMT merge semantics, so
// these assert the emitted SQL carries the dedup / distinct-cap construct the
// fix installs — the same query-SHAPE idiom the ContractEventsRecent dedup tests use.

// TestContractActivitySummaryFor_DedupsRMTCount:
// ContractActivitySummaryFor reads stellar.contract_active_ledgers (an RMT);
// a bare count() would let overlapping-backfill duplicate parts inflate
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
