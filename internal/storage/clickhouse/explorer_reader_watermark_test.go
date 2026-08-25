package clickhouse

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Tests for the per-account activity watermark bound (#31): AccountOperations'
// keyset arms resolve with `ORDER BY pk DESC LIMIT n` over stellar.operations,
// which streams granules backwards from the TIP until the account's rows turn
// up — measured ~4s live for a 46d-idle account (2026-08-24). With a watermark
// row in stellar.account_activity the reader bounds EACH arm's resolve with
// `ledger_seq <= watermark`, so the reverse read starts at the account's real
// last activity. These pin the emitted SQL and the bind order (stubConn from
// tx_hash_index_test.go — SQL shape assertions, same convention as
// explorer_reader_union_bounds_test.go); the live-ClickHouse correctness proof
// — bounded rows == unbounded rows, incl. a participant row ABOVE the sourced
// watermark — is test/integration/account_activity_watermark_test.go.

// Query-shape classifiers for the watermark reads.
func isAccountActivityProbe(q string) bool {
	return strings.Contains(q, "stellar.account_activity") && !strings.Contains(q, "WHERE")
}

func isAccountActivityLookup(q string) bool {
	return strings.Contains(q, "max(last_ledger)") &&
		strings.Contains(q, "stellar.account_activity") &&
		strings.Contains(q, "WHERE account_id = ?")
}

// watermarkStubConn routes the account_activity probe + lookup to real
// single-column rows (so the reader takes the bounded path) and everything
// else to `rows`.
func watermarkStubConn(watermark uint32, rows *stubRows) *stubConn {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
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

func TestAccountOperations_WatermarkBoundsEachArmResolve(t *testing.T) {
	const (
		limit     = 37
		watermark = uint32(63_411_270)
		account   = "GTEST_WATERMARK"
	)
	conn := watermarkStubConn(watermark, &stubRows{})
	r := &ExplorerReader{conn: conn}

	if _, err := r.AccountOperations(context.Background(), account, limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	q := conn.queries[len(conn.queries)-1]
	arm1, arm2 := armBodies(t, q)

	// BOTH arms must carry the bound — a sourced-only bound would still walk
	// the tip for the participant arm, and (worse) a bound on the OUTER
	// hydration only would not stop either arm's reverse read.
	for i, arm := range []string{arm1, arm2} {
		if !strings.Contains(arm, "AND ledger_seq <= ?") {
			t.Errorf("arm %d resolve is unbounded — the reverse primary-key read walks granules "+
				"from the tip instead of starting at the account's last activity (#31):\n%s", i+1, arm)
		}
	}

	// Bind order must mirror the SQL text: account, bound, page size per arm,
	// then the keyset + hydration LIMITs. A transposed slice would bind a
	// ledger number as a page size.
	want := []any{
		account, watermark, limit, // arm 1: account, watermark bound, page size
		account, watermark, limit, // arm 2: same
		limit, // keyset merge
		limit, // hydration pass
	}
	got := conn.args[len(conn.args)-1]
	if len(got) != len(want) {
		t.Fatalf("bound %d args, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestAccountOperations_WatermarkPreservesCursorArgOrder(t *testing.T) {
	const (
		limit     = 9
		watermark = uint32(63_411_270)
	)
	conn := watermarkStubConn(watermark, &stubRows{})
	r := &ExplorerReader{conn: conn}

	cur := ExplorerCursor{Ledger: 63_000_000, A: 4, B: 2}
	if _, err := r.AccountOperations(context.Background(), "GTEST", limit, cur); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	q := conn.queries[len(conn.queries)-1]
	// The bound precedes the cursor tuple in each arm's text, so it must
	// precede it in the args too.
	if i, j := strings.Index(q, "ledger_seq <= ?"), strings.Index(q, "< (?, ?, ?)"); !(i >= 0 && j > i) {
		t.Fatalf("bound clause must precede the cursor clause in the emitted SQL:\n%s", q)
	}
	want := []any{
		"GTEST", watermark, cur.Ledger, cur.A, cur.B, limit, // arm 1
		"GTEST", watermark, cur.Ledger, cur.A, cur.B, limit, // arm 2
		limit, // keyset merge
		limit, // hydration pass
	}
	got := conn.args[len(conn.args)-1]
	if len(got) != len(want) {
		t.Fatalf("bound %d args, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestAccountOperations_NoWatermarkFallsBackUnbounded pins the degrade
// direction: no watermark row for the account (max() over zero rows scans as
// 0) → the emitted SQL and args are EXACTLY the pre-watermark shape. A
// missing watermark may only cost performance — it must never bound (a
// fabricated bound of 0 would hide the account's whole history).
func TestAccountOperations_NoWatermarkFallsBackUnbounded(t *testing.T) {
	const limit = 37
	conn := watermarkStubConn(0, &stubRows{})
	r := &ExplorerReader{conn: conn}

	if _, err := r.AccountOperations(context.Background(), "GTEST", limit, ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	q := conn.queries[len(conn.queries)-1]
	if strings.Contains(q, "ledger_seq <= ?") {
		t.Fatalf("no watermark, yet the query carries a bound — a zero/absent watermark must fall "+
			"back to the unbounded resolve, never bound at 0 (hides all history):\n%s", q)
	}
	want := []any{"GTEST", limit, "GTEST", limit, limit, limit}
	got := conn.args[len(conn.args)-1]
	if len(got) != len(want) {
		t.Fatalf("bound %d args, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}
}
