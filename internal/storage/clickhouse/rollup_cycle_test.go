// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"strings"
	"testing"
)

// lakeArchiveTables are the append-only, ledger-partitioned archives a
// rollup cycle reads. Their row counts are functions of chain history —
// 10,309,271,697 rows / 583.54 GiB and 24,736,312,156 rows / 2.18 TiB on
// r1 2026-09-06 — so a statement that reads one of them without a ledger
// window has a cost nothing bounds.
var lakeArchiveTables = []string{"stellar.account_movements", "stellar.operations"}

// rollupCycles is every recompute cycle this package ships, so a cycle
// added later inherits the walk guards below rather than having to
// remember them.
func rollupCycles() map[string][]rollupStep {
	return map[string][]rollupStep{
		"creators": creatorsRollupStatements,
		"sponsors": sponsorsRollupStatements,
	}
}

// TestRollupCyclesWalkEveryArchiveScan is the guard for the failure that
// took both boards off the air: a statement whose cost is a function of
// the whole archive rather than of one partition. The creator board's
// unwalked statement read all 10,309,146,441 movement rows and died at
// 8.12 GiB against an 8 GiB budget, so its endpoint served 503.
//
// Any statement touching a lake archive must therefore be a WALKED step
// carrying the window bounds, and no other step may touch one — a cycle
// that reads the archive a second time to qualify its own board has
// re-introduced the same unbounded cost by another door.
func TestRollupCyclesWalkEveryArchiveScan(t *testing.T) {
	for name, steps := range rollupCycles() {
		t.Run(name, func(t *testing.T) {
			touching := 0
			for i, step := range steps {
				reads := ""
				for _, table := range lakeArchiveTables {
					if strings.Contains(step.sql, table) {
						reads = table
					}
				}
				if reads == "" {
					if step.walk {
						t.Errorf("step %d is walked but reads no lake archive; "+
							"a walked step must carry the window predicate its bounds bind to", i+1)
					}
					continue
				}
				touching++
				if !step.walk {
					t.Errorf("step %d reads %s unwalked — its cost is the whole archive, "+
						"not one partition:\n%s", i+1, reads, step.sql)
					continue
				}
				if !strings.Contains(step.sql, "BETWEEN ? AND ?") {
					t.Errorf("step %d is walked but carries no ledger-window predicate, "+
						"so every window would rescan the whole archive:\n%s", i+1, step.sql)
				}
			}
			if touching != 1 {
				t.Errorf("steps touching a lake archive: %d, want exactly 1", touching)
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
// r1's real tip and pins what it issues: the walked step once per
// 1M-ledger window, over contiguous windows that cover ledger 1 through
// the tip with no gap and no overlap, and every other step exactly once
// with no bounds.
//
// The window bounds are the corrected value here. An unwalked cycle
// issues the archive statement ONCE with no bounds, which is the shape
// that read 10,309,146,441 rows and exhausted the memory budget.
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
			if walked != 1 {
				t.Fatalf("cycle has %d walked steps, want 1", walked)
			}
			if got, want := len(rec.calls), once+wantWindows; got != want {
				t.Fatalf("issued %d statements, want %d (%d one-shot + %d windows)",
					got, want, once, wantWindows)
			}

			// Every windowed call must carry exactly two bounds, and the
			// windows must tile [1,tip] end to end.
			next := uint32(1)
			windows := 0
			for _, call := range rec.calls {
				if len(call.args) == 0 {
					continue
				}
				if len(call.args) != 2 {
					t.Fatalf("windowed statement carries %d bounds, want 2", len(call.args))
				}
				lo, loOK := call.args[0].(uint32)
				hi, hiOK := call.args[1].(uint32)
				if !loOK || !hiOK {
					t.Fatalf("window bounds are %T/%T, want uint32 ledger sequences",
						call.args[0], call.args[1])
				}
				if lo != next {
					t.Errorf("window %d starts at ledger %d, want %d — the walk left a gap",
						windows+1, lo, next)
				}
				if hi < lo || uint64(hi)-uint64(lo) >= rollupLedgerWindow {
					t.Errorf("window [%d,%d] is not one %d-ledger partition",
						lo, hi, rollupLedgerWindow)
				}
				next = hi + 1
				windows++
			}
			if windows != wantWindows {
				t.Errorf("walked %d windows, want %d", windows, wantWindows)
			}
			if next != tip+1 {
				t.Errorf("the walk stopped at ledger %d, want the tip %d", next-1, tip)
			}
		})
	}
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
