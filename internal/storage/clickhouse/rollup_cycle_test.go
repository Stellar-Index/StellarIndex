// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// lakeArchiveTables are the append-only, ledger-partitioned archives a
// rollup cycle reads. Their row counts are functions of chain history —
// 10,309,271,697 rows / 583.54 GiB and 24,736,312,156 rows / 2.18 TiB on
// r1 2026-09-06, and stellar.transactions carried 519,663,457 rows in
// partition 63 alone on 2026-09-07 — so a statement that reads one of
// them without a ledger window has a cost nothing bounds.
var lakeArchiveTables = []string{
	"stellar.account_movements",
	"stellar.operations",
	"stellar.transactions",
}

// appliedEffectSources are the lake tables that hold a row only where an
// effect was actually APPLIED to the ledger: a CAP-67 movement is written
// from the transaction meta, so a failed transaction produces none. A
// board that pairs an operation with one of these has gated itself on
// transaction success through the pairing.
var appliedEffectSources = []string{"stellar.account_movements"}

// rollupCycles is every recompute cycle this package ships, so a cycle
// added later inherits the walk guards below rather than having to
// remember them.
func rollupCycles() map[string][]rollupStep {
	return map[string][]rollupStep{
		"creators": creatorsRollupStatements,
		"sponsors": sponsorsRollupStatements,
	}
}

// archivesRead returns the lake archives a statement reads, in
// lakeArchiveTables order. A statement may legitimately read two (the
// creators cycle's post-P23 arm pairs a window's movements with that
// window's operations), which is exactly why the guards below count
// window predicates per ARCHIVE rather than per statement.
func archivesRead(sql string) []string {
	var reads []string
	for _, table := range lakeArchiveTables {
		if strings.Contains(sql, table) {
			reads = append(reads, table)
		}
	}
	return reads
}

// ledgerClampRe matches an explicit ledger-boundary predicate —
// `ledger < N`, `m.ledger >= N`, `ledger_seq >= N` — the form an arm
// uses to claim one side of a protocol boundary. The window predicate
// (`BETWEEN ? AND ?`) and the partition expression deliberately do not
// match: this is about which HALF of the ledger axis a statement owns,
// not which window it is running.
var ledgerClampRe = regexp.MustCompile(`(?:\w+\.)?ledger(?:_seq)?\s*(<|>=)\s*(\d+)`)

// ledgerClamp is one parsed boundary predicate.
type ledgerClamp struct {
	below bool // "<" rather than ">="
	at    uint32
}

// ledgerClamps parses every boundary predicate in a statement.
func ledgerClamps(t *testing.T, sql string) []ledgerClamp {
	t.Helper()
	var out []ledgerClamp
	for _, m := range ledgerClampRe.FindAllStringSubmatch(sql, -1) {
		n, err := strconv.ParseUint(m[2], 10, 32)
		if err != nil {
			t.Fatalf("ledger clamp %q: %v", m[0], err)
		}
		out = append(out, ledgerClamp{below: m[1] == "<", at: uint32(n)})
	}
	return out
}

// TestRollupCyclesWalkEveryArchiveScan is the guard for the failure that
// took both boards off the air: a statement whose cost is a function of
// the whole archive rather than of one partition. The creator board's
// unwalked statement read all 10,309,146,441 movement rows and died at
// 8.12 GiB against an 8 GiB budget, so its endpoint served 503.
//
// Any statement touching a lake archive must therefore be a WALKED step,
// and it must carry a window predicate FOR EACH archive it reads. The
// per-archive count is the part a per-statement count would miss: a join
// condition prunes neither side's partitions, so a two-source arm that
// bounds only one of them has re-introduced the unbounded cost on the
// other, in a statement that still looks walked.
func TestRollupCyclesWalkEveryArchiveScan(t *testing.T) {
	for name, steps := range rollupCycles() {
		t.Run(name, func(t *testing.T) {
			walkedArchiveSteps := 0
			for i, step := range steps {
				reads := archivesRead(step.sql)
				if len(reads) == 0 {
					if step.walk {
						t.Errorf("step %d is walked but reads no lake archive; "+
							"a walked step must carry the window predicate its bounds bind to", i+1)
					}
					continue
				}
				if !step.walk {
					t.Errorf("step %d reads %v unwalked — its cost is the whole archive, "+
						"not one partition:\n%s", i+1, reads, step.sql)
					continue
				}
				walkedArchiveSteps++
				if got := strings.Count(step.sql, "BETWEEN ? AND ?"); got != len(reads) {
					t.Errorf("step %d reads %d archive(s) %v but carries %d window predicate(s); "+
						"an unbounded source rescans its whole archive every window:\n%s",
						i+1, len(reads), reads, got, step.sql)
				}
			}
			if walkedArchiveSteps == 0 {
				t.Error("no step reads a lake archive; the cycle builds its board from nothing")
			}
		})
	}
}

// TestRollupCyclesReadEachArchiveOncePerWindow is what "scans the archive
// once" has to mean once a cycle legitimately has more than one source
// arm: for any given window, one arm reads the archive, not two.
//
// A second reader of the same archive is admitted only when it cannot
// overlap the first — each reader carries an explicit ledger clamp, all
// the clamps name the SAME boundary, and exactly one of them takes the
// side below it. That makes the arms a partition of the ledger axis:
// their union is every ledger (nothing silently stops at the boundary,
// which was #493) and their intersection is empty (no creation is
// counted twice, which would inflate every row of the board). It also
// keeps the cost honest, because a window wholly on the far side of the
// clamp prunes to no parts at all.
func TestRollupCyclesReadEachArchiveOncePerWindow(t *testing.T) {
	for name, steps := range rollupCycles() {
		t.Run(name, func(t *testing.T) {
			byTable := map[string][]int{}
			for i, step := range steps {
				for _, table := range archivesRead(step.sql) {
					byTable[table] = append(byTable[table], i)
				}
			}
			for table, idxs := range byTable {
				if len(idxs) < 2 {
					continue
				}
				boundary, below := uint32(0), 0
				for _, i := range idxs {
					clamps := ledgerClamps(t, steps[i].sql)
					if len(clamps) == 0 {
						t.Errorf("%s is read by steps %v but step %d carries no ledger clamp, "+
							"so the arms overlap and every creation in the overlap is counted twice",
							table, idxs, i+1)
						continue
					}
					armBelow := false
					for _, c := range clamps {
						if boundary == 0 {
							boundary = c.at
						}
						if c.at != boundary {
							t.Errorf("step %d clamps at ledger %d, but another arm of %s clamps at %d; "+
								"two boundaries leave a gap or an overlap between them",
								i+1, c.at, table, boundary)
						}
						if c.below {
							armBelow = true
						}
					}
					if armBelow {
						below++
					}
				}
				if below != 1 {
					t.Errorf("%s is read by %d steps %v of which %d take the side BELOW the boundary, "+
						"want exactly 1 — any other split leaves a gap or an overlap",
						table, len(idxs), idxs, below)
				}
			}
		})
	}
}

// TestRollupCyclesDeclareTheirWindowBinds pins rollupStep.windowBinds
// against the placeholders actually in the template. The walk binds one
// (lo, hi) pair per declared bind, so a template that grows a second
// source and forgets to declare it fails its window at run time with a
// bind-count error — and one that declares more pairs than it carries
// would bind a window into somebody else's placeholder. Neither is
// something to discover on r1 at 03:00.
func TestRollupCyclesDeclareTheirWindowBinds(t *testing.T) {
	for name, steps := range rollupCycles() {
		t.Run(name, func(t *testing.T) {
			for i, step := range steps {
				pairs := strings.Count(step.sql, "BETWEEN ? AND ?")
				if !step.walk {
					if pairs != 0 || step.windowBinds != 0 {
						t.Errorf("step %d is not walked but carries %d window predicate(s) "+
							"and declares %d bind(s); nothing would ever bind them",
							i+1, pairs, step.windowBinds)
					}
					continue
				}
				declared := step.windowBinds
				if declared == 0 {
					declared = 1
				}
				if declared != pairs {
					t.Errorf("walked step %d declares %d window bind pair(s) but carries %d:\n%s",
						i+1, declared, pairs, step.sql)
				}
				// No placeholder may belong to anything other than a
				// window bound, or the positional args land in the wrong
				// predicate.
				if got := strings.Count(step.sql, "?"); got != 2*pairs {
					t.Errorf("walked step %d carries %d placeholder(s) for %d window predicate(s); "+
						"a stray ? shifts every bound after it:\n%s", i+1, got, pairs, step.sql)
				}
			}
		})
	}
}

// TestRollupCyclesGateOperationsOnTransactionSuccess is the guard for the
// bug class both boards shipped with: counting operations that never took
// effect.
//
// stellar.operations has NO success gate. The lake stores what the ledger
// contained rather than only what succeeded, so extractOps retains the
// operations of failed transactions by design (extract.go's Ops arm). A
// league table built straight off that table therefore credits accounts
// for work that was rolled back — measured on r1 2026-09-07, 10.8% of the
// archive's sponsorship operations and about 8% of its CreateAccount
// operations sit in failed transactions, and the served revocation count
// was better than twice its true value (#493, #494).
//
// So any statement reading stellar.operations must establish application
// one of the two ways this repo has evidence for: pair the operation with
// an applied effect that a failed transaction cannot produce, or join its
// transaction and read the success flag. A third statement that reads the
// table bare fails here rather than on a served board.
//
// WHAT THIS GUARD DOES NOT REACH, stated so the next reader does not
// mistake its silence for a verdict: it walks rollupCycles(), so it sees
// the []rollupStep cycles and nothing else. Five other readers of
// stellar.operations are outside it and are ungated today —
// explorer_reader.go, sdex_op_reader.go (which gates itself, via a
// successful-tx IN-set), contract_call_op_reader.go,
// classic_movement_reader.go and participant_backfill.go. Those serve
// per-entity detail or feed a decoder rather than ranking a board, and an
// unfiltered read is plausibly correct for them — a transaction's failed
// operations are part of that transaction's honest history, and
// /v1/accounts/{g}/operations stamps transaction_successful per row. The
// point is that nobody has ASKED the question of them, not that the
// answer is known to be fine.
func TestRollupCyclesGateOperationsOnTransactionSuccess(t *testing.T) {
	for name, steps := range rollupCycles() {
		t.Run(name, func(t *testing.T) {
			reading := 0
			for i, step := range steps {
				if !strings.Contains(step.sql, "stellar.operations") {
					continue
				}
				reading++
				if strings.Contains(step.sql, "stellar.transactions") &&
					strings.Contains(step.sql, "successful") {
					continue
				}
				gated := false
				for _, src := range appliedEffectSources {
					if strings.Contains(step.sql, src) {
						gated = true
					}
				}
				if !gated {
					t.Errorf("step %d reads stellar.operations without gating on transaction "+
						"success — it counts operations from FAILED transactions, which that "+
						"table retains by design. Pair the operation with an applied effect "+
						"(%v) or join stellar.transactions and read `successful`:\n%s",
						i+1, appliedEffectSources, step.sql)
				}
			}
			if reading == 0 {
				t.Error("no step reads stellar.operations; this guard is asserting nothing")
			}
		})
	}
}

// TestRollupCyclesStageBeforeTheyExchange: the walk must be finished
// before anything is written to a staging arm, and the EXCHANGE must be
// last. That ordering is the whole atomic-swap guarantee — a cycle
// interrupted mid-walk has to leave the previous cycle's board live,
// because the endpoint refuses a board without its span and a
// half-written one would be served as authoritative.
func TestRollupCyclesStageBeforeTheyExchange(t *testing.T) {
	for name, steps := range rollupCycles() {
		t.Run(name, func(t *testing.T) {
			lastWalk, firstStagingWrite, exchanges := -1, -1, 0
			for i, step := range steps {
				if step.walk {
					lastWalk = i
				}
				if strings.HasPrefix(step.sql, "INSERT INTO stellar.") &&
					strings.Contains(strings.SplitN(step.sql, "\n", 2)[0], "_staging") &&
					firstStagingWrite < 0 {
					firstStagingWrite = i
				}
				if strings.Contains(step.sql, "EXCHANGE TABLES") {
					exchanges++
					if i != len(steps)-1 {
						t.Errorf("EXCHANGE at step %d is not the last step", i+1)
					}
				}
			}
			if lastWalk < 0 {
				t.Fatal("cycle has no walked step")
			}
			if firstStagingWrite < 0 {
				t.Fatal("cycle never writes a staging arm")
			}
			// <= not <: a walked step that is ITSELF the first staging
			// write leaves a partial board behind on interruption just
			// as surely as an earlier one does.
			if firstStagingWrite <= lastWalk {
				t.Errorf("staging is written at step %d, not after the walk finishes at step %d — "+
					"an interrupted cycle would leave a partial board to be swapped live",
					firstStagingWrite+1, lastWalk+1)
			}
			if exchanges != 1 {
				t.Errorf("found %d EXCHANGE steps, want exactly 1", exchanges)
			}
		})
	}
}

// TestRollupCyclesBudgetIsNotRaised: the fix for a step that outgrew its
// memory budget is to make the step smaller, never to make the budget
// bigger. A ceiling above the ops-batch class buys one more cycle and
// then fails on a larger archive, so a raise fails here instead.
func TestRollupCyclesBudgetIsNotRaised(t *testing.T) {
	for name, steps := range rollupCycles() {
		t.Run(name, func(t *testing.T) {
			for i, step := range steps {
				if !strings.Contains(step.sql, "max_memory_usage") {
					continue
				}
				if !strings.Contains(step.sql, "max_memory_usage = 8589934592") {
					t.Errorf("step %d sets a max_memory_usage other than the 8 GiB "+
						"full-history scan budget:\n%s", i+1, step.sql)
				}
				if !strings.Contains(step.sql, "max_bytes_before_external_group_by") {
					t.Errorf("step %d caps memory without a spill threshold, so growth "+
						"costs failures instead of time:\n%s", i+1, step.sql)
				}
			}
		})
	}
}

// recordingExecer captures what a cycle actually issues.
type recordingExecer struct {
	calls []recordedCall
}

type recordedCall struct {
	sql  string
	args []any
}

func (r *recordingExecer) Exec(_ context.Context, query string, args ...any) error {
	r.calls = append(r.calls, recordedCall{sql: query, args: args})
	return nil
}

// TestRunRollupStepsWalksEveryWindowOnce drives the cycle runner over
// r1's real tip and pins what it issues: EVERY walked step once per
// 1M-ledger window, over contiguous windows that cover ledger 1 through
// the tip with no gap and no overlap, and every other step exactly once
// with no bounds.
//
// The window bounds are the corrected value here. An unwalked cycle
// issues the archive statement ONCE with no bounds, which is the shape
// that read 10,309,146,441 rows and exhausted the memory budget.
//
// The per-step tiling matters now that a cycle may have more than one
// walked arm: a second arm that walked a shorter range — or that bound
// only one of its two sources to the window — would leave the ledgers
// beyond it out of the board while the first arm's tiling still looked
// complete.
func TestRunRollupStepsWalksEveryWindowOnce(t *testing.T) {
	// r1's lake tip on 2026-09-06; account_movements holds 65 partitions.
	const tip = 64_303_935
	const wantWindows = 65

	for name, steps := range rollupCycles() {
		t.Run(name, func(t *testing.T) {
			rec := &recordingExecer{}
			if err := runRollupSteps(context.Background(), rec, tip, name, steps,
				func(string, ...any) {}); err != nil {
				t.Fatalf("runRollupSteps: %v", err)
			}

			var walked, once int
			for _, step := range steps {
				if step.walk {
					walked++
				} else {
					once++
				}
			}
			if walked < 1 {
				t.Fatal("cycle has no walked step; its cost is the whole archive")
			}
			if got, want := len(rec.calls), once+walked*wantWindows; got != want {
				t.Fatalf("issued %d statements, want %d (%d one-shot + %d walked x %d windows)",
					got, want, once, walked, wantWindows)
			}

			// Group the windowed calls by the template that issued them,
			// so each walked arm's tiling is checked on its own.
			nextByStep := map[string]uint32{}
			windowsByStep := map[string]int{}
			for _, call := range rec.calls {
				if len(call.args) == 0 {
					continue
				}
				step, ok := walkedStepFor(steps, call.sql)
				if !ok {
					t.Fatalf("windowed statement matches no walked step:\n%s", call.sql)
				}
				pairs := step.windowBinds
				if pairs < 1 {
					pairs = 1
				}
				if len(call.args) != 2*pairs {
					t.Fatalf("windowed statement carries %d bounds, want %d (%d pair(s))",
						len(call.args), 2*pairs, pairs)
				}
				lo, loOK := call.args[0].(uint32)
				hi, hiOK := call.args[1].(uint32)
				if !loOK || !hiOK {
					t.Fatalf("window bounds are %T/%T, want uint32 ledger sequences",
						call.args[0], call.args[1])
				}
				// Every pair binds the SAME window: a template that
				// bounds two sources must bound them to one another, or
				// the join reads across windows.
				for k := 1; k < pairs; k++ {
					if call.args[2*k] != any(lo) || call.args[2*k+1] != any(hi) {
						t.Fatalf("bind pair %d is [%v,%v], want the same window [%d,%d] as pair 1",
							k+1, call.args[2*k], call.args[2*k+1], lo, hi)
					}
				}
				if _, seen := nextByStep[call.sql]; !seen {
					nextByStep[call.sql] = 1
				}
				if lo != nextByStep[call.sql] {
					t.Errorf("window %d starts at ledger %d, want %d — the walk left a gap",
						windowsByStep[call.sql]+1, lo, nextByStep[call.sql])
				}
				if hi < lo || uint64(hi)-uint64(lo) >= rollupLedgerWindow {
					t.Errorf("window [%d,%d] is not one %d-ledger partition",
						lo, hi, rollupLedgerWindow)
				}
				nextByStep[call.sql] = hi + 1
				windowsByStep[call.sql]++
			}
			if len(windowsByStep) != walked {
				t.Errorf("%d walked template(s) issued windows, want %d", len(windowsByStep), walked)
			}
			for sql, windows := range windowsByStep {
				if windows != wantWindows {
					t.Errorf("walked %d windows, want %d, for:\n%s", windows, wantWindows, sql)
				}
				if next := nextByStep[sql]; next != tip+1 {
					t.Errorf("the walk stopped at ledger %d, want the tip %d, for:\n%s",
						next-1, tip, sql)
				}
			}
		})
	}
}

// walkedStepFor resolves the walked template an issued statement came
// from.
func walkedStepFor(steps []rollupStep, sql string) (rollupStep, bool) {
	for _, step := range steps {
		if step.walk && step.sql == sql {
			return step, true
		}
	}
	return rollupStep{}, false
}

// TestRunRollupStepsRefusesAnEmptyLake: a zero tip yields no windows,
// so a cycle that proceeded would write empty staging arms and then
// EXCHANGE them over a populated board — turning a transient zero read
// into an outage lasting until the next daily cycle. The board and its
// span swap together, so keeping the previous pair serves older data
// with its own honest span rather than a board beside a foreign one.
func TestRunRollupStepsRefusesAnEmptyLake(t *testing.T) {
	for name, steps := range rollupCycles() {
		t.Run(name, func(t *testing.T) {
			rec := &recordingExecer{}
			err := runRollupSteps(context.Background(), rec, 0, name, steps,
				func(string, ...any) {})
			if err == nil {
				t.Fatal("a zero tip built a board instead of refusing")
			}
			if !strings.Contains(err.Error(), "lake tip is 0") {
				t.Errorf("error does not name the cause: %v", err)
			}
			if len(rec.calls) != 0 {
				t.Errorf("a zero tip issued %d statement(s); the live board must be left untouched",
					len(rec.calls))
			}
		})
	}
}
