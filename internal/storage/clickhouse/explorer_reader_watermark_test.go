package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Tests for the per-account activity watermark bound: AccountOperations'
// key arms read `ORDER BY pk DESC LIMIT n`, which streams granules backwards
// from the TIP until the account's rows turn up (~4 s for a 46d-idle account). With a watermark
// row in stellar.account_activity the reader bounds EACH arm's resolve with
// `ledger_seq <= watermark`, so the reverse read starts at the account's real
// last activity. These pin the emitted SQL and the bind order (stubConn from
// tx_hash_index_test.go — SQL shape assertions, same convention as
// explorer_reader_union_bounds_test.go); the live-ClickHouse correctness proof
// — bounded rows == unbounded rows, incl. a participant row ABOVE the sourced
// watermark — is test/integration/lake_test.go.

// Query-shape classifiers for the watermark reads.
func isAccountActivityProbe(q string) bool {
	return strings.Contains(q, "stellar.account_activity") && !strings.Contains(q, "WHERE")
}

func isAccountActivityLookup(q string) bool {
	return strings.Contains(q, "max(last_ledger)") &&
		strings.Contains(q, "stellar.account_activity") &&
		strings.Contains(q, "WHERE account_id = ?")
}

// watermarkStubConn routes the ops_by_source probe and the account_activity
// probe + lookup to real single-column rows (so the reader takes the bounded
// path) and everything else to `rows`.
func watermarkStubConn(watermark uint32, rows *stubRows) *stubConn {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isOpsBySourceProbe(q):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case isAccountActivityProbe(q):
			return &stubRows{data: [][]any{{watermark}}}, nil
		case isAccountActivityLookup(q):
			return &stubRows{data: [][]any{{watermark}}}, nil
		default:
			return rows, nil
		}
	}
	return conn
}

// keyReads returns the emitted key reads (ops_by_source / participants) and
// their args.
func keyReads(conn *stubConn) (qs []string, args [][]any) {
	for i, q := range conn.queries {
		if isOpsBySourceProbe(q) || isVisibilityLookup(q) {
			continue
		}
		if strings.Contains(q, "FROM stellar.ops_by_source") || strings.Contains(q, "FROM stellar.operation_participants") {
			qs, args = append(qs, q), append(args, conn.args[i])
		}
	}
	return qs, args
}

func TestAccountOperations_WatermarkBoundsEachArmResolve(t *testing.T) {
	const (
		limit     = 37
		watermark = uint32(63_411_270)
	)
	conn := watermarkStubConn(watermark, &stubRows{})
	if _, _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	// BOTH arms must carry the bound — a bound on one arm, or on the
	// hydration only, leaves the other arm's reverse read walking the tip.
	qs, args := keyReads(conn)
	if len(qs) != 2 {
		t.Fatalf("key reads = %d, want 2 (sourced + participant windows): %v", len(qs), conn.queries)
	}
	for i, q := range qs {
		if !strings.Contains(q, "AND ledger_seq <= ?") || args[i][1] != watermark {
			t.Errorf("key read %d is unbounded or binds %v as the bound (#31):\n%s", i, args[i], q)
		}
	}
	q, got := exactOpsArm(t, watermarkStubConn(watermark, &stubRows{}).respond, limit, ExplorerCursor{})
	assertArgs(t, q, got, []any{"GTEST", watermark, limit})
}

func TestAccountOperations_WatermarkPreservesCursorArgOrder(t *testing.T) {
	const (
		limit     = 9
		watermark = uint32(63_411_270)
	)
	cur := ExplorerCursor{Ledger: 63_000_000, A: 4, B: 2}
	q, got := exactOpsArm(t, watermarkStubConn(watermark, &stubRows{}).respond, limit, cur)
	// The bound precedes the cursor tuple in the text, so it must in the args.
	if i, j := strings.Index(q, "ledger_seq <= ?"), strings.Index(q, "< (?, ?, ?)"); !(i >= 0 && j > i) {
		t.Fatalf("bound clause must precede the cursor clause in the emitted SQL:\n%s", q)
	}
	assertArgs(t, q, got, []any{"GTEST", watermark, cur.Ledger, cur.Ledger, cur.A, cur.B, limit})
}

// TestAccountOperations_NoWatermarkFallsBackUnbounded pins the degrade
// direction: no watermark row (max() over zero rows scans as 0) → no bound on
// any key read. A missing watermark may only cost performance; a fabricated
// bound of 0 would hide the account's whole history.
func TestAccountOperations_NoWatermarkFallsBackUnbounded(t *testing.T) {
	const limit = 37
	conn := watermarkStubConn(0, &stubRows{})
	if _, _, err := (&ExplorerReader{conn: conn}).AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	qs, _ := keyReads(conn)
	for _, q := range qs {
		if strings.Contains(q, "ledger_seq <= ?") {
			t.Fatalf("no watermark, yet a key read carries a bound:\n%s", q)
		}
	}
	q, got := exactOpsArm(t, watermarkStubConn(0, &stubRows{}).respond, limit, ExplorerCursor{})
	assertArgs(t, q, got, []any{"GTEST", limit})
}

func assertArgs(t *testing.T, q string, got, want []any) {
	t.Helper()
	if n := strings.Count(q, "?"); n != len(got) {
		t.Fatalf("query has %d placeholders, reader bound %d args: %v\n%s", n, len(got), got, q)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}
