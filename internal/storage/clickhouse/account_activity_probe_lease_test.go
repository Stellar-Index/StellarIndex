package clickhouse

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// accountActivityLake models ONE ClickHouse under ONE long-lived reader for
// stellar.account_activity: a watermark table that starts holding rows and
// can be TRUNCATEd (rows removed, object still present) under the running
// process — the same "empties mid-process" shape F119 fixed for
// tx_hash_index, exercised here through accountActivityAvailable /
// accountActivityWatermark, F120's second cited call site. Query-shape
// classifiers (isAccountActivityProbe, isAccountActivityLookup) are shared
// with explorer_reader_watermark_test.go.
type accountActivityLake struct {
	rows bool
}

func (l *accountActivityLake) respond(q string) (driver.Rows, error) {
	switch {
	case isAccountActivityProbe(q):
		if l.rows {
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		}
		return &stubRows{}, nil
	case isAccountActivityLookup(q):
		if l.rows {
			return &stubRows{data: [][]any{{uint32(555)}}}, nil
		}
		return &stubRows{}, nil
	default:
		return nil, fmt.Errorf("unexpected query: %s", q)
	}
}

// TestAccountActivityWatermark_PositiveLeaseRenewsAfterTruncate pins F120
// against its SECOND cited site (accountActivityAvailable, explorer_reader.go
// pre-fix ~1256-1300): the requireRows lease landed for F119 is the SAME
// shared primitive every requireRows probe goes through, not a special case
// wired only for tx_hash_index. Absent the lease, a positive verdict here
// would never be re-confirmed for the process lifetime: the probe query
// (`SELECT last_ledger … LIMIT 1`) would run exactly once, cold, and never
// again — including after stellar.account_activity is TRUNCATEd under the
// running reader. The lease requires ONE renewal probe once it expires.
func TestAccountActivityWatermark_PositiveLeaseRenewsAfterTruncate(t *testing.T) {
	lake := &accountActivityLake{rows: true}
	conn := &stubConn{respond: lake.respond}
	clk := newFakeClock()
	r := &ExplorerReader{conn: conn, accountActivityProbe: schemaProbe{now: clk.now}}
	ctx := context.Background()

	last, ok := r.accountActivityWatermark(ctx, "GACCOUNTAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if !ok || last != 555 {
		t.Fatalf("cold: (last=%d, ok=%v), want (555, true)", last, ok)
	}
	if n := countQueries(conn.queries, isAccountActivityProbe); n != 1 {
		t.Fatalf("cold: probes = %d, want 1", n)
	}

	// TRUNCATE stellar.account_activity: the object still exists but now
	// holds nothing — the MV-drop / TRUNCATE pathology the requireRows
	// guard exists to catch.
	lake.rows = false
	clk.advance(schemaProbeLease + time.Second)

	if _, ok := r.accountActivityWatermark(ctx, "GACCOUNTAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); ok {
		t.Fatal("watermark still reported available once the backing table was truncated under a live reader")
	}
	if n := countQueries(conn.queries, isAccountActivityProbe); n != 2 {
		t.Fatalf("probes after the lease expired = %d, want 2 (cold start + one lease renewal) — "+
			"a positive requireRows verdict must not be latched for the process lifetime", n)
	}
}
