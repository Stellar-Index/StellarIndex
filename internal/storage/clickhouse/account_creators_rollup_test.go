// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"strings"
	"testing"
)

// TestCreatorsRollupStatsDeriveFromTheBoard pins the property that makes
// the served coverage span honest by construction: the stats arm — which
// carries from_ledger/thru_ledger — must aggregate the STAGING BOARD the
// same cycle just wrote, never re-read stellar.account_movements.
//
// If the span came from a second scan of the archive it could describe a
// different set of rows than the board it qualifies, and the endpoint
// would report a coverage span its own numbers do not back. Deriving it
// from the board makes that divergence unrepresentable.
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

// TestCreatorsRollupSwapIsAtomic: the board and the span that qualifies
// it must swap in ONE metadata transaction. A board swapped new beside
// the previous cycle's span is exactly the overstatement this surface
// exists to avoid, and AccountCreators would serve it as authoritative.
func TestCreatorsRollupSwapIsAtomic(t *testing.T) {
	var exchanges []string
	for _, step := range creatorsRollupStatements {
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
	if exchanges[0] != creatorsRollupStatements[len(creatorsRollupStatements)-1].sql {
		t.Error("the EXCHANGE must be the last statement, after every staging arm is filled")
	}
	// The working table is not served and must never be swapped.
	if strings.Contains(exchanges[0], "account_creators_ops") {
		t.Error("the working table is not a served table and must not be exchanged")
	}
}

// creationArms names the two halves of the ledger axis this cycle
// aggregates, each by the source predicate that defines it, together
// with the ledger range that source can ACTUALLY hold a creation in.
//
// That last column is the fact the board originally got wrong (#493) and
// is not a restatement of the SQL. stellar.account_movements' create_account
// rows have exactly one writer, `stellarindex-ops classic-movements-backfill`,
// whose -to is hard-clamped below P23BoundaryLedger because ADR-0047 D2
// makes that projection historical-only — so the classic arm cannot see
// a creation at or above the boundary however wide its window is.
// Measured on r1 2026-09-07: max(ledger) = 58,762,516 for
// movement_kind='create_account' and 0 rows at or above the boundary,
// against a lake tip of 64,310,629. Post-P23 the same creation is a
// CAP-67 `transfer` movement, and only the CreateAccount operation in
// stellar.operations says the transfer was a creation.
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

// TestCreatorsRollupSpansTheP23Boundary is the guard for the defect this
// board shipped with: a league table whose population silently stopped
// at a protocol boundary while its ranking went on being served as
// current. Protocol 23 changed how a CreateAccount is RECORDED, not
// whether it happens, so a cycle that reads only the classic
// representation ranks creators over a set that ends at ledger
// 58,762,516 — 4,715,612 creations short of the tip on r1 2026-09-07.
//
// It drives the real walk over a real post-P23 tip and asks, of each
// probe ledger, how many creation arms actually land it in the working
// table. An arm lands a ledger only if all three hold: the walk gave it
// a window containing that ledger, the arm's own boundary clamp admits
// it, and the arm's SOURCE can hold a creation there (creationArms).
//
// Exactly one, everywhere, is the property. Zero is the defect — the
// board's population stops and its coverage span stops with it. Two is
// the opposite defect — every creator in the overlap counted twice, and
// a board wrong in the direction that looks like success.
//
// This is the whole served claim, because the rest of the chain is
// pinned either side of it: the board aggregates the working table
// (TestCreatorsRollupJoinsOutsideTheWalk) and the coverage span is
// min/max over that board (TestCreatorsRollupStatsDeriveFromTheBoard),
// so a working table whose creations reach the tip is a thru_ledger that
// reaches the tip.
func TestCreatorsRollupSpansTheP23Boundary(t *testing.T) {
	// r1's lake tip on 2026-09-07 — about a year of ledgers above the
	// boundary, which is the span the board was missing.
	const tip = 64_310_629

	rec := &recordingExecer{}
	if err := runRollupSteps(context.Background(), rec, tip, "creators",
		creatorsRollupStatements, func(string, ...any) {}); err != nil {
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

// armClampAdmits reports whether an arm's own boundary predicates admit
// a ledger. Every clamp must name P23BoundaryLedger: a second literal
// for the same boundary is how the two arms come to disagree about where
// it is, which is a gap or an overlap depending on which way it drifts.
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

// TestCreatorsRollupDedupesTheArchive: stellar.account_movements is a
// ReplacingMergeTree, so un-merged duplicate parts are normal. Counting
// rows straight out of it would inflate accounts_created — a silently
// wrong league table. EVERY walked pass that lands the working table
// must collapse duplicates over the source table's full ORDER BY key.
//
// Grouping per window loses nothing: each source's partition expression
// is a function of its ledger column, which is itself part of that ORDER
// BY key, so every row of a duplicate group lands in the same window.
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

// TestCreatorsRollupPostP23FundingComesFromTheMovement pins the reason
// the post-P23 arm joins through a movement instead of reading
// stellar.operations directly.
//
// stellar.operations retains the operations of FAILED transactions by
// design (extract.go's Ops arm has no success gate — the lake keeps what
// the ledger contained). A creation that never happened has no CAP-67
// transfer, so pairing the operation with its movement is what gates the
// board on transaction success. Measured on r1 2026-09-07 over ledgers
// 63,000,000-63,010,000: of 6,266 distinct CreateAccount operations the
// 5,890 in successful transactions match exactly one transfer leg each,
// and the 376 in failed transactions match none.
//
// So the operations side may contribute the join KEY and nothing else.
// Taking the creator from operations.source_account — the obvious
// simplification — would put 8% more creators on the board than ever
// created an account, silently.
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

// TestCreatorsRollupScansMovementsOnce: stellar.account_movements is the
// expensive table — 10,309,271,697 rows / 583.54 GiB on r1 2026-09-06.
// It is read once PER WINDOW, by whichever arm owns that side of the
// boundary, and only ever by a walked step that fills the working table;
// every other figure derives from those rows, which is what makes the
// board and its coverage span describe the same data.
//
// "Once" is per window rather than per cycle because the archive changes
// representation at P23 and the cycle reads both. The arms clamp on
// opposite sides of the boundary, so a window wholly on the far side of
// a clamp prunes to no parts at all — measured at 0 rows read and 2-4 ms
// on r1 2026-09-07, against 2.4-107 s for the arm that owns it.
func TestCreatorsRollupScansMovementsOnce(t *testing.T) {
	var touching []int
	for i, step := range creatorsRollupStatements {
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

// TestCreatorsRollupJoinsOutsideTheWalk pins the placement that the
// walk alone would not have fixed.
//
// The board's LEFT JOIN builds one row per account that currently exists
// — 10,928,611 rows at 3.18 GiB measured on r1 2026-09-06 — and that
// build side does not shrink when the movement scan is partitioned. A
// join over THAT population left inside the walked step would rebuild
// the same hash table on every one of the 65 windows and re-read the
// whole account entry range each time, while leaving the cycle's largest
// single memory consumer un-walked. It belongs in the once-per-cycle
// step that reads the working table.
//
// The hazard is the POPULATION, not the keyword: a walked step may join
// where both sides are bounded by its own window (the post-P23 arm pairs
// a window's transfers with that window's CreateAccount operations, and
// measures 1.43 GiB at its widest on r1 2026-09-07). What it may not do
// is touch the account-entry table, or leave the planner to choose which
// side of a join becomes the hash table — the two sides grow with
// different populations, so an unpinned estimate silently moves the
// cycle's peak from one to the other as the chain grows.
func TestCreatorsRollupJoinsOutsideTheWalk(t *testing.T) {
	for i, step := range creatorsRollupStatements {
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
	if !strings.Contains(board, "FROM stellar.account_creators_ops AS c") {
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

// creatorsRollupStatementsInto returns the INSERTs that fill the named
// table, failing unless there are exactly want of them.
func creatorsRollupStatementsInto(t *testing.T, table string, want int) []string {
	t.Helper()
	var found []string
	for _, step := range creatorsRollupStatements {
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
	for _, step := range creatorsRollupStatements {
		if strings.Contains(step.sql, marker) {
			found = append(found, step.sql)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d statements carrying %q, want exactly 1", len(found), marker)
	}
	return found[0]
}
