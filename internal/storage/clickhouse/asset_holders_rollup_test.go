// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func isHoldersProbe(q string) bool {
	return strings.Contains(q, "SELECT rank FROM stellar.asset_holders_rollup") && strings.Contains(q, "LIMIT 1")
}

func isHoldersRows(q string) bool {
	return strings.Contains(q, "stellar.asset_holders_rollup") && strings.Contains(q, "account_id, balance, computed_at")
}

func isHoldersCount(q string) bool {
	return strings.Contains(q, "stellar.asset_holders_counts")
}

// TestHoldersRollupSwapIsAtomic pins RA-2: the five live↔staging swaps must be
// issued as ONE multi-pair EXCHANGE TABLES statement, not five sequential
// EXCHANGE calls. ClickHouse commits a multi-pair EXCHANGE as a single metadata
// transaction, so a crash/ctx-cancel/CH-restart between swaps cannot leave the
// board swapped-new while counts/stats/histograms hold the previous cycle's
// data — the half-swapped state holdersRollupBoard trusts as authoritative.
//
// Proven red against the pre-fix code (five separate EXCHANGE statements): the
// exactly-one assertion counts 5 and fails.
func TestHoldersRollupSwapIsAtomic(t *testing.T) {
	// All five live tables that must swap as a group.
	wantPairs := []string{
		"stellar.asset_holders_rollup_staging AND stellar.asset_holders_rollup",
		"stellar.asset_holders_counts_staging AND stellar.asset_holders_counts",
		"stellar.accounts_stats_staging AND stellar.accounts_stats",
		"stellar.accounts_wealth_histogram_staging AND stellar.accounts_wealth_histogram",
		"stellar.accounts_trustline_histogram_staging AND stellar.accounts_trustline_histogram",
	}

	stmts := holdersRollupStatements(time.Now())

	// Exactly one statement may contain EXCHANGE TABLES — a single atomic swap.
	var exchangeStmts []string
	for _, s := range stmts {
		if strings.Contains(s, "EXCHANGE TABLES") {
			exchangeStmts = append(exchangeStmts, s)
		}
	}
	if len(exchangeStmts) != 1 {
		t.Fatalf("holdersRollupStatements has %d EXCHANGE TABLES statements, want exactly 1 atomic multi-pair swap (RA-2)", len(exchangeStmts))
	}

	// normalize whitespace so the multi-line SQL literal compares cleanly.
	swap := strings.Join(strings.Fields(exchangeStmts[0]), " ")
	for _, p := range wantPairs {
		if !strings.Contains(swap, p) {
			t.Errorf("atomic EXCHANGE is missing pair %q; swap=%q", p, swap)
		}
	}

	// The atomic swap must be the final statement: every staging arm has to be
	// filled before the group swap fires.
	last := stmts[len(stmts)-1]
	if !strings.Contains(last, "EXCHANGE TABLES") {
		t.Errorf("the atomic EXCHANGE must be the last statement, got %q", strings.Join(strings.Fields(last), " "))
	}
}

// TestHoldersRollupStatementsShareOneCycleStamp pins the property
// readHoldersRollupCycle/readAccountsStatsCycle depend on: every staging
// INSERT in one cycle must bake in the SAME computed_at, not each table's own
// row-level default. Two inserts disagreeing here would make every
// consistency check in holdersRollupBoard/AccountsStats fire on every read,
// even for a perfectly healthy cycle.
//
// Proven red against the pre-fix statements (each relying on its own
// `DEFAULT now()`): the two insert statements checked below carried no
// explicit computed_at value at all, so this substring search fails.
func TestHoldersRollupStatementsShareOneCycleStamp(t *testing.T) {
	cycleAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	stamp := "toDateTime('" + cycleAt.Format(holdersRollupTimeLayout) + "', 'UTC')"

	stmts := holdersRollupStatements(cycleAt)
	checked := 0
	for _, s := range stmts {
		if !strings.Contains(s, "INSERT INTO") {
			continue
		}
		checked++
		if !strings.Contains(s, stamp) {
			t.Errorf("insert statement does not bake in the shared cycle stamp %s:\n%s", stamp, s)
		}
	}
	if checked == 0 {
		t.Fatal("no INSERT statement found — this test would pass vacuously")
	}
}

// TestHoldersRollupBoard_RetriesOnceWhenACycleSwapsMidRead pins T361: a
// group EXCHANGE landing between the board-rows read and the count read must
// not serve a board from one cycle paired with a count from another. The
// stub reports the rows query as still on cycle1 the first time (as if read
// before the swap) and the count query as already on cycle2 (as if read
// after) — the exact shape one EXCHANGE landing between the two queries
// produces — so holdersRollupBoard must retry, and the retried (self-
// consistent, both cycle2) pair is what must be served.
//
// Proven red against the pre-fix holdersRollupBoard, which had no
// computed_at comparison at all: it returned the FIRST (torn) pair —
// balance=100 paired with total=999 — instead of retrying to the
// self-consistent balance=200/total=500.
func TestHoldersRollupBoard_RetriesOnceWhenACycleSwapsMidRead(t *testing.T) {
	cycle1 := time.Now().UTC().Truncate(time.Second).Add(-50 * time.Minute)
	cycle2 := cycle1.Add(30 * time.Minute)

	rowsCalls := 0
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isHoldersProbe(q):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case isHoldersRows(q):
			rowsCalls++
			if rowsCalls == 1 {
				return &stubRows{data: [][]any{{"GHOLDER1", int64(100), cycle1}}}, nil
			}
			return &stubRows{data: [][]any{{"GHOLDER1", int64(200), cycle2}}}, nil
		case isHoldersCount(q):
			// The count side always reports the CURRENT live cycle
			// (cycle2), so read #1's rows (cycle1) mismatch and read #2's
			// rows (cycle2) match.
			return &stubRows{data: [][]any{{int64(500), cycle2}}}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}
	r := &ExplorerReader{conn: conn}

	out, total, ok, err := r.holdersRollupBoard(t.Context(), "USDC-"+testIssuer, 100)
	if err != nil {
		t.Fatalf("holdersRollupBoard: %v", err)
	}
	if !ok {
		t.Fatal("ok = false on a read that resolved consistent after one retry")
	}
	if len(out) != 1 || out[0].Balance != 200 {
		t.Errorf("board = %+v, want balance=200 (the retried, self-consistent read) — 100 paired with total=500 means a torn read reached the caller", out)
	}
	if total != 500 {
		t.Errorf("total = %d, want 500", total)
	}
	if rowsCalls != 2 {
		t.Errorf("board-rows read ran %d time(s), want exactly 2 (one retry after the cycle-stamp mismatch)", rowsCalls)
	}
}
