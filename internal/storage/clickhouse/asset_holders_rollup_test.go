// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// TestHoldersRollupSwapIsAtomic pins that the five live/staging swaps are one multi-pair EXCHANGE TABLES, so
// a crash between swaps cannot leave the board new while counts/stats/histograms hold the previous cycle.
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

// TestHoldersRollupStatementsShareOneCycleStamp: every staging INSERT in a cycle must bake in the same
// computed_at, not each table's `DEFAULT now()`, or the consistency checks trip on every read of a healthy
// cycle.
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

func isHoldersCycleStamp(q string) bool {
	return strings.Contains(q, "SELECT computed_at FROM stellar.asset_holders_rollup LIMIT 1")
}

// holdersRollupLake stubs the rollup reads: the asset's board rows and count
// row (both stamped boardAt; omitted when listed is false) and the live
// table's cycle stamp tableAt.
func holdersRollupLake(t *testing.T, listed bool, boardAt, tableAt time.Time) *stubConn {
	t.Helper()
	return &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case isHoldersProbe(q):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case isHoldersRows(q):
			if !listed {
				return &stubRows{}, nil
			}
			return &stubRows{data: [][]any{{"GHOLDER1", int64(200), boardAt}}}, nil
		case isHoldersCount(q):
			if !listed {
				// max() over no rows: 0 holders, epoch stamp.
				return &stubRows{data: [][]any{{int64(0), time.Unix(0, 0).UTC()}}}, nil
			}
			return &stubRows{data: [][]any{{int64(500), boardAt}}}, nil
		case isHoldersCycleStamp(q):
			return &stubRows{data: [][]any{{tableAt}}}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}
}

// TestHoldersRollupBoard_RefusesAStaleCycle: a board whose cycle stamp is older than
// holdersRollupMaxAge (the rollup timer has wedged) must not be served as the current holders
// board; ok=false sends AssetHolders to the live per-request scans instead.
func TestHoldersRollupBoard_RefusesAStaleCycle(t *testing.T) {
	stale := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	r := &ExplorerReader{conn: holdersRollupLake(t, true, stale, stale)}

	out, total, ok, err := r.holdersRollupBoard(t.Context(), "USDC-"+testIssuer, 100)
	if err != nil {
		t.Fatalf("holdersRollupBoard: %v", err)
	}
	if ok {
		t.Fatalf("ok = true for a cycle stamped %s ago — a wedged rollup's board was served as current: board=%+v total=%d",
			time.Since(stale).Round(time.Minute), out, total)
	}
}

// TestHoldersRollupBoard_RefusesAStaleCycleForAnUnlistedAsset: an asset with no row in either
// table is "authoritatively zero holders" only for a CURRENT cycle. Under a wedged rollup, an
// asset issued since the last cycle would otherwise be served as having no holders.
func TestHoldersRollupBoard_RefusesAStaleCycleForAnUnlistedAsset(t *testing.T) {
	stale := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	r := &ExplorerReader{conn: holdersRollupLake(t, false, time.Time{}, stale)}

	_, total, ok, err := r.holdersRollupBoard(t.Context(), "NEW-"+testIssuer, 100)
	if err != nil {
		t.Fatalf("holdersRollupBoard: %v", err)
	}
	if ok {
		t.Fatalf("ok = true (total=%d) for an unlisted asset under a cycle stamped %s ago — a stale empty board was served as authoritative",
			total, time.Since(stale).Round(time.Minute))
	}
}

// TestHoldersRollupBoard_ServesAFreshCycle is the non-vacuity guard for the
// two tests above: the same shapes with a current stamp must still be served
// from the rollup (a gate that refused everything would pass them too).
func TestHoldersRollupBoard_ServesAFreshCycle(t *testing.T) {
	fresh := time.Now().UTC().Truncate(time.Second).Add(-40 * time.Minute)

	r := &ExplorerReader{conn: holdersRollupLake(t, true, fresh, fresh)}
	out, total, ok, err := r.holdersRollupBoard(t.Context(), "USDC-"+testIssuer, 100)
	if err != nil || !ok {
		t.Fatalf("listed asset, fresh cycle: ok=%v err=%v, want served", ok, err)
	}
	if len(out) != 1 || out[0].Balance != 200 || total != 500 {
		t.Errorf("board=%+v total=%d, want balance=200 total=500", out, total)
	}

	r = &ExplorerReader{conn: holdersRollupLake(t, false, time.Time{}, fresh)}
	out, total, ok, err = r.holdersRollupBoard(t.Context(), "NEW-"+testIssuer, 100)
	if err != nil || !ok {
		t.Fatalf("unlisted asset, fresh cycle: ok=%v err=%v, want served as zero holders", ok, err)
	}
	if len(out) != 0 || total != 0 {
		t.Errorf("board=%+v total=%d, want empty board, total=0", out, total)
	}
}

// TestHoldersRollupBoard_RetriesOnceWhenACycleSwapsMidRead: if a cycle swap lands between the rows read and
// the count read, the board must retry and serve a pair from one cycle, not the torn first read.
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

// shrinkGuardConn is a scripted holdersRollupConn: every Exec succeeds and
// is recorded in issue order, and QueryRow answers a fixed count() per
// exact "SELECT count() FROM <table>" query.
type shrinkGuardConn struct {
	counts    map[string]uint64
	execCalls []string
}

func (c *shrinkGuardConn) Exec(_ context.Context, query string, _ ...any) error {
	c.execCalls = append(c.execCalls, query)
	return nil
}

func (c *shrinkGuardConn) QueryRow(_ context.Context, query string, _ ...any) driver.Row {
	for table, n := range c.counts {
		if query == "SELECT count() FROM "+table {
			return &shrinkGuardRow{n: n}
		}
	}
	return &shrinkGuardRow{err: fmt.Errorf("shrinkGuardConn: no scripted count for %q", query)}
}

type shrinkGuardRow struct {
	driver.Row
	n   uint64
	err error
}

func (r *shrinkGuardRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	p, ok := dest[0].(*uint64)
	if !ok {
		return fmt.Errorf("shrinkGuardRow: unexpected dest %T, want *uint64", dest[0])
	}
	*p = r.n
	return nil
}

func (r *shrinkGuardRow) Err() error { return r.err }

// TestRunHoldersRollupRefusesAShrunkenBoard pins that a staging arm far smaller than the live one is not
// published. The FINAL scan can read far fewer rows without any error, so only a row-count check before the
// EXCHANGE catches it.
func TestRunHoldersRollupRefusesAShrunkenBoard(t *testing.T) {
	conn := &shrinkGuardConn{counts: map[string]uint64{
		// Staging came out of this cycle's fills with almost nothing —
		// the shape a truncated/partial FINAL scan produces.
		"stellar.asset_holders_rollup_staging": 3,
		// What is CURRENTLY live: a healthy prior cycle's board.
		"stellar.asset_holders_rollup":         500_000,
		"stellar.asset_holders_counts_staging": 500,
		"stellar.asset_holders_counts":         500,
	}}

	err := runHoldersRollupSteps(context.Background(), conn, func(string, ...any) {}, nil)
	if err == nil {
		t.Fatal("runHoldersRollupSteps swapped a board that shrank from 500,000 to 3 rows instead of refusing")
	}
	if !strings.Contains(err.Error(), "shrink guard") {
		t.Errorf("error does not name the shrink guard: %v", err)
	}

	for _, q := range conn.execCalls {
		if strings.Contains(q, "EXCHANGE TABLES") {
			t.Errorf("EXCHANGE TABLES was issued despite the shrink — the previous (good) cycle must stay live:\n%s", q)
		}
	}
}

// TestRunHoldersRollupPublishesAHealthyBoard is the guard's negative case:
// a cycle whose staging counts are in line with (or ahead of) live must
// still swap, so the shrink guard cannot itself become a standing outage.
func TestRunHoldersRollupPublishesAHealthyBoard(t *testing.T) {
	conn := &shrinkGuardConn{counts: map[string]uint64{
		"stellar.asset_holders_rollup_staging": 500_100,
		"stellar.asset_holders_rollup":         500_000,
		"stellar.asset_holders_counts_staging": 505,
		"stellar.asset_holders_counts":         500,
		"stellar.asset_stats_daily_staging":    905,
		assetStatsDailyLatestDay:               900,
	}}

	if err := runHoldersRollupSteps(context.Background(), conn, func(string, ...any) {}, nil); err != nil {
		t.Fatalf("runHoldersRollupSteps: %v", err)
	}

	found := false
	for _, q := range conn.execCalls {
		if strings.Contains(q, "EXCHANGE TABLES") {
			found = true
		}
	}
	if !found {
		t.Error("a healthy cycle did not issue the EXCHANGE TABLES swap")
	}
}

var (
	maxExecutionTimeRe = regexp.MustCompile(`max_execution_time\s*=\s*(\d+)`)
	timeoutStartSecRe  = regexp.MustCompile(`(?m)^TimeoutStartSec=(.+)$`)
	systemdSpanTokenRe = regexp.MustCompile(`^(\d+)(h|min|s)$`)
)

// TestHoldersRollupUnitTimeoutCoversStatementCeilings pins that the unit's TimeoutStartSec is at least the
// serial sum of every statement's max_execution_time, so systemd cannot SIGTERM a legal cycle before its
// EXCHANGE.
func TestHoldersRollupUnitTimeoutCoversStatementCeilings(t *testing.T) {
	var ceiling time.Duration
	for i, stmt := range holdersRollupStatements(time.Now()) {
		m := maxExecutionTimeRe.FindAllStringSubmatch(stmt, -1)
		if len(m) == 0 {
			// Metadata-only statements: no scan to bound.
			head := strings.TrimSpace(stmt)
			if strings.HasPrefix(head, "TRUNCATE TABLE ") || strings.HasPrefix(head, "EXCHANGE TABLES ") ||
				(strings.HasPrefix(head, "ALTER TABLE ") && strings.Contains(head, " ADD COLUMN IF NOT EXISTS ")) {
				continue
			}
			t.Fatalf("holders rollup statement %d carries no max_execution_time, so the cycle has no computable ceiling:\n%s", i+1, stmt)
		}
		if len(m) > 1 {
			t.Fatalf("holders rollup statement %d carries %d max_execution_time settings, want one:\n%s", i+1, len(m), stmt)
		}
		secs, err := strconv.Atoi(m[0][1])
		if err != nil {
			t.Fatalf("statement %d: parse max_execution_time %q: %v", i+1, m[0][1], err)
		}
		ceiling += time.Duration(secs) * time.Second
	}
	if ceiling == 0 {
		t.Fatal("no capped statement found — this test would pass vacuously")
	}

	unit := filepath.Join(systemdRepoRoot(t), "configs", "ansible", "roles", "archival-node",
		"templates", "systemd", "holders-rollup.service.j2")
	body, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("read %s: %v", unit, err)
	}
	m := timeoutStartSecRe.FindAllSubmatch(body, -1)
	if len(m) != 1 {
		t.Fatalf("%s: want exactly one TimeoutStartSec= line, found %d", unit, len(m))
	}
	timeout := parseSystemdSpan(t, strings.TrimSpace(string(m[0][1])))

	if timeout < ceiling {
		t.Fatalf("holders-rollup.service TimeoutStartSec=%s is below the cycle's summed max_execution_time %s: systemd kills a cycle ClickHouse would still let finish",
			timeout, ceiling)
	}
}

// parseSystemdSpan parses the subset of systemd.time(7) spans the unit
// templates use: space-separated <n>h / <n>min / <n>s tokens.
func parseSystemdSpan(t *testing.T, span string) time.Duration {
	t.Helper()
	units := map[string]time.Duration{"h": time.Hour, "min": time.Minute, "s": time.Second}
	var d time.Duration
	fields := strings.Fields(span)
	if len(fields) == 0 {
		t.Fatalf("empty systemd time span")
	}
	for _, tok := range fields {
		m := systemdSpanTokenRe.FindStringSubmatch(tok)
		if m == nil {
			t.Fatalf("unsupported systemd time span token %q in %q", tok, span)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("parse %q: %v", tok, err)
		}
		d += time.Duration(n) * units[m[2]]
	}
	return d
}

// TestRunHoldersRollupReportsEachStep pins the progress signal the ops heartbeat publishes: one onStep per completed statement,
// so stellarindex_ops_job_no_progress sees a long cycle advance.
func TestRunHoldersRollupReportsEachStep(t *testing.T) {
	conn := &shrinkGuardConn{counts: map[string]uint64{
		"stellar.asset_holders_rollup_staging": 500_100,
		"stellar.asset_holders_rollup":         500_000,
		"stellar.asset_holders_counts_staging": 505,
		"stellar.asset_holders_counts":         500,
		"stellar.asset_stats_daily_staging":    905,
		assetStatsDailyLatestDay:               900,
	}}

	steps := 0
	if err := runHoldersRollupSteps(context.Background(), conn, func(string, ...any) {}, func() { steps++ }); err != nil {
		t.Fatalf("runHoldersRollupSteps: %v", err)
	}
	cycleAt := time.Now()
	want := len(holdersRollupStatements(cycleAt)) + len(assetStatsSnapshotFills(cycleAt)) + 1
	if steps != want || steps != len(conn.execCalls) {
		t.Errorf("onStep fired %d times, want %d (one per executed statement, %d executed)", steps, want, len(conn.execCalls))
	}
}

// The classic circulating read trusts the rollup only if its trustline count step sums the same balances as the scan.
func TestHoldersRollupCountStepCarriesCirculating(t *testing.T) {
	for _, s := range holdersRollupStatements(time.Now()) {
		if strings.Contains(s, "asset_holders_counts_staging (asset, holders, circulating, computed_at)") {
			if !strings.Contains(s, "sum(toInt128(balance))") || !strings.Contains(s, "WHERE "+classicCirculatingFilter+"\n") {
				t.Fatalf("trustline count step does not sum the scan's population as Int128:\n%s", s)
			}
			return
		}
	}
	t.Fatal("no count step writes circulating")
}

// A binary that lands before the operator ALTER must heal the schema before it writes the new column, on both twins.
func TestHoldersRollupAddsCirculatingBeforeWriting(t *testing.T) {
	stmts := holdersRollupStatements(time.Now())
	altered := map[string]int{}
	for i, s := range stmts {
		for _, tbl := range []string{"stellar.asset_holders_counts ", "stellar.asset_holders_counts_staging "} {
			if strings.HasPrefix(s, "ALTER TABLE "+tbl+"ADD COLUMN IF NOT EXISTS circulating") {
				altered[tbl] = i + 1
			}
		}
		if strings.Contains(s, "circulating, computed_at)") && (altered["stellar.asset_holders_counts "] == 0 || altered["stellar.asset_holders_counts_staging "] == 0) {
			t.Fatalf("statement %d writes circulating before both twins are altered (%v)", i+1, altered)
		}
	}
	if len(altered) != 2 {
		t.Fatalf("want both twins altered, got %v", altered)
	}
}
