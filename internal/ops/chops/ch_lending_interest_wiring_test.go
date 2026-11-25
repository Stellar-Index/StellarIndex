package chops

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math/big"
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

// Interest runs hold the lending_state lock shared: a state run holding it exclusive refuses a manual interest run
// (and a dry run) and makes the timer's pass skip quietly, while another reader's shared hold stops neither. A held
// interest lock refuses a manual run and makes the timer's pass skip quietly. A state resume beside a reader skips
// quietly; a replay (-from) beside one, or a resume beside another state run, refuses.
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
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "already locked") || len(f.reads) != 0 {
		t.Fatalf("-interest-only beside a state run: %v reads %v, want a refusal reading nothing", err, f.reads)
	}
	dry := r
	dry.dryRun = true
	if err := dry.run(); err == nil || len(f.reads) != 0 {
		t.Fatalf("dry run beside a state run: %v reads %v, want a refusal", err, f.reads)
	}
	var log bytes.Buffer
	timer := lendingRun{chAddr: "fake", lockFile: r.lockFile, heartbeat: hb}
	if err := timer.lockedInterestPass(context.Background(), &log); err != nil || !strings.Contains(log.String(), "a lending_state run holds") || len(f.reads) != 0 {
		t.Fatalf("timer pass beside a state run: %v %q reads %v", err, log.String(), f.reads)
	}
	state := lendingRun{chAddr: "fake", lockFile: r.lockFile}
	if err := state.statePass(context.Background()); err == nil || !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("state resume beside a state run: %v, want a refusal", err)
	}
	unlock()
	unlockR, err := acquireSharedLock(lendingInterestJob, r.lockFile)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockR()
	if err := r.run(); err != nil || len(f.marks) == 0 {
		t.Fatalf("-interest-only beside another reader: %v marks %v", err, f.marks)
	}
	replay := lendingRun{chAddr: "fake", lockFile: r.lockFile, from: 5}
	if err := replay.statePass(context.Background()); err == nil || !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("state replay beside a reader: %v, want a refusal", err)
	}
	state.heartbeat = hb
	if err := state.statePass(context.Background()); err != nil {
		t.Fatalf("state resume beside a reader: %v, want a quiet skip", err)
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
	log.Reset()
	if err := timer.lockedInterestPass(context.Background(), &log); err != nil || !strings.Contains(log.String(), "another interest run") || len(f.reads) != 0 {
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
	// At 2 an hour's own versions reach the bound: a refusal naming the flag, not an endless halving.
	over := newFakeInterestLake()
	over.install(t)
	if _, err := runInterest(t, lendingRun{interest: interestFlags{maxVersions: 2}}); err == nil ||
		!strings.Contains(err.Error(), "the fold holds an hour whole: raise -interest-max-versions") || len(over.marks) != 0 || len(over.inserted) != 0 {
		t.Fatalf("an hour over the bound: %v marks %v, want a refusal writing nothing", err, over.marks)
	}
	// Raised to 3, the least that fits the carried open hour's two versions, the same lake halves and passes.
	f := newFakeInterestLake()
	f.install(t)
	out, err := runInterest(t, lendingRun{interest: interestFlags{maxVersions: 3}})
	if err != nil || !strings.Contains(out, "holds over 3 versions; halving it") || f.marks[len(f.marks)-1][1] != 200_017 {
		t.Fatalf("%v marks %v log %q", err, f.marks, out)
	}
	if got, want := interestRowsByHour(f.inserted), interestRowsByHour(whole.inserted); !maps.Equal(got, want) {
		t.Fatalf("rows with the raised bound:\n%v\nwant the unbounded run's:\n%v", got, want)
	}
}

// interestRowsByHour renders each written row by its hour and asset; a re-derived hour keeps its last row.
func interestRowsByHour(rows []lendinginterest.HourRow) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		out[r.Hour.String()+"/"+r.Asset] = fmt.Sprintf("%+v", r)
	}
	return out
}

// The open hour's cause events count towards the bound beside its versions: the fold holds them again. Four events
// and a version carried out of [199997,199999] reach a bound of 5, which the versions alone do not.
func TestInterestPass_VersionBoundCountsCarriedEvents(t *testing.T) {
	lake := func() *fakeInterestLake {
		f := newFakeInterestLake()
		for i := range 4 {
			f.events = append(f.events, undecodableFill(t, 199_998, i))
		}
		f.install(t)
		return f
	}
	whole := lake()
	if _, err := runInterest(t, lendingRun{}); err != nil {
		t.Fatal(err)
	}
	over := lake()
	_, err := runInterest(t, lendingRun{interest: interestFlags{maxVersions: 5}})
	if err == nil || !strings.Contains(err.Error(), "carries 5 versions and cause events, over the bound 5") ||
		!strings.Contains(err.Error(), "-interest-max-versions") || len(over.marks) != 1 {
		t.Fatalf("err %v marks %v, want the carried events to reach the bound after the first hour", err, over.marks)
	}
	f := lake()
	if _, err := runInterest(t, lendingRun{interest: interestFlags{maxVersions: 8}}); err != nil {
		t.Fatal(err)
	}
	if got, want := interestRowsByHour(f.inserted), interestRowsByHour(whole.inserted); !maps.Equal(got, want) {
		t.Fatalf("rows with the bound:\n%v\nwant the unbounded run's:\n%v", got, want)
	}
}

// The flag refuses a bound of 0 or less at parse; unset, the pass uses the default.
func TestInterestFlags_MaxVersions(t *testing.T) {
	for _, arg := range []string{"0", "-5", "x"} {
		var f interestFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f.register(fs)
		if err := fs.Parse([]string{"-interest-max-versions", arg}); err == nil {
			t.Errorf("-interest-max-versions %s parsed", arg)
		}
	}
	var f interestFlags
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f.register(fs)
	if err := fs.Parse([]string{"-interest-max-versions", "300000"}); err != nil || f.maxVersions != 300_000 {
		t.Fatalf("parse: %v, %d", err, f.maxVersions)
	}
	if b := (&interestPass{}).versionBound(); b != lendingInterestMaxVersions {
		t.Fatalf("default bound %d", b)
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
		{"prove -interest with a version bound", lendingRun{prove: true, interest: interestFlags{prove: true, maxVersions: 9}}, ""},
		{"state prove with a version bound", lendingRun{prove: true, interest: interestFlags{maxVersions: 9}}, "-interest-max-versions does not apply"},
		{"derive with a version bound", lendingRun{interest: interestFlags{only: true, maxVersions: 9}}, ""},
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

// A pool lending_state never derived (no instance row) refuses before anything is read or written, naming the replay;
// one it derived with no reserve still advances (TestInterestPass_PoolWithoutReserveKeysAdvances).
func TestInterestPass_UnderivedPoolRefuses(t *testing.T) {
	for _, lake := range []bool{true, false} {
		f := newFakeInterestLake()
		f.underived = []clickhouse.LendingInterestUnderived{{Pool: liPool, Deployed: 1, Lake: lake}}
		f.install(t)
		want := map[bool]string{true: "lending_state has not derived pool CPOOL", false: "neither lending_state nor the lake"}[lake]
		_, err := runInterest(t, lendingRun{})
		if err == nil || !strings.Contains(err.Error(), want) || len(f.reads) != 0 || len(f.marks) != 0 || len(f.inserted) != 0 {
			t.Fatalf("lake %t: %v reads %v marks %v, want %q and nothing read or written", lake, err, f.reads, f.marks, want)
		}
	}
}

// A seeded instance whose WASM is not the code the timeline has in force at the run's start refuses.
func TestInterestPass_SeedInstanceCodeCrossCheck(t *testing.T) {
	f := newFakeInterestLake()
	f.seedHash = "1111111111111111111111111111111111111111111111111111111111111111"
	f.install(t)
	_, err := runInterest(t, lendingRun{})
	if err == nil || !strings.Contains(err.Error(), "the seed from ledger 1") || len(f.marks) != 0 || len(f.inserted) != 0 {
		t.Fatalf("err = %v marks %v, want the seed's cross-check refusal and nothing written", err, f.marks)
	}
}

// A key with no seed whose first version is not its creation existed before the run: lending_state lacks its rows.
func TestInterestPass_UnseededKeyMustBeNew(t *testing.T) {
	f := newFakeInterestLake()
	f.versions[0].Created = false
	f.install(t)
	_, err := runInterest(t, lendingRun{})
	if err == nil || !strings.Contains(err.Error(), "not its creation") || len(f.marks) != 0 || len(f.inserted) != 0 {
		t.Fatalf("err = %v marks %v, want the refusal and nothing written", err, f.marks)
	}
}

// A pending redo stops the timer's pass quietly and refuses any other run but a re-derive from at or below its next
// ledger, which advances the redo window by window until it reaches the watermark.
func TestInterestPass_PendingRedo(t *testing.T) {
	f := newFakeInterestLake()
	f.wm, f.floor = 200_017, 199_990
	f.redo = &clickhouse.LendingInterestRedo{Gen: 3, From: 199_995}
	f.install(t)
	out, err := runInterest(t, lendingRun{})
	if err != nil || !strings.Contains(out, "-interest-only -from 199995") || len(f.reads) != 0 {
		t.Fatalf("timer under a redo: %v %q reads %v", err, out, f.reads)
	}
	for _, r := range []lendingRun{{interest: interestFlags{only: true}}, {from: 199_998, interest: interestFlags{only: true}}} {
		if _, err := runInterest(t, r); err == nil || !strings.Contains(err.Error(), "re-derive them first") || len(f.reads) != 0 {
			t.Fatalf("%+v under a redo: %v, want a refusal reading nothing", r, err)
		}
	}
	if _, err := runInterest(t, lendingRun{from: 199_995, interest: interestFlags{only: true}}); err != nil {
		t.Fatal(err)
	}
	if want := [][2]uint32{{3, 199_996}, {3, 200_017}}; !slices.Equal(f.redoMarks, want) || f.redo != nil {
		t.Fatalf("redo marks %v pending %v, want %v and the redo done", f.redoMarks, f.redo, want)
	}
}

// A redo armed below the floor (a lending_state replay from before the interest rows start) is redone from the floor:
// the timer names it, a re-derive from there is let through and clears the redo, and the heartbeat shows the hold.
func TestInterestPass_RedoBelowFloor(t *testing.T) {
	f := newFakeInterestLake()
	f.wm, f.floor = 200_017, 199_990
	f.redo = &clickhouse.LendingInterestRedo{Gen: 4, From: 199_000}
	f.install(t)
	hb, read := liHeartbeat(t)
	out, err := runInterest(t, lendingRun{heartbeat: hb})
	if err != nil || !strings.Contains(out, "-interest-only -from 199990") || len(f.reads) != 0 {
		t.Fatalf("timer under a redo below the floor: %v %q reads %v", err, out, f.reads)
	}
	if text := read(); metric(text, lendingInterestRedoPendingMetric) != "1" || metric(text, "stellarindex_ops_job_last_exit_ok") != "1" {
		t.Fatalf("heartbeat:\n%s\nwant the redo held the run, which exited cleanly", text)
	}
	if _, err := runInterest(t, lendingRun{heartbeat: hb, from: 199_990, interest: interestFlags{only: true}}); err != nil {
		t.Fatalf("re-derive from the floor: %v", err)
	}
	if n := len(f.redoMarks); f.redo != nil || n == 0 || f.redoMarks[n-1] != [2]uint32{4, 200_017} {
		t.Fatalf("redo marks %v pending %v, want the redo done through 200017", f.redoMarks, f.redo)
	}
	if got := metric(read(), lendingInterestRedoPendingMetric); got != "0" {
		t.Fatalf("redo_pending after the re-derive = %q, want 0", got)
	}
}

// A new key created in one transaction and updated in another of the same ledger, on rows without intra-ledger order,
// passes whichever sorts first; with apply order, or with no creation in the ledger, an update first refuses.
func TestCheckNewKeys_FirstLedger(t *testing.T) {
	created, updated := liReserve(10, 1), liReserve(10, 2)
	created.Created, created.IntraLedgerSeq, created.TxHash = true, 0, "b"
	updated.IntraLedgerSeq, updated.TxHash = 0, "a"
	later := liReserve(11, 3)
	for _, vs := range [][]lendinginterest.Version{{created, updated, later}, {updated, created, later}} {
		st := interestCarry{known: map[string]bool{}}
		if err := st.checkNewKeys(vs); err != nil {
			t.Fatalf("unordered first ledger %s,%s: %v", vs[0].TxHash, vs[1].TxHash, err)
		}
	}
	ordered := []lendinginterest.Version{updated, created}
	ordered[0].IntraLedgerSeq, ordered[1].IntraLedgerSeq = 1, 2
	other := updated
	other.TxHash = "c"
	for _, vs := range [][]lendinginterest.Version{ordered, {updated, other}, {updated, later}} {
		st := interestCarry{known: map[string]bool{}}
		if err := st.checkNewKeys(vs); err == nil || !strings.Contains(err.Error(), "not its creation") {
			t.Fatalf("%+v: %v, want the refusal", vs, err)
		}
	}
}

// An excluded pair cuts the telescoped chain; the pairs around it are still recomputed, so a wrong gross fails.
func TestCheckGross_ExcludedPairKeepsTheRestChecked(t *testing.T) {
	vs := []lendinginterest.Version{liReserve(8, 1), liReserve(9, 2), liReserve(10, 3), liReserve(11, 4)}
	prev := liReserve(7, 0).Reserve
	head, err := lendinginterest.TelescopedGross(&prev, vs[:2])
	if err != nil {
		t.Fatal(err)
	}
	tail, err := lendinginterest.TelescopedGross(nil, vs[2:])
	if err != nil {
		t.Fatal(err)
	}
	whole, _ := lendinginterest.TelescopedGross(&prev, vs)
	gross := new(big.Int).Add(head, tail)
	if gross.Cmp(whole) == 0 {
		t.Fatal("degenerate fixture: the excluded pair moved nothing")
	}
	r := lendinginterest.HourRow{Gross: gross, AccrualErrors: 1, Excluded: []lendinginterest.Version{vs[2]}}
	var c interestChecked
	if msg := checkGross(r, &prev, vs, &c); msg != "" || c.telescoped != 1 || c.telescopeSplit != 1 || c.excludedPairs != 1 {
		t.Fatalf("matching gross: %q %+v", msg, c)
	}
	r.Gross = new(big.Int).Add(gross, big.NewInt(1))
	if msg := checkGross(r, &prev, vs, &c); !strings.Contains(msg, "telescope to") {
		t.Fatalf("gross off by one on a row with an excluded pair: %q, want a failure", msg)
	}
	r.Gross, r.Excluded = gross, nil
	if msg := checkGross(r, &prev, vs, &c); !strings.Contains(msg, "1 excluded pairs counted, 0 listed") {
		t.Fatalf("unlisted exclusion: %q", msg)
	}
}
