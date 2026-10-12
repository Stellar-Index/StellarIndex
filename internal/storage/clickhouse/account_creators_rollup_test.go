// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"strings"
	"testing"
)

// TestCreatorsRollupStatsDeriveFromTheBoard pins that the stats arm aggregates the staging board written this
// cycle and never re-reads stellar.account_movements, so its ledger span cannot diverge from the board.
func TestCreatorsRollupStatsDeriveFromTheBoard(t *testing.T) {
	stats := creatorsRollupStatement(t, "account_creators_stats_staging")

	for _, metric := range []string{"from_ledger", "thru_ledger", "from_time", "thru_time", "creations_total"} {
		if !strings.Contains(stats, metric) {
			t.Errorf("stats statement is missing the %q metric", metric)
		}
	}
	if !strings.Contains(stats, "FROM stellar.account_creators_rollup_staging") {
		t.Error("stats statement must aggregate the staging board written by this same cycle")
	}
	if strings.Contains(stats, "stellar.account_movements") {
		t.Error("stats statement re-reads the movement archive; the span would then " +
			"describe a different row set than the board it qualifies")
	}
	// The span must be min/max over the board's own ledger columns, not a
	// literal.
	if !strings.Contains(stats, "min(first_ledger)") || !strings.Contains(stats, "max(last_ledger)") {
		t.Error("coverage span must be derived (min/max over the board), not asserted")
	}
}

// TestCreatorsRollupSwapIsAtomic: the board and its qualifying span must swap in ONE metadata transaction, or
// AccountCreators serves a new board beside the previous cycle's span as authoritative.
func TestCreatorsRollupSwapIsAtomic(t *testing.T) {
	var exchanges []string
	for _, step := range creatorsRollupStatements(P23BoundaryLedger) {
		if strings.Contains(step.sql, "EXCHANGE TABLES") {
			exchanges = append(exchanges, step.sql)
		}
	}
	if len(exchanges) != 1 {
		t.Fatalf("found %d EXCHANGE statements, want exactly 1 multi-pair swap", len(exchanges))
	}
	for _, pair := range []string{
		"stellar.account_creators_rollup_staging AND stellar.account_creators_rollup",
		"stellar.account_creators_stats_staging AND stellar.account_creators_stats",
	} {
		if !strings.Contains(exchanges[0], pair) {
			t.Errorf("the single EXCHANGE is missing the pair %q", pair)
		}
	}
	if exchanges[0] != creatorsRollupStatements(P23BoundaryLedger)[len(creatorsRollupStatements(P23BoundaryLedger))-1].sql {
		t.Error("the EXCHANGE must be the last statement, after every staging arm is filled")
	}
	// The working table is not served and must never be swapped.
	if strings.Contains(exchanges[0], "account_creators_ops") {
		t.Error("the working table is not a served table and must not be exchanged")
	}
}

// creationArms names the two ledger-axis halves this cycle aggregates, each with the range its source can
// really hold a creation in: classic create_account rows are clamped below P23BoundaryLedger (ADR-0047 D2),
// so past it only the CreateAccount operation marks a CAP-67 transfer as a creation.
var creationArms = []struct {
	name string
	// marker identifies the arm in a statement.
	marker string
	// lo, hi bound where this arm's SOURCE can hold a creation.
	lo, hi uint32
}{
	{
		name:   "classic create_account movements",
		marker: "movement_kind = 'create_account'",
		lo:     1,
		hi:     P23BoundaryLedger - 1,
	},
	{
		name:   "CAP-67 transfers paired with CreateAccount operations",
		marker: "op_type = '" + opCreateAccount + "'",
		lo:     SorobanGenesisLedger,
		hi:     ^uint32(0),
	},
}

// TestCreatorsRollupSpansTheP23Boundary: each probe ledger must land in the working table via exactly one
// creation arm. Zero means the board stops at the boundary; two means creators in the overlap are counted
// twice. With the neighbouring tests this pins the whole served claim.
func TestCreatorsRollupSpansTheP23Boundary(t *testing.T) {
	// r1's lake tip — about a year of ledgers above the
	// boundary, which is the span the board was missing.
	const tip = 64_310_629

	rec := &recordingExecer{}
	if err := runRollupSteps(context.Background(), rec, tip, "creators",
		creatorsRollupStatements(P23BoundaryLedger), func(string, ...any) {}); err != nil {
		t.Fatalf("runRollupSteps: %v", err)
	}

	for _, probe := range []struct {
		name   string
		ledger uint32
	}{
		{"the first ledger of the chain", 1},
		{"a classic-era creation", 3},
		{"a pre-P23 ledger that is also post-Soroban", 55_000_000},
		{"the last pre-P23 ledger", P23BoundaryLedger - 1},
		{"the first P23 ledger", P23BoundaryLedger},
		{"a creation from the last year", 63_000_002},
		{"the lake tip", tip},
	} {
		t.Run(probe.name, func(t *testing.T) {
			var landed []string
			for _, call := range rec.calls {
				if !strings.HasPrefix(call.sql, "INSERT INTO stellar.account_creators_ops") {
					continue
				}
				lo, hi, ok := callWindow(t, call)
				if !ok || probe.ledger < lo || probe.ledger > hi {
					continue
				}
				if !armClampAdmits(t, call.sql, probe.ledger) {
					continue
				}
				for _, arm := range creationArms {
					if strings.Contains(call.sql, arm.marker) &&
						probe.ledger >= arm.lo && probe.ledger <= arm.hi {
						landed = append(landed, arm.name)
					}
				}
			}
			if len(landed) != 1 {
				t.Errorf("ledger %d is landed in the working table by %d arm(s) %v, want exactly 1 — "+
					"0 means the board's population stops there and its coverage span with it; "+
					"2 means every creation there is counted twice",
					probe.ledger, len(landed), landed)
			}
		})
	}
}

// callWindow reads the window an issued walked statement was bound to.
func callWindow(t *testing.T, call recordedCall) (uint32, uint32, bool) {
	t.Helper()
	if len(call.args) < 2 {
		return 0, 0, false
	}
	lo, loOK := call.args[0].(uint32)
	hi, hiOK := call.args[1].(uint32)
	if !loOK || !hiOK {
		t.Fatalf("window bounds are %T/%T, want uint32 ledger sequences", call.args[0], call.args[1])
	}
	return lo, hi, true
}

// armClampAdmits reports whether an arm's own boundary predicates admit a ledger. Every clamp must
// name P23BoundaryLedger: a second literal for the boundary lets the arms gap or overlap.
func armClampAdmits(t *testing.T, sql string, ledger uint32) bool {
	t.Helper()
	admits := true
	for _, c := range ledgerClamps(t, sql) {
		if c.at != P23BoundaryLedger {
			t.Errorf("arm clamps at ledger %d, not P23BoundaryLedger (%d):\n%s",
				c.at, P23BoundaryLedger, sql)
		}
		if c.below && ledger >= c.at {
			admits = false
		}
		if !c.below && ledger < c.at {
			admits = false
		}
	}
	return admits
}

// TestCreatorsRollupDedupesTheArchive: account_movements is a ReplacingMergeTree, so un-merged duplicates
// would inflate accounts_created; every pass landing the working table must dedupe over the full ORDER BY
// key. Grouping per window loses nothing: the partition expression derives from a ledger column in that key.
func TestCreatorsRollupDedupesTheArchive(t *testing.T) {
	arms := creatorsRollupStatementsInto(t, "account_creators_ops", 2)

	for i, arm := range arms {
		if !strings.Contains(arm, "argMax(") {
			t.Errorf("arm %d does not collapse ReplacingMergeTree duplicates (argMax over ingested_at):\n%s", i+1, arm)
		}
		// Only the funder arm of a movement pair is a creation record;
		// counting both directions would double every creator's total.
		if !strings.Contains(arm, "direction = 'sent'") {
			t.Errorf("arm %d does not read only the funder (sent) arm of the movement pair:\n%s", i+1, arm)
		}
	}

	classic := creatorsArm(t, "movement_kind = 'create_account'")
	if !strings.Contains(classic, "GROUP BY address, ledger, tx_hash, op_index, leg_index, direction") {
		t.Error("the classic arm's dedupe must group by account_movements' full ORDER BY key; " +
			"a narrower key would drop real creations, a wider one would keep duplicates")
	}

	p23 := creatorsArm(t, "op_type = '"+opCreateAccount+"'")
	if !strings.Contains(p23, "GROUP BY m.address, m.ledger, m.tx_hash, m.op_index, m.leg_index, m.direction") {
		t.Error("the post-P23 arm's dedupe must group by account_movements' full ORDER BY key")
	}
	if !strings.Contains(p23, "GROUP BY ledger_seq, tx_hash, op_index") {
		t.Error("the post-P23 arm must also collapse stellar.operations' duplicates over ITS " +
			"full ORDER BY key, or one re-ingested operation multiplies a creation")
	}
	if !strings.Contains(p23, "movement_kind = 'transfer'") {
		t.Error("the post-P23 arm must read the CAP-67 transfer that carries the funding leg")
	}
}

// TestCreatorsRollupPostP23FundingComesFromTheMovement pins that the creator comes from the movement, not
// operations.source_account. Operations keep failed transactions, and only a real transfer proves a creation.
func TestCreatorsRollupPostP23FundingComesFromTheMovement(t *testing.T) {
	p23 := creatorsArm(t, "op_type = '"+opCreateAccount+"'")

	opsSide := p23[strings.Index(p23, "INNER JOIN"):strings.Index(p23, ") AS o ON")]
	for _, col := range []string{"source_account", "close_time", "body_xdr"} {
		if strings.Contains(opsSide, col) {
			t.Errorf("the operations side selects %q; it may contribute the join key only, "+
				"because it holds failed transactions' operations too:\n%s", col, opsSide)
		}
	}
	for _, projected := range []string{
		"m.address AS creator",
		"argMax(m.counterparty, m.ingested_at) AS created",
		"argMax(m.amount, m.ingested_at) AS amount",
		"argMax(m.ledger_close_time, m.ingested_at)",
	} {
		if !strings.Contains(p23, projected) {
			t.Errorf("post-P23 arm does not take %q from the movement", projected)
		}
	}
	// The pairing is per operation, so the join must carry the whole
	// operation identity. Any looser key would attach one creation's
	// funding to another operation in the same transaction.
	if !strings.Contains(p23, "ON m.ledger = o.ledger_seq AND m.tx_hash = o.tx_hash AND m.op_index = o.op_index") {
		t.Error("the post-P23 join key must be the full (ledger, tx_hash, op_index) operation identity")
	}
}

// TestCreatorsRollupScansMovementsOnce: account_movements is read once per window, by the arm owning that
// side of the P23 boundary, and only by a walked step that fills the working table. Everything else derives
// from those rows, so the board and its coverage span describe the same data.
func TestCreatorsRollupScansMovementsOnce(t *testing.T) {
	var touching []int
	for i, step := range creatorsRollupStatements(P23BoundaryLedger) {
		if !strings.Contains(step.sql, "stellar.account_movements") {
			continue
		}
		touching = append(touching, i+1)
		if !step.walk {
			t.Errorf("step %d reads stellar.account_movements unwalked", i+1)
			continue
		}
		if !strings.Contains(step.sql, "ledger BETWEEN ? AND ?") {
			t.Errorf("step %d is walked but carries no ledger-window predicate on the movement "+
				"archive, which is what prunes it to one partition", i+1)
		}
		if !strings.HasPrefix(step.sql, "INSERT INTO stellar.account_creators_ops") {
			t.Errorf("step %d reads the archive but does not fill the working table", i+1)
		}
		clamps := ledgerClamps(t, step.sql)
		if len(clamps) == 0 {
			t.Errorf("step %d reads the archive with no P23 clamp, so it overlaps the other arm", i+1)
		}
	}
	if len(touching) != 2 {
		t.Fatalf("statements touching stellar.account_movements: %v, want exactly 2 "+
			"(one arm each side of the P23 boundary)", touching)
	}
}

// TestCreatorsRollupJoinsOutsideTheWalk pins that the LEFT JOIN over the account-entry table runs in the
// once-per-cycle step, not the walked step, which would rebuild that hash table on every window. A walked
// step may join only window-bounded sides, and must pin the build side so the peak does not drift.
func TestCreatorsRollupJoinsOutsideTheWalk(t *testing.T) {
	for i, step := range creatorsRollupStatements(P23BoundaryLedger) {
		if !step.walk {
			continue
		}
		if strings.Contains(step.sql, "stellar.ledger_entries_current") {
			t.Errorf("walked step %d reads the live account entries; that build side is the "+
				"account population and would be paid once per window:\n%s", i+1, step.sql)
		}
		if strings.Contains(step.sql, "JOIN") && !strings.Contains(step.sql, "query_plan_join_swap_table = 0") {
			t.Errorf("walked step %d joins without pinning its build side; which population "+
				"sets the step's peak would then be a planner estimate:\n%s", i+1, step.sql)
		}
	}
	board := creatorsRollupStatement(t, "account_creators_rollup_staging")
	if !strings.Contains(board, "LEFT JOIN") {
		t.Fatal("the board no longer joins the live account entries; live_accounts " +
			"and live_stroops would stop describing the created set")
	}
	if !strings.Contains(board, "FROM stellar.account_creators_ops\n") ||
		strings.Contains(board, "stellar.account_movements") || strings.Contains(board, "stellar.operations") {
		t.Error("the board must join the working table the walk wrote, not re-scan the archive")
	}
	// The build side is pinned to the account-entry population so the
	// cycle's peak cannot silently switch to the creation population,
	// which grows with chain history instead.
	if !strings.Contains(board, "query_plan_join_swap_table = 0") {
		t.Error("the board's join build side is unpinned; which population sets the " +
			"cycle's peak would then be a planner estimate")
	}
}

// TestCreatorsRollupLiveAccountsDedupeRecycledAddresses: a creator recycling one address yields several rows
// with the same `created`; live_accounts/live_stroops must count one row per distinct (creator, created)
// pair.
func TestCreatorsRollupLiveAccountsDedupeRecycledAddresses(t *testing.T) {
	board := creatorsRollupStatement(t, "account_creators_rollup_staging")

	if !strings.Contains(board, "GROUP BY c.creator, c.created") {
		t.Error("board does not collapse the live join to one row per (creator, created) pair " +
			"before aggregating live_accounts/live_stroops; a recycled address is counted once per creation event")
	}
	if !strings.Contains(board, "uniqExactIf(created, is_live)") {
		t.Error("live_accounts is not counted as a distinct-created-address aggregate " +
			"(uniqExactIf over the deduped pairs); a recycled address inflates the count")
	}
	// The dedupe must be nested inside the live-join subquery, below the outer per-creator aggregate: the join
	// fans out one row per creation event, and only a GROUP BY on the pair before that aggregate collapses it
	// before sum(live_stroops).
	liveJoinAt := strings.Index(board, "uniqExactIf(created, is_live)")
	dedupeAt := strings.Index(board, "GROUP BY c.creator, c.created")
	outerGroupAt := strings.LastIndex(board, "GROUP BY creator")
	if liveJoinAt == -1 || dedupeAt == -1 || outerGroupAt == -1 ||
		!(liveJoinAt < dedupeAt && dedupeAt < outerGroupAt) {
		t.Error("the (creator, created) dedupe must sit inside the live-join subquery, " +
			"nested below its own outer per-creator GROUP BY, or the per-event fan-out " +
			"survives into sum(live_stroops)")
	}
}

// TestCreatorsRollupLiveCreditsLatestCreatorOnly guards the cross-creator recycle (A creates X, X merges, B
// re-creates X): the live join must credit X's current incarnation to its latest creator only, or
// live_accounts_total counts it per creator. Executing proof: test/integration/clickhouse_rollups_test.go.
func TestCreatorsRollupLiveCreditsLatestCreatorOnly(t *testing.T) {
	board := creatorsRollupStatement(t, "account_creators_rollup_staging")

	latest := strings.Index(board, "argMax(creator, (ledger, creator)) AS creator")
	collapse := strings.Index(board, "GROUP BY created\n")
	join := strings.Index(board, "LEFT JOIN (\n\t                 SELECT account_id, balance")
	if latest == -1 || collapse == -1 || join == -1 || !(latest < collapse && collapse < join) {
		t.Error("the live join's left side is not one row per created address attributed to its " +
			"latest creator; an address recycled by different creators is live under each of them")
	}
	if strings.Contains(board, "FROM stellar.account_creators_ops AS c") {
		t.Error("the live join reads per-creation rows directly; every creator that ever " +
			"created a recycled address is credited with its current balance")
	}
}

// clampLedger guards the stats column's Int64 against values a ledger
// sequence can never hold. Returning 0 routes them into the "warming"
// branch instead of onto the wire as a coverage claim.
func TestClampLedger(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int64
		want uint32
	}{
		{"a real ledger", 64184370, 64184370},
		{"the max uint32", 4294967295, 4294967295},
		{"empty rollup reads as no span", 0, 0},
		{"negative is not a ledger", -1, 0},
		{"beyond uint32 is not a ledger", 4294967296, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampLedger(tc.in); got != tc.want {
				t.Errorf("clampLedger(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestCreatorsRollupArmsSplitAtTheGivenBoundary pins that both creation arms clamp at the boundary the
// CALLER passes (P23BoundaryLedger on pubnet, chain start on a reset test net), and nowhere else.
// Red if the pubnet constant were baked into the SQL: the classic arm would own every test-net ledger.
func TestCreatorsRollupArmsSplitAtTheGivenBoundary(t *testing.T) {
	for _, boundary := range []uint32{1, 2, P23BoundaryLedger} {
		classic, p23 := creatorsArmsAt(t, boundary)

		classicClamps := ledgerClamps(t, classic)
		if len(classicClamps) != 1 || !classicClamps[0].below || classicClamps[0].at != boundary {
			t.Errorf("boundary %d: classic arm clamps %+v, want exactly one `< %d`", boundary, classicClamps, boundary)
		}
		p23Clamps := ledgerClamps(t, p23)
		if len(p23Clamps) != 2 {
			t.Errorf("boundary %d: post-P23 arm carries %d clamps, want 2 (one per source table)", boundary, len(p23Clamps))
		}
		for _, c := range p23Clamps {
			if c.below || c.at != boundary {
				t.Errorf("boundary %d: post-P23 arm clamp %+v, want `>= %d`", boundary, c, boundary)
			}
		}
	}

	// A test-net ledger (every one sits below the pubnet boundary): owned
	// by the post-P23 arm at the chain-start boundary, by the classic arm
	// at pubnet's — the latter is the empty-cohort defect.
	const testNetLedger = 4_728_426
	classic, p23 := creatorsArmsAt(t, 1)
	if clampsAdmit(t, classic, testNetLedger) {
		t.Errorf("boundary 1: the classic arm admits ledger %d; a post-P23-only chain has no create_account movements to find", testNetLedger)
	}
	if !clampsAdmit(t, p23, testNetLedger) {
		t.Errorf("boundary 1: the post-P23 arm does not admit ledger %d; the creation there would be counted by no arm", testNetLedger)
	}
	classic, p23 = creatorsArmsAt(t, P23BoundaryLedger)
	if !clampsAdmit(t, classic, testNetLedger) || clampsAdmit(t, p23, testNetLedger) {
		t.Errorf("pubnet boundary: ledger %d must be owned by the classic arm alone", testNetLedger)
	}
}

// creatorsArmsAt returns the classic and post-P23 creation arms of the
// cycle built for the given boundary.
func creatorsArmsAt(t *testing.T, boundary uint32) (classic, p23 string) {
	t.Helper()
	for _, step := range creatorsRollupStatements(boundary) {
		switch {
		case strings.Contains(step.sql, "movement_kind = 'create_account'"):
			classic = step.sql
		case strings.Contains(step.sql, "op_type = '"+opCreateAccount+"'"):
			p23 = step.sql
		}
	}
	if classic == "" || p23 == "" {
		t.Fatalf("boundary %d: cycle is missing a creation arm (classic=%t, p23=%t)", boundary, classic != "", p23 != "")
	}
	return classic, p23
}

// clampsAdmit reports whether every boundary predicate in a statement
// admits the ledger, whatever value the clamps sit at.
func clampsAdmit(t *testing.T, sql string, ledger uint32) bool {
	t.Helper()
	for _, c := range ledgerClamps(t, sql) {
		if c.below && ledger >= c.at {
			return false
		}
		if !c.below && ledger < c.at {
			return false
		}
	}
	return true
}

// creatorsRollupStatementsInto returns the INSERTs that fill the named
// table, failing unless there are exactly want of them.
func creatorsRollupStatementsInto(t *testing.T, table string, want int) []string {
	t.Helper()
	var found []string
	for _, step := range creatorsRollupStatements(P23BoundaryLedger) {
		if strings.HasPrefix(step.sql, "INSERT INTO stellar."+table) {
			found = append(found, step.sql)
		}
	}
	if len(found) != want {
		t.Fatalf("found %d INSERTs into %s, want exactly %d", len(found), table, want)
	}
	return found
}

// creatorsRollupStatement returns the single INSERT that fills the named
// staging table, failing if the cycle does not fill it exactly once.
func creatorsRollupStatement(t *testing.T, table string) string {
	t.Helper()
	return creatorsRollupStatementsInto(t, table, 1)[0]
}

// creatorsArm returns the one creation arm carrying the given source
// marker. Two arms landing the same marker would mean the cycle reads
// one side of the boundary twice.
func creatorsArm(t *testing.T, marker string) string {
	t.Helper()
	var found []string
	for _, step := range creatorsRollupStatements(P23BoundaryLedger) {
		if strings.Contains(step.sql, marker) {
			found = append(found, step.sql)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d statements carrying %q, want exactly 1", len(found), marker)
	}
	return found[0]
}
