package chops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// liHeartbeat is a -heartbeat path in a temp directory and a reader of the interest pass's textfile beside it.
func liHeartbeat(t *testing.T) (string, func() string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "ops_job_"+lendingJob+".prom"), func() string {
		b, err := os.ReadFile(filepath.Join(dir, "ops_job_"+lendingInterestJob+".prom"))
		if err != nil {
			t.Fatalf("interest heartbeat: %v", err)
		}
		return string(b)
	}
}

// metric is the value of the textfile's first sample of name.
func metric(text, name string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, name+"{") || strings.HasPrefix(l, name+" ") {
			return l[strings.LastIndex(l, " ")+1:]
		}
	}
	return ""
}

// The timer's pass never does backfill-scale work: more than one window behind lending_state it reads nothing, says
// so, and leaves its cursor at the watermark for the lagging alert; within one window it catches up in two windows.
func TestInterestPass_TimerSkipsABackfill(t *testing.T) {
	f := newFakeInterestLake()
	f.wm = 100_015 // an hour's last ledger, 100,002 behind lending_state
	f.install(t)
	hb, read := liHeartbeat(t)
	out, err := runInterest(t, lendingRun{heartbeat: hb})
	if err != nil || !strings.Contains(out, "lending interest: backfill incomplete (100002 ledgers behind; run the backfill chunks)") {
		t.Fatalf("timer run: %v %q, want nil and the backfill-incomplete line", err, out)
	}
	if len(f.reads)+len(f.calls)+f.seeds != 0 {
		t.Fatalf("timer run did backfill work: reads %v calls %v", f.reads, f.calls)
	}
	if text := read(); metric(text, "stellarindex_ops_job_progress_cursor") != "100015" || metric(text, "stellarindex_ops_job_last_exit_ok") != "1" {
		t.Fatalf("heartbeat:\n%s\nwant cursor 100015 and a clean exit", text)
	}
	// The same backlog by hand (a backfill chunk) runs.
	if _, err := runInterest(t, lendingRun{interest: interestFlags{only: true, maxWindows: 1}}); err != nil || len(f.reads) != 1 {
		t.Fatalf("-interest-only chunk: %v reads %v", err, f.reads)
	}
	f.wm, f.reads, f.marks, f.calls = 100_022, nil, nil, nil // 99,995 behind
	if _, err := runInterest(t, lendingRun{}); err != nil {
		t.Fatal(err)
	}
	if len(f.reads) != 2 || f.marks[len(f.marks)-1][1] != f.stateWM {
		t.Fatalf("one window behind: reads %v marks %v, want two windows to %d", f.reads, f.marks, f.stateWM)
	}
}

// A refusal while planning (here an uncovered pool) is a failed run on the interest heartbeat, cursor at the watermark.
func TestInterestPass_RefusalBeforeTheFirstWindowFailsTheHeartbeat(t *testing.T) {
	f := newFakeInterestLake()
	f.gaps = []clickhouse.LendingInterestPoolGap{{Pool: liPool, Deployed: 1}}
	f.install(t)
	hb, read := liHeartbeat(t)
	if _, err := runInterest(t, lendingRun{heartbeat: hb}); err == nil || !strings.Contains(err.Error(), "-interest-pool") {
		t.Fatalf("err = %v, want the uncovered-pool refusal", err)
	}
	if text := read(); metric(text, "stellarindex_ops_job_last_exit_ok") != "0" || metric(text, "stellarindex_ops_job_progress_cursor") != "199989" {
		t.Fatalf("heartbeat:\n%s\nwant a failed run at cursor 199989", text)
	}
}

// An instance version naming code the timeline does not have in force (the instance index stopped short of an
// upgrade) refuses the window; the matching hash passes.
func TestInterestPass_InstanceCodeCrossCheck(t *testing.T) {
	cfg := lendinginterest.Version{
		Pool: liPool, Kind: lendinginterest.KindConfig, Ledger: 199_995, IntraLedgerSeq: 1, TxHash: "cfg",
		CloseTime: liClose(199_995), BstopRate: 2_000_000, WasmHash: liV2,
	}
	f := newFakeInterestLake()
	f.versions = append(f.versions, cfg)
	f.install(t)
	if _, err := runInterest(t, lendingRun{}); err != nil {
		t.Fatal(err)
	}
	g := newFakeInterestLake()
	cfg.WasmHash = strings.Repeat("ab", 32)
	g.versions = append(g.versions, cfg)
	g.install(t)
	_, err := runInterest(t, lendingRun{})
	if err == nil || !strings.Contains(err.Error(), "timeline is behind the lake") || len(g.marks) != 0 || len(g.inserted) != 0 {
		t.Fatalf("err = %v marks %v, want the cross-check refusal and nothing written", err, g.marks)
	}
}

// A pool with no ResData key (dead, or not yet written) folds to no rows, skips the state read and advances.
func TestInterestPass_PoolWithoutReserveKeysAdvances(t *testing.T) {
	f := newFakeInterestLake()
	f.noReserveKeys, f.versions = true, nil
	f.install(t)
	if _, err := runInterest(t, lendingRun{}); err != nil {
		t.Fatal(err)
	}
	if want := [][2]uint32{{199_990, 199_996}, {199_997, 200_017}}; len(f.inserted) != 0 || !slices.Equal(f.marks, want) {
		t.Fatalf("inserted %d marks %v, want none and %v", len(f.inserted), f.marks, want)
	}
}

// Manual interest runs take only the interest lock, so a held lending_state lock does not stop them; a held interest
// lock refuses a manual run and makes the timer's pass skip quietly.
func TestLendingRun_InterestLocks(t *testing.T) {
	f := newFakeInterestLake()
	f.install(t)
	dir := t.TempDir()
	hb, _ := liHeartbeat(t)
	r := lendingRun{chAddr: "fake", lockFile: filepath.Join(dir, "ch-entry-history-lending.lock"), heartbeat: hb, interest: interestFlags{only: true}}
	unlock, err := acquireRollupLock(lendingJob, r.lockFile)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := r.run(); err != nil || len(f.marks) == 0 {
		t.Fatalf("-interest-only beside a held state lock: %v marks %v", err, f.marks)
	}
	unlockI, err := acquireRollupLock(lendingInterestJob, filepath.Join(dir, interestLock))
	if err != nil {
		t.Fatal(err)
	}
	defer unlockI()
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("manual run beside a held interest lock: %v, want a refusal", err)
	}
	f.reads = nil
	var log bytes.Buffer
	timer := lendingRun{chAddr: "fake", lockFile: r.lockFile, heartbeat: hb}
	if err := timer.lockedInterestPass(context.Background(), &log); err != nil || !strings.Contains(log.String(), "skipped") || len(f.reads) != 0 {
		t.Fatalf("timer pass beside a held interest lock: %v %q reads %v", err, log.String(), f.reads)
	}
}

// Each storage refusal of a mark leaves nothing marked; the rows inserted before it are rewritten by the re-run,
// which converges on one fold's rows.
func TestInterestPass_RefusedMarkThenRerunConverges(t *testing.T) {
	for _, refusal := range []error{
		clickhouse.ErrLendingInterestNotHourEnd, clickhouse.ErrLendingInterestNotHourStart,
		clickhouse.ErrLendingInterestPoolUncovered, clickhouse.ErrLendingInterestAheadOfState,
	} {
		f := newFakeInterestLake()
		f.refuse = refusal
		f.install(t)
		if _, err := runInterest(t, lendingRun{}); !errors.Is(err, refusal) || len(f.marks) != 0 || len(f.inserted) == 0 {
			t.Fatalf("%v: err %v marks %v inserted %d, want the refusal, nothing marked, the first window inserted", refusal, err, f.marks, len(f.inserted))
		}
		f.refuse = nil
		if _, err := runInterest(t, lendingRun{}); err != nil {
			t.Fatal(err)
		}
		if want := [][2]uint32{{199_990, 199_996}, {199_997, 200_017}}; !slices.Equal(f.marks, want) {
			t.Fatalf("%v: re-run marks %v, want %v", refusal, f.marks, want)
		}
		latest := map[string]lendinginterest.HourRow{} // the table's FINAL view: one row per (pool, asset, hour)
		for _, r := range f.inserted {
			latest[r.Hour.String()] = r
		}
		if len(latest) != 4 {
			t.Fatalf("%v: %d distinct hours after the re-run, want 4", refusal, len(latest))
		}
	}
}

// An hour the lake has not closed at the tip is not committed: the mark stops at the previous hour's last ledger.
func TestInterestPass_OpenHourAtTheTipWaits(t *testing.T) {
	f := newFakeInterestLake()
	f.tip = 200_017 // 200_017 ends its hour, but 200_018 is not in the lake yet
	f.install(t)
	if _, err := runInterest(t, lendingRun{}); err != nil {
		t.Fatal(err)
	}
	if want := [][2]uint32{{199_990, 199_996}, {199_997, 200_010}}; !slices.Equal(f.marks, want) {
		t.Fatalf("marks %v, want %v", f.marks, want)
	}
	for _, r := range f.inserted {
		if !r.Hour.Before(liClose(200_011)) {
			t.Fatalf("row for the open hour %s written", r.Hour)
		}
	}
}

// A per-pool backfill starts at the floor (the pool predates it), marks the pool's own coverage up to the shared
// watermark, and keeps the heartbeat cursor at that watermark.
func TestInterestPass_PoolBackfill(t *testing.T) {
	f := newFakeInterestLake()
	f.wm, f.floor = 200_017, 199_990
	f.gaps = []clickhouse.LendingInterestPoolGap{{Pool: liPool, Deployed: 1}}
	f.install(t)
	hb, read := liHeartbeat(t)
	if _, err := runInterest(t, lendingRun{heartbeat: hb, interest: interestFlags{only: true, pool: liPool}}); err != nil {
		t.Fatal(err)
	}
	if want := [][2]uint32{{199_990, 199_996}, {199_997, 200_017}}; !slices.Equal(f.marks, want) || slices.Contains(f.calls, "mark") {
		t.Fatalf("marks %v calls %v, want %v by pool-mark only", f.marks, f.calls, want)
	}
	if c := metric(read(), "stellarindex_ops_job_progress_cursor"); c != "200017" {
		t.Fatalf("cursor %s, want the shared watermark 200017", c)
	}
	if _, err := runInterest(t, lendingRun{interest: interestFlags{only: true, pool: "COTHER"}}); err == nil {
		t.Fatal("a pool outside the registry was backfilled")
	}
	f.gaps = nil
	if out, err := runInterest(t, lendingRun{interest: interestFlags{only: true, pool: liPool}}); err != nil || !strings.Contains(out, "nothing to derive") {
		t.Fatalf("covered pool: %v %q", err, out)
	}
}

// A window over the version bound is halved until it fits; the rows equal the unsplit run's.
func TestInterestPass_VersionBoundHalvesTheWindow(t *testing.T) {
	whole := newFakeInterestLake()
	whole.install(t)
	if _, err := runInterest(t, lendingRun{}); err != nil {
		t.Fatal(err)
	}
	f := newFakeInterestLake()
	f.install(t)
	saved := lendingInterestMaxVersions
	lendingInterestMaxVersions = 2
	t.Cleanup(func() { lendingInterestMaxVersions = saved })
	out, err := runInterest(t, lendingRun{})
	if err != nil || !strings.Contains(out, "halving it") || f.marks[len(f.marks)-1][1] != 200_017 {
		t.Fatalf("%v marks %v log %q", err, f.marks, out)
	}
	gross := map[string]string{}
	for _, r := range f.inserted {
		gross[r.Hour.String()] = r.Gross.String()
	}
	for _, r := range whole.inserted {
		if gross[r.Hour.String()] != r.Gross.String() {
			t.Errorf("hour %s gross %s halved, %s whole", r.Hour, gross[r.Hour.String()], r.Gross)
		}
	}
}

// Ranges the pass cannot honour refuse, reading nothing, instead of exiting 0 with nothing done.
func TestInterestPass_RangeRefusals(t *testing.T) {
	f := newFakeInterestLake()
	f.wm, f.floor = 200_017, 199_990
	f.install(t)
	only := interestFlags{only: true}
	for name, r := range map[string]lendingRun{
		"-from below the floor":      {from: 199_980, interest: only},
		"-from past lending_state":   {from: 200_018, interest: only},
		"-to below -from":            {from: 200_000, to: 199_995, interest: only},
		"-to below the resume point": {to: 199_000, interest: only},
	} {
		if _, err := runInterest(t, r); err == nil || len(f.reads) != 0 {
			t.Errorf("%s: err %v reads %v, want a refusal before any read", name, err, f.reads)
		}
	}
}

func TestInterestFlagsCheck(t *testing.T) {
	for _, c := range []struct {
		name string
		r    lendingRun
		want string
	}{
		{"timer", lendingRun{floor: 1}, ""},
		{"init with a floor", lendingRun{floor: 60_000_000, interest: interestFlags{init: true, only: true}}, ""},
		{"state pass with floor and range", lendingRun{floor: 5, from: 10, to: 20}, ""},
		{"interest-only re-derive", lendingRun{floor: 1, from: 10, to: 20, interest: interestFlags{only: true}}, ""},
		{"interest-only with a floor", lendingRun{floor: 5, interest: interestFlags{only: true}}, "-floor-ledger"},
		{"pool with a floor", lendingRun{floor: 5, interest: interestFlags{pool: "P"}}, "-floor-ledger"},
		{"prove with interest", lendingRun{prove: true, interest: interestFlags{only: true}}, "do not apply"},
		{"prove -interest over a range and pool", lendingRun{prove: true, from: 5, to: 9, floor: 5, interest: interestFlags{prove: true, pool: "P"}}, ""},
		{"-interest without -prove", lendingRun{interest: interestFlags{prove: true}}, "applies to -prove"},
		{"prove -interest with a derive flag", lendingRun{prove: true, interest: interestFlags{prove: true, maxUnordered: 1}}, "the derive's"},
		{"state prove with a pool", lendingRun{prove: true, interest: interestFlags{pool: "P"}}, "lending_state -prove"},
		{"init and pool", lendingRun{interest: interestFlags{init: true, pool: "P"}}, "separate runs"},
		{"init with -from", lendingRun{from: 5, interest: interestFlags{init: true}}, "set their own range"},
		{"negative windows", lendingRun{interest: interestFlags{maxWindows: -1}}, ">= 0"},
	} {
		err := c.r.interest.check(c.r)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

// An upgrade ledger's earlier transactions ran the old code: a window starting there refuses unless both hashes map to
// the same arithmetic.
func TestGateLendingWASM_UpgradeLedger(t *testing.T) {
	other := strings.Repeat("ef", 32)
	pools := []clickhouse.LendingContract{{ID: "P", Protocol: clickhouse.LendingBlendV2, Deployed: 10}}
	code := map[string][]clickhouse.ContractCodeVersion{"P": {{Ledger: 10, WasmHash: other}, {Ledger: 150, WasmHash: liV2}}}
	if _, err := gateLendingWASM(code, pools, 150, 200); err == nil || !strings.Contains(err.Error(), "upgraded from WASM "+other) {
		t.Fatalf("err = %v, want the upgrade-ledger refusal", err)
	}
	lendingInterestWASM[other] = lendinginterest.BlendV2
	t.Cleanup(func() { delete(lendingInterestWASM, other) })
	if hi, err := gateLendingWASM(code, pools, 150, 200); err != nil || hi != 200 {
		t.Fatalf("same-arithmetic upgrade: hi %d err %v, want 200", hi, err)
	}
	if hi, err := gateLendingWASM(code, pools, 151, 200); err != nil || hi != 200 {
		t.Fatal(fmt.Sprint(hi, err))
	}
}
