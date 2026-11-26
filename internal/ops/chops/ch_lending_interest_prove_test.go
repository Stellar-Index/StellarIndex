package chops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// proofFixture is the fake lake after the writer ran over it, with the rows it inserted as the stored table.
type proofFixture struct {
	*fakeInterestLake
	stored []clickhouse.LendingInterestStoredRow
}

func newProofFixture(t *testing.T, edit ...func(*fakeInterestLake)) *proofFixture {
	t.Helper()
	f := newFakeInterestLake()
	for _, e := range edit {
		e(f)
	}
	f.install(t)
	if out, err := runInterest(t, lendingRun{}); err != nil {
		t.Fatalf("writer: %v\n%s", err, out)
	}
	f.floor, f.wm = 199_990, f.marks[len(f.marks)-1][1]
	p := &proofFixture{fakeInterestLake: f}
	for _, r := range f.inserted {
		s := r
		// The columns the table does not store.
		s.Baselines, s.ExcludedCreditDelta, s.FeeOnlyCreditDelta, s.FeeOnlyCarryDelta, s.FeeOnlyVersions, s.FeeOnlyUnknown = 0, nil, nil, nil, 0, 0
		s.Excluded = nil
		p.stored = append(p.stored, clickhouse.LendingInterestStoredRow{
			Row: s, ResidualExplained: new(big.Int).Set(r.ResidualExplained), ResidualAttributed: new(big.Int).Set(r.ResidualAttributed),
			ExplainedVersions: r.ExplainedVersions, AttributedVersions: r.AttributedVersions, DeriveRev: lendingInterestRev,
		})
	}
	if len(p.stored) != 4 {
		t.Fatalf("degenerate fixture: the writer stored %d rows", len(p.stored))
	}
	a, b := lendingInterestStored, lendingInterestBeyond
	t.Cleanup(func() { lendingInterestStored, lendingInterestBeyond = a, b })
	lendingInterestStored = func(_ context.Context, _ string, pools []string, from, to time.Time) ([]clickhouse.LendingInterestStoredRow, error) {
		var out []clickhouse.LendingInterestStoredRow
		for _, s := range p.stored {
			if slices.Contains(pools, s.Row.Pool) && !s.Row.Hour.Before(from) && s.Row.Hour.Before(to) {
				out = append(out, s)
			}
		}
		return out, nil
	}
	lendingInterestBeyond = func(_ context.Context, _ string, pools []string, after time.Time) ([]clickhouse.LendingInterestRowID, error) {
		var out []clickhouse.LendingInterestRowID
		for _, s := range p.stored {
			if s.Row.Hour.After(after) || !slices.Contains(pools, s.Row.Pool) {
				out = append(out, clickhouse.LendingInterestRowID{Pool: s.Row.Pool, Asset: s.Row.Asset, Hour: s.Row.Hour, DeriveRev: s.DeriveRev})
			}
		}
		return out, nil
	}
	return p
}

// prove runs -prove -interest and returns its report, error and the proof_failed gauge from its textfile; the
// textfile lives beside r.heartbeat when set (so a later run reads it back), else in a fresh directory.
func (p *proofFixture) prove(t *testing.T, r lendingRun) (out, gauge string, err error) {
	t.Helper()
	if r.heartbeat == "" {
		r.heartbeat = filepath.Join(t.TempDir(), "state.prom")
	}
	dir := filepath.Dir(r.heartbeat)
	r.chAddr = "fake"
	if r.lockFile == "" {
		r.lockFile = filepath.Join(dir, "state.lock")
	}
	var report, log bytes.Buffer
	err = r.proveLendingInterest(context.Background(), &report, &log)
	raw, rerr := os.ReadFile(filepath.Join(dir, "ops_job_"+r.proveJob()+".prom")) //nolint:gosec // test temp dir
	if rerr != nil {
		t.Fatal(rerr)
	}
	gauge = "absent"
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, metricInterestProofFailed+"{") {
			gauge = l[strings.LastIndex(l, " ")+1:]
		}
	}
	return report.String(), gauge, err
}

// The writer's own rows prove: every hour, with the identities re-run over the stored values; the gauge is 0.
func TestProveInterest_WritersRowsProve(t *testing.T) {
	p := newProofFixture(t)
	out, gauge, err := p.prove(t, lendingRun{})
	if err != nil || gauge != "0" {
		t.Fatalf("err %v gauge %s\n%s", err, gauge, out)
	}
	for _, want := range []string{
		"lending_interest CPOOL CASSET: 4 hours proved, 0 mismatched, 0 INCOMPLETE",
		": PROVED: 4 hours proved, 0 mismatches, 0 INCOMPLETE, 0 unmarked",
		"gross telescoped 4 (0 around 0 excluded pairs)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// Each mismatch class is named with pool, asset, hour and column, fails the run and sets the gauge.
func TestProveInterest_Mismatches(t *testing.T) {
	h1 := liClose(199_990).Add(time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name   string
		mutate func(p *proofFixture)
		want   string
	}{
		{
			"missing row", func(p *proofFixture) { p.stored = slices.Delete(p.stored, 1, 2) },
			"mismatch CPOOL CASSET " + h1 + " row: stored none, recomputed present (versions 2)",
		},
		{"stale extra row", func(p *proofFixture) {
			s := p.stored[1]
			s.Row.Asset = "COTHER"
			p.stored = append(p.stored, s)
		}, "mismatch CPOOL COTHER " + h1 + " row: stored present, recomputed none"},
		{"altered sum", func(p *proofFixture) {
			p.stored[1].Row.Gross = new(big.Int).Add(p.stored[1].Row.Gross, big.NewInt(1))
		}, "mismatch CPOOL CASSET " + h1 + " gross: stored "},
		{
			"altered count", func(p *proofFixture) { p.stored[1].Row.Versions++ },
			"mismatch CPOOL CASSET " + h1 + " versions (the lake's count): stored 3, recomputed 2",
		},
		{
			"altered counter", func(p *proofFixture) { p.stored[1].Row.AccrualErrors = 1 },
			"mismatch CPOOL CASSET " + h1 + " accrual_errors: stored 1, recomputed 0",
		},
		{
			"event column", func(p *proofFixture) { p.stored[1].ExplainedVersions = 1 },
			"mismatch CPOOL CASSET " + h1 + " explained_versions: stored 1, recomputed 0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProofFixture(t)
			tc.mutate(p)
			out, gauge, err := p.prove(t, lendingRun{})
			if err == nil || gauge != "1" || !strings.Contains(out, tc.want) || !strings.Contains(out, ": MISMATCH: ") {
				t.Fatalf("err %v gauge %s, want a failure listing %q:\n%s", err, gauge, tc.want, out)
			}
		})
	}
}

// A row of another derive revision is INCOMPLETE, not compared: non-zero exit, but no proof failure. Revision 1
// rows predate the event classification, so every one of them is INCOMPLETE until re-derived.
func TestProveInterest_StaleRevisionIsIncomplete(t *testing.T) {
	p := newProofFixture(t)
	p.stored[2].DeriveRev = 1
	p.stored[2].Row.Gross = big.NewInt(-1) // an older arithmetic's value is not a mismatch
	out, gauge, err := p.prove(t, lendingRun{})
	if err == nil || !strings.Contains(err.Error(), "INCOMPLETE") || gauge != "0" ||
		!strings.Contains(out, "derive_rev 1, this binary derives 3") || !strings.Contains(out, "3 hours proved, 0 mismatched, 1 INCOMPLETE") {
		t.Fatalf("err %v gauge %s:\n%s", err, gauge, out)
	}
}

// A row past the watermark is listed as unmarked and neither proves nor fails; -to and -interest-pool narrow the range.
func TestProveInterest_UnmarkedAndRange(t *testing.T) {
	p := newProofFixture(t)
	s := p.stored[3]
	s.Row.Hour = s.Row.Hour.Add(5 * time.Hour)
	p.stored = append(p.stored, s)
	out, _, err := p.prove(t, lendingRun{})
	if err != nil || !strings.Contains(out, "unmarked CPOOL CASSET "+s.Row.Hour.Format(time.RFC3339)) || !strings.Contains(out, "1 unmarked") {
		t.Fatalf("err %v:\n%s", err, out)
	}
	out, _, err = p.prove(t, lendingRun{to: 199_996, interest: interestFlags{pool: liPool}})
	if err != nil || !strings.Contains(out, "1 hours proved") || !strings.Contains(out, "[199990,199996]") {
		t.Fatalf("-to one hour: err %v:\n%s", err, out)
	}
	if _, _, err := p.prove(t, lendingRun{interest: interestFlags{pool: "CNOPE"}}); err == nil {
		t.Fatal("-interest-pool of an unknown pool succeeded")
	}
}

// A pool whose coverage row stops below the watermark is proved to it and reported INCOMPLETE past it.
func TestProveInterest_UncoveredPool(t *testing.T) {
	p := newProofFixture(t)
	p.gaps = []clickhouse.LendingInterestPoolGap{{Pool: liPool, Deployed: 1, Thru: 199_996}}
	out, _, err := p.prove(t, lendingRun{})
	if err == nil || !strings.Contains(out, "coverage row stops at 199996") || !strings.Contains(out, "1 hours proved") ||
		!strings.Contains(out, "3 unmarked") {
		t.Fatalf("err %v:\n%s", err, out)
	}
}

// An uninitialised host has nothing to prove: one line, exit 0, gauge 0.
func TestProveInterest_Uninitialised(t *testing.T) {
	p := newProofFixture(t)
	p.wm = 0
	out, gauge, err := p.prove(t, lendingRun{})
	if err != nil || gauge != "0" || out != "lending interest: not initialised\n" {
		t.Fatalf("err %v gauge %s out %q", err, gauge, out)
	}
}

// The identities re-run over the lake: a lending_state row that moved after the write fails the proof.
func TestProveInterest_IdentityOverTheLake(t *testing.T) {
	p := newProofFixture(t)
	p.tamper = func(rows []clickhouse.LendingInterestStateRow) {
		for i := range rows {
			if !rows[i].Before && rows[i].Ledger == 199_994 {
				r := rows[i].Value.Reserve
				r.BackstopCredit = new(big.Int).Add(r.BackstopCredit, big.NewInt(1))
				rows[i].Value.Reserve = r
			}
		}
	}
	out, gauge, err := p.prove(t, lendingRun{})
	if err == nil || gauge != "1" || !strings.Contains(out, "the lake's versions fail the in-line identities") {
		t.Fatalf("err %v gauge %s:\n%s", err, gauge, out)
	}
}

// An hour with an excluded pair proves: the stored row's gross telescopes around the pair the fresh fold excluded,
// which the table does not store.
func TestProveInterest_ExcludedPairTelescopes(t *testing.T) {
	p := newProofFixture(t, func(f *fakeInterestLake) {
		// After 199_998, in ledger order; its d_rate is below 199_998's: an accrual error.
		f.versions = slices.Insert(f.versions, 3, liReserve(200_000, 1))
	})
	var excluded int
	for _, s := range p.stored {
		excluded += int(s.Row.AccrualErrors + s.Row.UnorderedVersions)
	}
	if excluded != 1 {
		t.Fatalf("degenerate fixture: %d excluded pairs stored, want 1", excluded)
	}
	out, gauge, err := p.prove(t, lendingRun{})
	if err != nil || gauge != "0" || !strings.Contains(out, ": PROVED: 4 hours proved, 0 mismatches") ||
		!strings.Contains(out, "gross telescoped 4 (1 around 1 excluded pairs)") {
		t.Fatalf("err %v gauge %s:\n%s", err, gauge, out)
	}
}

// Under a pending redo the hours from the redo's hour on are INCOMPLETE, not compared; the hours below it prove.
func TestProveInterest_RedoPendingIsIncomplete(t *testing.T) {
	p := newProofFixture(t)
	p.redo = &clickhouse.LendingInterestRedo{Gen: 1, From: 199_998} // mid-hour: the hour from 199_997 is stale too
	p.stored[1].Row.Gross = big.NewInt(-1)                          // a stale row past the cut is not a mismatch
	out, gauge, err := p.prove(t, lendingRun{})
	if err == nil || !strings.Contains(err.Error(), "INCOMPLETE") || gauge != "0" ||
		!strings.Contains(out, "INCOMPLETE redo pending from ledger 199998 (re-derived through 0): ledgers [199997,200017] not compared") ||
		!strings.Contains(out, "[199990,199996] @200017: INCOMPLETE: 1 hours proved, 0 mismatches") {
		t.Fatalf("err %v gauge %s:\n%s", err, gauge, out)
	}
}

// A seed the lake does not prove, or whose proof read timed out, leaves its pools INCOMPLETE with the reason, never
// proved; the writer refuses both, writing nothing.
func TestProveInterest_UnprovedSeedIsIncomplete(t *testing.T) {
	for _, sentinel := range []error{clickhouse.ErrLendingInterestSeedUnproved, clickhouse.ErrLendingInterestSeedProofTimeout} {
		seeds := func(context.Context, string, []clickhouse.LendingContract, []clickhouse.LendingInterestKey, time.Time, uint32,
		) (clickhouse.LendingInterestSeedSet, error) {
			return clickhouse.LendingInterestSeedSet{}, fmt.Errorf("reserve res at ledger 9: %w", sentinel)
		}
		p := newProofFixture(t)
		lendingInterestSeeds = seeds
		out, gauge, err := p.prove(t, lendingRun{})
		if err == nil || !strings.Contains(err.Error(), "INCOMPLETE") || gauge != "0" ||
			!strings.Contains(out, "INCOMPLETE pools CPOOL [199990,200017]: not compared, the seed is not proved") ||
			!strings.Contains(out, ": INCOMPLETE: 0 hours proved") {
			t.Fatalf("%v: err %v gauge %s:\n%s", sentinel, err, gauge, out)
		}
		f := newFakeInterestLake()
		f.install(t)
		lendingInterestSeeds = seeds
		if _, err := runInterest(t, lendingRun{}); !errors.Is(err, sentinel) || len(f.inserted) != 0 || len(f.marks) != 0 {
			t.Fatalf("%v: writer err %v inserted %d marks %v, want the refusal and nothing written", sentinel, err, len(f.inserted), f.marks)
		}
	}
}

// stubProveLockSleep counts the proof's lock waits instead of sleeping; at calls == release it runs onRelease.
func stubProveLockSleep(t *testing.T, release int, onRelease func()) *int {
	t.Helper()
	calls := new(int)
	orig := proveLockSleep
	t.Cleanup(func() { proveLockSleep = orig })
	proveLockSleep = func(_ context.Context, d time.Duration) error {
		if d != proveLockRetry {
			t.Errorf("waited %s, want %s", d, proveLockRetry)
		}
		*calls++
		if *calls == release {
			onRelease()
		}
		return nil
	}
	return calls
}

// Against a writer holding -lock-file or the interest lock past the 15-minute wait, the unit's fire skips with exit 0
// and writes no heartbeat (so proof_stale still ages); a run by hand refuses. Neither reads the lake.
func TestProveInterest_LockHeldPastTheWait(t *testing.T) {
	for _, lock := range []string{"state.lock", interestLock} {
		t.Run(lock, func(t *testing.T) {
			p := newProofFixture(t)
			dir := t.TempDir()
			r := lendingRun{chAddr: "fake", lockFile: filepath.Join(dir, "state.lock"), heartbeat: filepath.Join(dir, "state.prom")}
			unlock, err := acquireRollupLock("writer", filepath.Join(dir, lock))
			if err != nil {
				t.Fatal(err)
			}
			calls := stubProveLockSleep(t, -1, nil)
			reads := len(p.reads)
			var out, log bytes.Buffer
			t.Setenv("INVOCATION_ID", "")
			if err := r.proveLendingInterest(context.Background(), &out, &log); err == nil || !strings.Contains(err.Error(), "prove once it finishes") {
				t.Fatalf("run by hand under the lock: err %v, want a refusal", err)
			}
			if want := int(proveLockWait / proveLockRetry); *calls != want {
				t.Fatalf("waited %d times before refusing, want %d (15 min every 10 s)", *calls, want)
			}
			t.Setenv("INVOCATION_ID", "0123456789abcdef")
			if err := r.proveLendingInterest(context.Background(), &out, &log); err != nil ||
				!strings.Contains(log.String(), "skipped, a lending_state or interest write run held its lock past 15m0s") {
				t.Fatalf("unit fire under the lock: err %v, log %q; want a logged skip", err, log.String())
			}
			if _, err := os.Stat(filepath.Join(dir, "ops_job_"+r.proveJob()+".prom")); !os.IsNotExist(err) || len(p.reads) != reads || out.Len() != 0 {
				t.Fatalf("a skipped proof wrote its heartbeat (%v), read the lake (%d reads) or reported %q", err, len(p.reads)-reads, out.String())
			}
			unlock()
			if out, _, err := p.prove(t, lendingRun{lockFile: r.lockFile}); err != nil {
				t.Fatalf("proof once the writer finished: %v\n%s", err, out)
			}
		})
	}
}

// A fire that meets a short state pass waits it out and proves, rather than skipping the day.
func TestProveInterest_WaitsOutAShortWriter(t *testing.T) {
	p := newProofFixture(t)
	dir := t.TempDir()
	lock := filepath.Join(dir, "state.lock")
	unlock, err := acquireRollupLock(lendingJob, lock)
	if err != nil {
		t.Fatal(err)
	}
	calls := stubProveLockSleep(t, 3, unlock)
	t.Setenv("INVOCATION_ID", "0123456789abcdef")
	out, gauge, err := p.prove(t, lendingRun{lockFile: lock})
	if err != nil || gauge != "0" || *calls != 3 || !strings.Contains(out, ": PROVED: 4 hours proved") {
		t.Fatalf("err %v gauge %s waits %d:\n%s", err, gauge, *calls, out)
	}
	// The proof holds the interest lock shared: an interest writer (exclusive) cannot start inside it.
	unlockProof, err := lendingRun{lockFile: lock}.proveLocks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlockProof()
	if _, err := acquireRollupLock(lendingInterestJob, filepath.Join(dir, interestLock)); err == nil {
		t.Fatal("an interest writer took its lock while the proof held it")
	}
}

// A run that errors before its verdict, or a spot check or INCOMPLETE run that proves, keeps the last verdict; only
// a whole-range run that compares everything clears it.
func TestProveInterest_OnlyAWholeVerdictClearsAMismatch(t *testing.T) {
	p := newProofFixture(t)
	hb := filepath.Join(t.TempDir(), "state.prom")
	good := p.stored[1].Row.Gross
	p.stored[1].Row.Gross = big.NewInt(-1)
	if _, gauge, err := p.prove(t, lendingRun{heartbeat: hb}); err == nil || gauge != "1" {
		t.Fatalf("mismatch: err %v gauge %s", err, gauge)
	}
	p.stored[1].Row.Gross = good
	beyond := lendingInterestBeyond
	lendingInterestBeyond = func(context.Context, string, []string, time.Time) ([]clickhouse.LendingInterestRowID, error) {
		return nil, fmt.Errorf("lake down")
	}
	if _, gauge, err := p.prove(t, lendingRun{heartbeat: hb}); err == nil || gauge != "1" {
		t.Fatalf("errored run: err %v gauge %s, want the last verdict 1 kept", err, gauge)
	}
	lendingInterestBeyond = beyond
	if _, gauge, err := p.prove(t, lendingRun{heartbeat: hb, to: 199_996}); err != nil || gauge != "1" {
		t.Fatalf("-to spot check: err %v gauge %s, want the last verdict 1 kept", err, gauge)
	}
	if _, gauge, err := p.prove(t, lendingRun{heartbeat: hb}); err != nil || gauge != "0" {
		t.Fatalf("whole range proved: err %v gauge %s, want 0", err, gauge)
	}
}

// A row of a contract the registry does not list is a mismatch at or below the watermark's hour, even when no pool
// has a coverage row to take that hour from.
func TestProveInterest_UnregisteredContract(t *testing.T) {
	for _, gaps := range [][]clickhouse.LendingInterestPoolGap{nil, {{Pool: liPool, Deployed: 1, Thru: 0}}} {
		p := newProofFixture(t)
		p.gaps = gaps
		s := p.stored[0]
		s.Row.Pool = "CFOREIGN"
		p.stored = append(p.stored, s)
		out, gauge, err := p.prove(t, lendingRun{})
		if err == nil || gauge != "1" || !strings.Contains(out, "mismatch CFOREIGN CASSET "+s.Row.Hour.Format(time.RFC3339)+
			" row: stored present, recomputed none: the contract is not a pool in the registry") {
			t.Fatalf("gaps %v: err %v gauge %s:\n%s", gaps, err, gauge, out)
		}
	}
}

// -prove-recent proves from the hour of watermark − N on, on the daily heartbeat; without it, the full range's.
func TestProveInterest_Recent(t *testing.T) {
	p := newProofFixture(t)
	r := lendingRun{interest: interestFlags{recent: 10}}
	out, gauge, err := p.prove(t, r)
	if err != nil || gauge != "0" || !strings.Contains(out, "lending_interest [200004,200017] @200017: PROVED: 2 hours proved") ||
		r.proveJob() != lendingInterestProveJob || (lendingRun{}).proveJob() != lendingInterestProveJob+"-full" {
		t.Fatalf("err %v gauge %s:\n%s", err, gauge, out)
	}
	out, _, err = p.prove(t, lendingRun{interest: interestFlags{recent: 1_000_000}})
	if err != nil || !strings.Contains(out, "[199990,200017]") {
		t.Fatalf("-prove-recent past the floor: err %v:\n%s", err, out)
	}
	for _, bad := range []lendingRun{
		{prove: true, interest: interestFlags{recent: 10}},
		{prove: true, from: 199_997, interest: interestFlags{prove: true, recent: 10}},
		{interest: interestFlags{recent: 10}},
	} {
		if err := bad.interest.check(bad); err == nil {
			t.Errorf("%+v: -prove-recent accepted", bad)
		}
	}
}
