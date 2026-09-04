package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The per-contract audit-trail read behind GET
// /v1/contracts/{id}/transfers. Its cost class is the contract: an
// unbounded read of a busy contract's rows has to sort every row that
// contract owns in the newest chunk before the LIMIT can take any, which
// is what made the OpenAPI example contract (the USDC SAC, the busiest
// token on the network) spend the handler's whole 8s budget and 503
// while quiet contracts answered in 0.19s. These pin the lookback ladder
// that mitigated it — and that full history is still reachable through it.

const (
	sep41LadderContract = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	sep41LadderFrom     = "GBFROM00000000000000000000000000000000000000000000000000"
	sep41LadderTo       = "GBTO0000000000000000000000000000000000000000000000000000"
	sep41LadderTxHash   = "aa10000000000000000000000000000000000000000000000000000000000000"
	// Maximum i128 — the amount proves the read maps through big.Int and
	// never narrows to float64/int64 (ADR-0003).
	sep41LadderAmount = "170141183460469231731687303715884105727"
)

var sep41LadderCols = []string{
	"ledger_close_time", "ledger", "tx_hash", "op_index", "event_index",
	"contract_id", "event_kind", "from_addr", "to_addr",
	"amount", "live_until_ledger", "authorized",
}

// sep41LadderRows builds n scripted transfer rows, newest first.
func sep41LadderRows(at time.Time, n int) [][]driver.Value {
	rows := make([][]driver.Value, 0, n)
	for i := range n {
		rows = append(rows, []driver.Value{
			at.Add(-time.Duration(i) * time.Second), int64(63_000_000 - i), sep41LadderTxHash,
			int64(0), int64(i),
			sep41LadderContract, "transfer", sep41LadderFrom, sep41LadderTo,
			sep41LadderAmount, nil, nil,
		})
	}
	return rows
}

// windowFloor returns the `ledger_close_time >= $n` bound of a recorded
// statement, and whether it carried one at all.
func windowFloor(t *testing.T, stmt recordedStmt) (time.Time, bool) {
	t.Helper()
	if !strings.Contains(stmt.sql, "ledger_close_time >= $") {
		return time.Time{}, false
	}
	// The floor is always bound immediately before the LIMIT.
	v := stmt.arg(t, len(stmt.args)-1)
	ts, ok := v.(time.Time)
	if !ok {
		t.Fatalf("window floor arg is %T, want time.Time\nSQL: %s", v, stmt.sql)
	}
	return ts, true
}

// TestListSEP41Transfers_BusyContractReadsOneBoundedWindow is the
// regression: the read a caller makes with no from/to filter must reach
// Postgres with a trailing-window floor, so the LIMIT can stop at an
// index scan instead of sorting every row the contract owns in the
// newest chunk. Without the floor this read is the 8s
// sep41-transfers-timeout 503 on the contract the OpenAPI spec names as
// its own example.
func TestListSEP41Transfers_BusyContractReadsOneBoundedWindow(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: sep41LadderCols,
		rows: sep41LadderRows(now.Add(-time.Minute), 2),
	})

	got, err := store.listSEP41TransfersAt(context.Background(), sep41LadderContract, "", "", 2, now, SEP41TransferLadderBudget)
	if err != nil {
		t.Fatalf("listSEP41TransfersAt: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].Amount == nil || got[0].Amount.String() != sep41LadderAmount {
		t.Errorf("amount = %v, want the i128 %s", got[0].Amount, sep41LadderAmount)
	}

	stmt := conn.only(t)
	floor, bounded := windowFloor(t, stmt)
	if !bounded {
		t.Fatalf("busy-contract read went out unbounded — the full-chunk sort that 503s:\n%s", stmt.sql)
	}
	if want := now.Add(-sep41TransferLookbackLadder[0]); !floor.Equal(want) {
		t.Errorf("window floor = %s, want the first rung %s", floor, want)
	}
	if floor.Location() != time.UTC {
		t.Errorf("window floor location = %s, want UTC", floor.Location())
	}
	if a, b := stmt.arg(t, 1), stmt.arg(t, 3); a != sep41LadderContract || b != 2 {
		t.Errorf("args = ($1 %v, $3 %v), want (contract, limit) — an unfiltered read binds only those two around the floor", a, b)
	}
}

// TestListSEP41Transfers_StopsAtTheFirstFullPage — a rung that fills the
// page is the answer: every row a floor excludes is strictly older than
// every row it keeps, so escalating further could only re-read the same
// page more expensively.
func TestListSEP41Transfers_StopsAtTheFirstFullPage(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store, conn := newScriptedStore(t,
		scriptedResult{cols: sep41LadderCols, rows: sep41LadderRows(now.Add(-30*time.Minute), 1)},
		scriptedResult{cols: sep41LadderCols, rows: sep41LadderRows(now.Add(-5*time.Hour), 2)},
	)

	got, err := store.listSEP41TransfersAt(context.Background(), sep41LadderContract, "", "", 2, now, SEP41TransferLadderBudget)
	if err != nil {
		t.Fatalf("listSEP41TransfersAt: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want the second rung's full page of 2", len(got))
	}
	if len(conn.stmts) != 2 {
		t.Fatalf("issued %d statements, want 2 (first rung short, second full):\n%s",
			len(conn.stmts), strings.Join(conn.statements(), "\n---\n"))
	}
	for i, stmt := range conn.stmts {
		floor, bounded := windowFloor(t, stmt)
		if !bounded {
			t.Fatalf("rung %d went out unbounded:\n%s", i, stmt.sql)
		}
		if want := now.Add(-sep41TransferLookbackLadder[i]); !floor.Equal(want) {
			t.Errorf("rung %d floor = %s, want %s", i, floor, want)
		}
	}
}

// TestListSEP41Transfers_FallsThroughToFullHistory — the ladder is a
// cost bound, never a claim about history. A contract too quiet to fill
// its page in the widest rung still gets the unbounded read, which is
// what keeps a long-dormant token's audit trail readable (quiet is not
// stale).
func TestListSEP41Transfers_FallsThroughToFullHistory(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	script := make([]scriptedResult, 0, len(sep41TransferLookbackLadder)+1)
	for range sep41TransferLookbackLadder {
		script = append(script, scriptedResult{cols: sep41LadderCols})
	}
	script = append(script, scriptedResult{
		cols: sep41LadderCols,
		rows: sep41LadderRows(now.Add(-400*24*time.Hour), 1),
	})
	store, conn := newScriptedStore(t, script...)

	got, err := store.listSEP41TransfersAt(context.Background(), sep41LadderContract, "", "", 2, now, SEP41TransferLadderBudget)
	if err != nil {
		t.Fatalf("listSEP41TransfersAt: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want the 1 pre-window row the fallback found", len(got))
	}
	if len(conn.stmts) != len(sep41TransferLookbackLadder)+1 {
		t.Fatalf("issued %d statements, want every rung plus the fallback:\n%s",
			len(conn.stmts), strings.Join(conn.statements(), "\n---\n"))
	}
	last := conn.stmts[len(conn.stmts)-1]
	if _, bounded := windowFloor(t, last); bounded {
		t.Errorf("the fallback read still carries a window floor — history older than the widest rung would be unreachable:\n%s", last.sql)
	}
	if a, b := last.arg(t, 1), last.arg(t, 2); a != sep41LadderContract || b != 2 {
		t.Errorf("fallback args = (%v, %v), want (contract, limit)", a, b)
	}
}

// TestSEP41TransfersQuery_PlaceholderBinding pins the numbering against
// the args for every shape the optional predicates produce — the whole
// reason query text and args are built by one function.
func TestSEP41TransfersQuery_PlaceholderBinding(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name     string
		from, to string
		since    time.Time
		want     []string // predicate text, in placeholder order after $1
	}{
		{name: "contract only"},
		{name: "windowed", since: since, want: []string{"ledger_close_time >= $2"}},
		{name: "from", from: sep41LadderFrom, want: []string{"from_addr = $2"}},
		{
			name: "from windowed", from: sep41LadderFrom, since: since,
			want: []string{"from_addr = $2", "ledger_close_time >= $3"},
		},
		{
			name: "from and to windowed", from: sep41LadderFrom, to: sep41LadderTo, since: since,
			want: []string{"from_addr = $2", "to_addr = $3", "ledger_close_time >= $4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, args := sep41TransfersQuery(sep41LadderContract, tc.from, tc.to, 100, tc.since)
			if !strings.Contains(q, "contract_id = $1") {
				t.Errorf("query lost the contract predicate:\n%s", q)
			}
			for _, pred := range tc.want {
				if !strings.Contains(q, pred) {
					t.Errorf("query missing %q:\n%s", pred, q)
				}
			}
			if tc.since.IsZero() && strings.Contains(q, "ledger_close_time >=") {
				t.Errorf("zero `since` must build the unbounded form:\n%s", q)
			}
			// The limit is always last, and every placeholder the text
			// names must have an arg behind it.
			if n := len(args); !strings.Contains(q, "LIMIT $"+strconv.Itoa(n)) {
				t.Errorf("LIMIT placeholder is not $%d:\n%s", n, q)
			}
			if got, want := len(args), len(tc.want)+2; got != want {
				t.Errorf("bound %d args, want %d (contract + %d predicates + limit)", got, want, len(tc.want))
			}
			if !strings.Contains(q, "ORDER BY ledger_close_time DESC, ledger DESC, op_index DESC") {
				t.Errorf("query lost its newest-first order — the ladder's short-circuit is only sound on it:\n%s", q)
			}
		})
	}
}

// TestSEP41TransferLookbackLadder_WidestRungIsTheMeasuredCeiling guards
// the ladder against being widened back into the cost class it exists
// to avoid. 7d is the widest window measured on r1 to still plan onto
// an index scan; 90d plans back to the per-chunk `Seq Scan + Sort`, so
// a wider rung is not a deeper cheap read — it is the timeout again,
// just later in the ladder. The ceiling is NOT "one chunk": migration
// 0047 declares 1-day chunks and 7-day compression, so on a tree-built
// deployment the 7d rung already spans up to eight chunks and reaches
// the compression boundary; r1's 30-day chunk width, which puts every
// rung inside one chunk, is drift from that migration, not a property
// the ladder may rely on.
func TestSEP41TransferLookbackLadder_WidestRungIsTheMeasuredCeiling(t *testing.T) {
	t.Parallel()
	if len(sep41TransferLookbackLadder) == 0 {
		t.Fatal("ladder is empty — every read falls straight through to the unbounded scan")
	}
	prev := time.Duration(0)
	for i, w := range sep41TransferLookbackLadder {
		if w <= prev {
			t.Errorf("rung %d = %s is not wider than rung %d (%s); the ladder must escalate", i, w, i-1, prev)
		}
		prev = w
	}
	if last := sep41TransferLookbackLadder[len(sep41TransferLookbackLadder)-1]; last > 7*24*time.Hour {
		t.Errorf("widest rung = %s; want <= 7d", last)
	}
}

// TestListSEP41Transfers_BudgetAbandonsAStalledRung is the ladder's cost
// bound. A rung the database has not answered when the ladder's budget
// runs out is abandoned, no wider rung is tried, and the fallback goes
// out on the caller's OWN deadline — so the ladder can never spend the
// handler's 8s on the way to the query that has the rows. Measured on
// r1, a 7d rung that cannot fill its page for a contract the chunk
// statistics still call busy was still running at a 9s statement
// timeout: without this bound that one rung 503s a request the
// unbounded read would have answered.
func TestListSEP41Transfers_BudgetAbandonsAStalledRung(t *testing.T) {
	const budget = 50 * time.Millisecond
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-400 * 24 * time.Hour)
	store, conn := newScriptedStore(t,
		scriptedResult{cols: sep41LadderCols, rows: sep41LadderRows(now.Add(-30*time.Minute), 1)},
		scriptedResult{stall: true},
		scriptedResult{cols: sep41LadderCols, rows: sep41LadderRows(oldest, 1)},
	)
	// The handler's deadline, so the fallback has one to be checked
	// against.
	callerDeadline := time.Now().Add(8 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()

	start := time.Now()
	got, err := store.listSEP41TransfersAt(ctx, sep41LadderContract, "", "", 2, now, budget)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("listSEP41TransfersAt: %v — a rung stalled past the ladder's budget must fall back, not fail", err)
	}
	if len(got) != 1 || !got[0].ObservedAt.Equal(oldest) {
		t.Fatalf("got %d rows, want the fallback's single pre-window row", len(got))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("read took %s against a %s ladder budget — the stalled rung ran on the caller's budget, not the ladder's", elapsed, budget)
	}
	if len(conn.stmts) != 3 {
		t.Fatalf("issued %d statements, want 3 (first rung short, second stalled, then the fallback — no wider rung after the budget is spent):\n%s",
			len(conn.stmts), strings.Join(conn.statements(), "\n---\n"))
	}

	stalled := conn.stmts[1]
	if _, bounded := windowFloor(t, stalled); !bounded {
		t.Errorf("the stalled statement was not a rung:\n%s", stalled.sql)
	}
	if stalled.deadline.IsZero() || !stalled.deadline.Before(callerDeadline) {
		t.Errorf("stalled rung's deadline = %s, want one earlier than the caller's %s — the rung ran on the caller's budget", stalled.deadline, callerDeadline)
	}
	if latest := start.Add(budget + time.Second); stalled.deadline.After(latest) {
		t.Errorf("stalled rung's deadline = %s, want within the %s ladder budget of %s", stalled.deadline, budget, start)
	}

	fallback := conn.stmts[2]
	if _, bounded := windowFloor(t, fallback); bounded {
		t.Errorf("the fallback read carries a window floor:\n%s", fallback.sql)
	}
	if !fallback.deadline.Equal(callerDeadline) {
		t.Errorf("fallback deadline = %s, want the caller's own %s — the fallback must not inherit the ladder's spent budget", fallback.deadline, callerDeadline)
	}
}

// TestListSEP41Transfers_CallerDeadlineIsTheCallersNotTheLadders — the
// budget is derived from the caller's context, never detached from it.
// A caller with less time than the ladder's budget gets its own
// deadline back, promptly and as context.DeadlineExceeded (which the
// handler maps to its timeout 503); no fallback is attempted on a
// context that is already dead, because there is nothing left to run
// it on.
func TestListSEP41Transfers_CallerDeadlineIsTheCallersNotTheLadders(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{stall: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := store.listSEP41TransfersAt(ctx, sep41LadderContract, "", "", 2, now, SEP41TransferLadderBudget)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's context.DeadlineExceeded to surface", err)
	}
	if elapsed > time.Second {
		t.Fatalf("read took %s against a 30ms caller deadline — the ladder's %s budget was applied on top of the caller's, not inside it", elapsed, SEP41TransferLadderBudget)
	}
	if len(conn.stmts) != 1 {
		t.Fatalf("issued %d statements, want only the stalled rung — a fallback on a dead context is a wasted statement:\n%s",
			len(conn.stmts), strings.Join(conn.statements(), "\n---\n"))
	}
}
