// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package opsutil_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// gaugeValue pulls one metric's sample value out of a textfile body,
// erroring the test if the series is absent — absence and zero are
// DIFFERENT states for these gauges and the tests must not conflate them
// (that conflation is the C4-038 defect one directory over).
func gaugeValue(t *testing.T, body, metric string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, metric+"{") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				t.Fatalf("malformed sample line %q", line)
			}
			return fields[1]
		}
	}
	t.Fatalf("metric %q is ABSENT from the textfile:\n%s", metric, body)
	return ""
}

func hasSeries(body, metric string) bool {
	return strings.Contains(body, "\n"+metric+"{") || strings.HasPrefix(body, metric+"{")
}

// TestJobHeartbeatPublishesRunningAndProgress is the C6-020 regression:
// a long backfill must publish enough state for a monitor to tell
// "working" from "hung" from "died". Before the fix NOTHING was published
// at all — no textfile, no series, no alert in either rule tree.
func TestJobHeartbeatPublishesRunningAndProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ops_job_ch_backfill.prom")

	fixed := time.Unix(1_700_000_000, 0)
	hb := opsutil.NewJobHeartbeat("ch-backfill", path, func() time.Time { return fixed })
	if !hb.Enabled() {
		t.Fatal("explicit path must enable the heartbeat")
	}
	hb.Start()
	hb.Progress(1234, 63_050_000)
	hb.Stop(true)

	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("heartbeat textfile not written: %v", err)
	}
	body := string(raw)

	if got := gaugeValue(t, body, "stellarindex_ops_job_progress_total"); got != "1234" {
		t.Errorf("progress_total = %s, want 1234", got)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_progress_cursor"); got != "63050000" {
		t.Errorf("progress_cursor = %s, want 63050000", got)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_heartbeat_unix"); got != "1700000000" {
		t.Errorf("heartbeat_unix = %s, want 1700000000", got)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_started_unix"); got != "1700000000" {
		t.Errorf("started_unix = %s, want 1700000000", got)
	}
	// Post-Stop the job is NOT running and the exit is recorded clean.
	if got := gaugeValue(t, body, "stellarindex_ops_job_running"); got != "0" {
		t.Errorf("running after Stop = %s, want 0", got)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_last_exit_ok"); got != "1" {
		t.Errorf("last_exit_ok = %s, want 1", got)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_last_finish_unix"); got != "1700000000" {
		t.Errorf("last_finish_unix = %s, want 1700000000", got)
	}
	// The label MUST be `ops_job`, never the reserved `job`. node_exporter
	// is scraped with honor_labels unset, so an emitted `job=` is renamed to
	// `exported_job` and overwritten with the scrape's own job name — which
	// collapses every ops job on the host into one match group for the
	// alerts' `and on (...)` join and renders "node_exporter" in the pager.
	if !strings.Contains(body, `stellarindex_ops_job_running{ops_job="ch-backfill"}`) {
		t.Errorf("series must carry ops_job=:\n%s", body)
	}
	if strings.Contains(body, `job="ch-backfill"`) && !strings.Contains(body, `ops_job="ch-backfill"`) {
		t.Errorf("series must NOT use the reserved `job` label:\n%s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "stellarindex_ops_job_") && strings.Contains(line, "{job=") {
			t.Errorf("reserved `job` label on %q — it will be renamed to exported_job at scrape time", line)
		}
	}
}

// TestJobHeartbeatRunningWhileInFlight pins the state that the stall
// alerts actually key on: while the job is executing, running MUST be 1
// and the terminal series MUST be absent (not 0 — a 0
// last_finish_unix reads as 1970 to every `time() - x` expression, which
// is how a "finished long ago" alert fires on a job that never finished).
func TestJobHeartbeatRunningWhileInFlight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "j.prom")
	fixed := time.Unix(1_700_000_000, 0)
	hb := opsutil.NewJobHeartbeat("backfill", path, func() time.Time { return fixed })
	hb.Start()
	defer hb.Stop(false)

	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("Start must write immediately: %v", err)
	}
	body := string(raw)
	if got := gaugeValue(t, body, "stellarindex_ops_job_running"); got != "1" {
		t.Errorf("running during the run = %s, want 1", got)
	}
	if hasSeries(body, "stellarindex_ops_job_last_finish_unix") {
		t.Errorf("last_finish_unix must be ABSENT mid-run, not 0:\n%s", body)
	}
	if hasSeries(body, "stellarindex_ops_job_last_exit_ok") {
		t.Errorf("last_exit_ok must be ABSENT mid-run, not 0:\n%s", body)
	}
}

// TestJobHeartbeatRecordsFailedExit — a run that ended BADLY must be
// distinguishable from one that ended cleanly and from one still running.
func TestJobHeartbeatRecordsFailedExit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "j.prom")
	hb := opsutil.NewJobHeartbeat("ch-backfill", path, nil)
	hb.Start()
	hb.Stop(false)

	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(raw)
	if got := gaugeValue(t, body, "stellarindex_ops_job_running"); got != "0" {
		t.Errorf("running = %s, want 0", got)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_last_exit_ok"); got != "0" {
		t.Errorf("last_exit_ok = %s, want 0 for a failed run", got)
	}
}

// TestJobHeartbeatCursorIsMonotoneMax — ch-backfill runs N parallel
// walkers over DISJOINT chunks, so consecutive Progress calls arrive from
// different parts of the range. A last-write cursor would oscillate and
// make the published "how far have we got" meaningless; it must be a
// running maximum.
func TestJobHeartbeatCursorIsMonotoneMax(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "j.prom")
	hb := opsutil.NewJobHeartbeat("ch-backfill", path, nil)
	hb.Start()
	hb.Progress(1, 63_100_000) // worker on the high chunk
	hb.Progress(2, 10_000_000) // worker on the low chunk
	hb.Progress(3, 20_000_000)
	hb.Stop(true)

	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(raw)
	if got := gaugeValue(t, body, "stellarindex_ops_job_progress_cursor"); got != "63100000" {
		t.Errorf("progress_cursor = %s, want the MAX 63100000 (a last-write cursor would report 20000000)", got)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_progress_total"); got != "3" {
		t.Errorf("progress_total = %s, want 3", got)
	}
}

// TestJobHeartbeatInertWithoutTextfileDir — on a laptop / in CI there is
// no node_exporter textfile directory, and the heartbeat must be entirely
// silent: no file anywhere, no error, no stray dot-file in the working
// tree. This is what lets ch-backfill default the flag instead of forcing
// every call site (and every operator's muscle memory) to pass it.
func TestJobHeartbeatInertWithoutTextfileDir(t *testing.T) {
	if _, err := os.Stat(opsutil.DefaultTextfileDir); err == nil {
		t.Skipf("%s exists on this machine — the inert path cannot be exercised here", opsutil.DefaultTextfileDir)
	}
	hb := opsutil.NewJobHeartbeat("ch-backfill", "", nil)
	if hb.Enabled() {
		t.Fatalf("heartbeat must be inert without %s, got path %q", opsutil.DefaultTextfileDir, hb.Path())
	}
	// Every method must be a safe no-op on an inert heartbeat.
	hb.Start()
	hb.Progress(1, 2)
	hb.Stop(true)
	if hb.Path() != "" {
		t.Errorf("inert heartbeat Path() = %q, want empty", hb.Path())
	}
}

// TestJobHeartbeatTickerKeepsLivenessFreshWithoutProgress is the
// load-bearing half of the two-alert design: the heartbeat timestamp must
// advance on a TIMER, independent of whether any work happened. If it only
// advanced on Progress, a hung job would look identical to a dead one and
// the two alerts (`ops_job_heartbeat_stale` = process gone,
// `ops_job_no_progress` = process hung) would collapse into one.
func TestJobHeartbeatTickerKeepsLivenessFreshWithoutProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "j.prom")

	// Clock advances 1s per read so a second render is provably later,
	// without waiting for the 60s production ticker.
	tick := int64(1_700_000_000)
	hb := opsutil.NewJobHeartbeat("ch-backfill", path, func() time.Time {
		tick++
		return time.Unix(tick, 0)
	})
	hb.Start()
	first := readGauge(t, path, "stellarindex_ops_job_heartbeat_unix")
	// No Progress call at all — the job is hung.
	hb.Stop(true)
	second := readGauge(t, path, "stellarindex_ops_job_heartbeat_unix")

	if first == second {
		t.Errorf("heartbeat_unix did not advance without progress (%s == %s) — a hung job would be indistinguishable from a dead one", first, second)
	}
	body := readAll(t, path)
	if got := gaugeValue(t, body, "stellarindex_ops_job_progress_total"); got != "0" {
		t.Errorf("progress_total = %s, want 0 (no work was reported)", got)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func readGauge(t *testing.T, path, metric string) string {
	t.Helper()
	return gaugeValue(t, readAll(t, path), metric)
}

// TestJobHeartbeatUsesNonReservedLabel is the C6-020 verification fix.
//
// `job` is a Prometheus RESERVED label. Both configs that collect this
// textfile scrape it through node_exporter with honor_labels unset
// (configs/prometheus/prometheus.r1.yml:41,
// configs/ansible/roles/prometheus/templates/prometheus.yml.j2:116), so an
// exposed `job="ch-backfill"` arrives as
// `job="node_exporter", exported_job="ch-backfill"`.
//
// The consequence was a live false-alert: the alerts join `and on (job)`,
// which after substitution matched EVERY ops job on the host as one group,
// so a cleanly-finished `backfill` was rescued by a healthy running
// `ch-backfill` and ticketed permanently. Two .prom files per host is the
// steady state — the heartbeat is wired into both subcommands.
//
// This test pins the emitter half; the join half is pinned by
// deploy/monitoring/rule-tests/ops-job_test.yml, whose fixtures now carry
// POST-scrape label shapes.
func TestJobHeartbeatUsesNonReservedLabel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "j.prom")
	hb := opsutil.NewJobHeartbeat("ch-backfill", path, nil)
	hb.Start()
	hb.Progress(5, 100)
	hb.Stop(true)

	body := readAll(t, path)
	var sampleLines int
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "stellarindex_ops_job_") {
			continue
		}
		sampleLines++
		if !strings.Contains(line, `ops_job="ch-backfill"`) {
			t.Errorf("sample missing ops_job label: %q", line)
		}
		if strings.Contains(line, `{job=`) || strings.Contains(line, `,job=`) {
			t.Errorf("sample uses the RESERVED `job` label (scrape will rename it to exported_job and substitute job=node_exporter): %q", line)
		}
	}
	if sampleLines == 0 {
		t.Fatalf("no samples emitted:\n%s", body)
	}
}

// TestJobHeartbeatConcurrentRunsDoNotShareOnePath — two concurrent runs of
// the same subcommand resolve to the same default path. Sharing it is a
// monitoring hole: they interleave whole-file rewrites, and whichever exits
// first writes running=0, silencing BOTH stall alerts for the survivor,
// which may then wedge for hours unobserved. Overlapping backfills are an
// ordinary operator action here.
//
// The second run must take a distinct path AND a distinct label set — two
// files in one collector directory exposing identical labels would be a
// duplicate sample, and node_exporter fails the WHOLE scrape on that.
func TestJobHeartbeatConcurrentRunsDoNotShareOnePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ops_job_ch_backfill.prom")

	first := opsutil.NewJobHeartbeat("ch-backfill", path, nil)
	first.Start()
	defer first.Stop(true)

	second := opsutil.NewJobHeartbeat("ch-backfill", path, nil)
	if second.Path() == first.Path() {
		t.Fatalf("both concurrent runs resolved to %q — the first to exit writes running=0 and silences the survivor's stall alerts", first.Path())
	}
	second.Start()
	second.Progress(7, 42)
	defer second.Stop(true)

	// The survivor's file must still say running=1 after the other run
	// writes its terminal state.
	firstBody := readAll(t, first.Path())
	if got := gaugeValue(t, firstBody, "stellarindex_ops_job_running"); got != "1" {
		t.Errorf("first run's running = %s, want 1", got)
	}
	// And the two must not expose identical label sets.
	secondBody := readAll(t, second.Path())
	if !strings.Contains(secondBody, "pid=") {
		t.Errorf("the fallback file must carry a distinguishing pid label, or node_exporter sees a duplicate sample and drops the whole scrape:\n%s", secondBody)
	}
	if strings.Contains(firstBody, "pid=") {
		t.Errorf("the primary run must keep a label-stable series (no pid):\n%s", firstBody)
	}
}

// TestJobHeartbeatSweepsDeadPIDSiblings — the `.pid<N>.prom` fallback files
// are per-invocation, so without a reaper every contention over the life of
// the host leaves one behind, each pinning a distinct `pid` label value:
// unbounded cardinality on a metric whose whole point is to be cheap.
//
// The sweep runs on CONTENTION (when a new sibling is about to be created),
// so the surviving set is bounded by the number of live runs. A sibling
// whose process is gone is removed; one whose process is alive is not,
// because removing it would blind a running job's monitoring.
func TestJobHeartbeatSweepsDeadPIDSiblings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ops_job_ch_backfill.prom")

	// A sibling from a run that is long gone. PID 1 is always alive on unix,
	// so use it for the must-survive case and a definitely-dead pid for the
	// must-be-reaped case.
	deadPID := findDeadPID(t)
	dead := filepath.Join(dir, fmt.Sprintf("ops_job_ch_backfill.pid%d.prom", deadPID))
	alive := filepath.Join(dir, "ops_job_ch_backfill.pid1.prom")
	// A file that is NOT one of ours must never be touched.
	bystander := filepath.Join(dir, "supply_snapshot.prom")
	for _, f := range []string{dead, alive, bystander} {
		if err := os.WriteFile(f, []byte("# stale\n"), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write %s: %v", f, err)
		}
	}

	// Hold the primary so the next constructor hits contention and sweeps.
	first := opsutil.NewJobHeartbeat("ch-backfill", path, nil)
	first.Start()
	defer first.Stop(true)

	second := opsutil.NewJobHeartbeat("ch-backfill", path, nil)
	second.Start()
	defer second.Stop(true)

	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("sibling for dead pid %d was not reaped (err=%v) — these accumulate one per contention forever", deadPID, err)
	}
	if _, err := os.Stat(alive); err != nil {
		t.Errorf("sibling for a LIVE pid was removed (%v) — that blinds a running job's monitoring", err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("an unrelated collector file was removed (%v)", err)
	}
	// And the contended run still got its own live file.
	if second.Path() == first.Path() {
		t.Fatalf("both runs resolved to %q", first.Path())
	}
	if _, err := os.Stat(second.Path()); err != nil {
		t.Errorf("the contended run's own file is missing: %v", err)
	}
}

// findDeadPID returns a pid that is not currently addressable, so the sweep
// test does not depend on a hardcoded guess being free on the runner.
func findDeadPID(t *testing.T) int {
	t.Helper()
	for pid := 999_000; pid < 999_500; pid++ {
		p, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if serr := p.Signal(syscall.Signal(0)); serr != nil && !errors.Is(serr, os.ErrPermission) {
			return pid
		}
	}
	t.Skip("no dead pid found in the probe range on this machine")
	return 0
}
