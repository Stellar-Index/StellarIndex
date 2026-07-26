// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package opsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultTextfileDir is the node_exporter textfile-collector directory
// the archival-node role provisions and scrapes
// (configs/ansible/roles/archival-node/tasks/10-observability.yml:34 —
// `--collector.textfile.directory=/var/lib/node_exporter/textfile_collector`).
//
// A long-running ops job defaults its heartbeat here rather than taking a
// mandatory flag, because the failure this closes (C6-020) is precisely
// that nobody remembers to add the flag: long backfills are the dominant
// ops activity right now (Phase A recompress, movement backfills) and a
// HUNG one was invisible in both rule trees. Defaulting means an operator
// who types the same `ch-backfill` command they always typed gets the
// heartbeat for free on r1, and gets nothing (silently) on a laptop where
// the directory does not exist — see [NewJobHeartbeat].
const DefaultTextfileDir = "/var/lib/node_exporter/textfile_collector"

// jobHeartbeatInterval is how often the background ticker rewrites the
// textfile while a job runs. It must be well under the alert's staleness
// window (deploy/monitoring/rules/ingestion.yml,
// stellarindex_ops_job_heartbeat_stale, 15 min) so an ordinary scrape gap
// or a slow write never reads as a dead process.
const jobHeartbeatInterval = 60 * time.Second

// jobHeartbeatLabel is the label name every series here carries.
//
// It is `ops_job`, NOT `job`, and that is not cosmetic. `job` is a
// Prometheus RESERVED label: a scrape sets it from `job_name`, and with
// `honor_labels` at its default `false` any `job` a target exposes is
// RENAMED to `exported_job` and overwritten. Both configs that collect this
// textfile scrape it through node_exporter with honor_labels unset
// (configs/prometheus/prometheus.r1.yml:41 and
// configs/ansible/roles/prometheus/templates/prometheus.yml.j2:116), so an
// emitted `job="ch-backfill"` would arrive as
// `job="node_exporter", exported_job="ch-backfill"`.
//
// The consequence was not cosmetic either: `and on (job)` would then match
// EVERY ops job on the host as one group, so a cleanly-finished `backfill`
// (running=0, heartbeat frozen) joined against a healthy running
// `ch-backfill` and produced a permanent false ticket — and `{{ $labels.job }}`
// rendered "node_exporter". Two .prom files on one host is the steady state,
// since the heartbeat is wired into both subcommands.
//
// Every other textfile emitter in this repo already avoids the reserved
// name (`source=`, `domain=`, `assertion=`, `stanza=`); this one now does
// too. Changing it is a WIRE-FORMAT change: the alert exprs in both rule
// trees join `on (ops_job, instance, pid)` and must move with it.
const jobHeartbeatLabel = "ops_job"

// JobHeartbeat publishes liveness + progress for one long-running
// stellarindex-ops job as node_exporter textfile gauges.
//
// The gap it closes (C6-020, audit-2026-07-23): NO backfill-progress
// alert existed in either rule tree. A backfill that wedged — a stalled S3
// read, a ClickHouse connection that never returns, an OOM-killed worker —
// looked exactly like a backfill that was still working, for as long as
// nobody happened to tail the journal. The two states are distinguished
// here by publishing them SEPARATELY:
//
//   - stellarindex_ops_job_running       1 while the process is alive
//   - stellarindex_ops_job_heartbeat_unix rewritten every minute by a
//     background ticker, INDEPENDENT of whether work is happening
//   - stellarindex_ops_job_progress_total units completed so far
//   - stellarindex_ops_job_progress_cursor highest ledger reached
//
// all labelled `ops_job=` — see [jobHeartbeatLabel] for why NOT `job=`.
//
// so `running==1 ∧ stale heartbeat` is "the process died hard without
// cleanup" (SIGKILL, OOM, host reboot) while `running==1 ∧ fresh
// heartbeat ∧ flat progress` is "the process is alive and hung". Those
// need different operator responses, which is why they are different
// alerts and not one.
//
// ONE RUN PER TEXTFILE. Two concurrent invocations of the same subcommand
// resolving to the same default path would trample each other: whichever
// exits first writes running=0 and silences the survivor's stall alerts
// for the rest of its run. [NewJobHeartbeat] therefore takes an exclusive
// flock on the resolved path and, when it is already held, falls back to a
// per-invocation `.pid<N>` path so BOTH runs stay observable rather than
// one silently disabling the other. Operators do run overlapping windows
// (`ch-full-backfill.sh` alongside a hand-driven catch-up), so this is a
// real state, not a theoretical one.
//
// The fallback file carries an extra `pid` label, and the alerts join
// `on (ops_job, instance, pid)` because of it: two files that differ only
// by a label the join ignores would cross-match, and a cleanly-finished
// primary would be "rescued" by its still-running sibling into a permanent
// false ticket. Dead siblings are reaped on the next contention — see
// [sweepStalePIDFiles], including the residual it does not cover.
//
// Deliberately FAIL-SOFT: every write error is swallowed. A heartbeat is
// observability for a job whose actual work is re-deriving the lake;
// aborting a multi-day backfill because a metrics file could not be
// written would be strictly worse than losing the metric. A write that
// never succeeds surfaces as a stale/absent heartbeat, which is exactly
// what the alert already covers.
//
// Zero value is not usable — construct with [NewJobHeartbeat].
type JobHeartbeat struct {
	job  string
	path string
	now  func() time.Time

	mu       sync.Mutex
	started  time.Time
	total    uint64
	cursor   uint64
	running  bool
	finished bool
	exitOK   bool

	lock *os.File // held for the process lifetime; see claimPath
	stop chan struct{}
	done chan struct{}
}

// NewJobHeartbeat builds a heartbeat for job `job` writing to `path`.
//
// path resolution, in order:
//   - an explicit non-empty path is used as given (operator override, and
//     what the tests use);
//   - an empty path resolves to <DefaultTextfileDir>/ops_job_<job>.prom,
//     but ONLY if that directory already exists.
//
// When neither applies the returned heartbeat is INERT: every method is a
// no-op. That is the laptop / CI case, and it is silent on purpose — a
// developer running `ch-backfill` locally should not see a warning about
// a node_exporter directory they will never have, and must not have a
// dot-file appear in their working tree either.
//
// The resolved path is then CLAIMED (see [claimPath]) so two concurrent
// runs of the same subcommand cannot share one file.
func NewJobHeartbeat(job, path string, now func() time.Time) *JobHeartbeat {
	if now == nil {
		now = time.Now
	}
	if path == "" {
		if st, err := os.Stat(DefaultTextfileDir); err == nil && st.IsDir() {
			path = filepath.Join(DefaultTextfileDir, "ops_job_"+sanitizeJobName(job)+".prom")
		}
	}
	hb := &JobHeartbeat{job: job, now: now}
	if path != "" {
		hb.path, hb.lock = claimPath(path)
	}
	return hb
}

// claimPath takes an exclusive, non-blocking flock on `<path>.lock` and
// returns the path to use plus the held lock file.
//
// A second concurrent run of the same subcommand resolves to the SAME
// default path. Sharing it is not a benign race: the two processes
// interleave whole-file rewrites, and whichever finishes first writes
// running=0 — silencing both stall alerts for the survivor, which may then
// wedge for hours completely unobserved. That is a monitoring hole
// disguised as a monitoring feature, and overlapping backfills are an
// ordinary operator action here (`ch-full-backfill.sh` next to a
// hand-driven catch-up window).
//
// The loser of the race does NOT go silent and does NOT fail: it falls
// back to `<path>.pid<N>.prom`, which node_exporter picks up from the same
// directory and which carries the same `ops_job` label, so both runs
// remain observable and both are covered by the same alerts. Emitting two
// series with identical labels would be worse than either — Prometheus
// would report a duplicate-sample scrape error and drop the WHOLE
// node_exporter scrape — so the fallback file additionally carries a
// distinguishing `pid` label (see render). The alert exprs in both rule
// trees join `on (ops_job, instance, pid)` for the same reason: without
// pid in the key the primary and a .pid sibling cross-match, and a
// cleanly-finished primary gets "rescued" by a still-running loser.
//
// pid is emitted ONLY on the fallback file, never unconditionally: the
// primary is the steady-state series and giving it a value that changes
// every single run would churn its identity for nothing (and PromQL
// matches the absent label as "", so the join works either way).
//
// Before falling back, sweep dead siblings — see [sweepStalePIDFiles].
//
// Fail-soft, like every other write on this path: if the lock cannot be
// created at all (read-only dir, permissions), the original path is used
// unlocked. That restores exactly the pre-guard behaviour rather than
// disabling the heartbeat.
func claimPath(path string) (string, *os.File) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644) //nolint:gosec // operator-supplied metrics path, same trust as the .prom itself
	if err != nil {
		return path, nil
	}
	if lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lerr == nil {
		return path, f
	}
	_ = f.Close()
	// Already claimed by a live run — take a private path instead, first
	// clearing out any sibling left behind by a run that is now dead.
	sweepStalePIDFiles(path)
	return pidPath(path, os.Getpid()), nil
}

// pidPath is the fallback textfile for a run that lost the flock.
func pidPath(path string, pid int) string {
	return strings.TrimSuffix(path, ".prom") + fmt.Sprintf(".pid%d.prom", pid)
}

// sweepStalePIDFiles deletes `<path>.pid<N>.prom` siblings whose process N
// is no longer alive.
//
// Without it the fallback files accumulate forever — every contention over
// the lifetime of the host leaves one, each pinning a distinct `pid` label
// value in Prometheus. That is unbounded label cardinality on a metric
// whose whole purpose is to be cheap, growing monotonically over months.
//
// Liveness is `kill(pid, 0)`: it signals nothing and reports whether the
// pid is addressable. EPERM counts as ALIVE (the process exists, it just
// belongs to another user) — only a definitively-absent process has its
// file removed, so a shared-host misidentification errs toward keeping a
// file rather than blinding a live run's monitoring.
//
// Runs on contention only, which is exactly when a new file is about to be
// created, so the set is bounded by the number of concurrent runs. It is
// deliberately NOT run at every startup: the common case is a single run
// with no contention, and scanning the collector directory on every
// `ch-backfill` invocation would be work for nothing.
//
// RESIDUAL, stated so it is not silently assumed away: a loser that is
// never followed by another contention leaves its file behind — the sweep
// is triggered by the next contention, not by a timer. That is one stale
// file per job name in the worst case, and its `running=0` /
// `last_exit_ok` terminal state is honest (a real run that really ended),
// so it neither alerts nor lies. A pid reused by an unrelated process
// keeps the file one cycle longer.
//
// Fail-soft throughout: a directory that cannot be read, or a file that
// cannot be removed, is skipped.
func sweepStalePIDFiles(path string) {
	dir := filepath.Dir(path)
	prefix := filepath.Base(strings.TrimSuffix(path, ".prom")) + ".pid"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".prom") {
			continue
		}
		pid, perr := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".prom"))
		if perr != nil || pid <= 0 || processAlive(pid) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// processAlive reports whether pid is addressable. Signal 0 performs the
// permission + existence checks without delivering anything. A permission
// error means the process EXISTS under another uid, which is alive for our
// purposes; any other error (ESRCH) means it is gone.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid) // never fails on unix
	if err != nil {
		return true // unknown — keep the file
	}
	sigErr := p.Signal(syscall.Signal(0))
	if sigErr == nil {
		return true
	}
	return errors.Is(sigErr, os.ErrPermission) || errors.Is(sigErr, syscall.EPERM)
}

// Enabled reports whether this heartbeat will write anything. Callers use
// it only to decide whether to mention the path in their startup banner.
func (h *JobHeartbeat) Enabled() bool { return h != nil && h.path != "" }

// Path is the textfile this heartbeat writes ("" when inert).
func (h *JobHeartbeat) Path() string {
	if h == nil {
		return ""
	}
	return h.path
}

// Start stamps running=1 and launches the background ticker that keeps
// stellarindex_ops_job_heartbeat_unix fresh even when no progress is being
// made. Call Stop (deferred) exactly once.
func (h *JobHeartbeat) Start() {
	if !h.Enabled() {
		return
	}
	h.mu.Lock()
	h.started = h.now()
	h.running = true
	h.finished = false
	h.stop = make(chan struct{})
	h.done = make(chan struct{})
	stop := h.stop
	done := h.done
	h.mu.Unlock()

	h.write()

	go func() {
		defer close(done)
		t := time.NewTicker(jobHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				h.write()
			}
		}
	}()
}

// Progress records that `total` units (ledgers, rows, …) are complete and
// that the walk has reached ledger `cursor`.
//
// cursor is kept as a MAXIMUM, not a last-write: ch-backfill runs N
// parallel range-walkers over disjoint chunks, so consecutive callbacks
// arrive from different chunks and a last-write cursor would oscillate
// wildly and make `changes()`-based staleness meaningless. total is
// monotone by construction (the caller's own counter).
//
// Cheap enough to call per ledger: it takes a mutex and updates two
// integers. The file itself is only rewritten by the ticker, so a
// 3000-ledger/s walk does not do 3000 renames/s.
func (h *JobHeartbeat) Progress(total, cursor uint64) {
	if !h.Enabled() {
		return
	}
	h.mu.Lock()
	h.total = total
	if cursor > h.cursor {
		h.cursor = cursor
	}
	h.mu.Unlock()
}

// Stop halts the ticker and writes the terminal state: running=0 plus
// last_finish_unix / last_exit_ok, so a completed job stops matching the
// stall alerts and an operator can still see when it ended and whether it
// ended cleanly. exitOK=false is the "it finished, badly" state — distinct
// from both "still running" and "never ran".
//
// Safe to call on an inert heartbeat and safe to call twice.
func (h *JobHeartbeat) Stop(exitOK bool) {
	if !h.Enabled() {
		return
	}
	h.mu.Lock()
	if h.stop != nil {
		close(h.stop)
		h.stop = nil
	}
	done := h.done
	h.done = nil
	h.running = false
	h.finished = true
	h.exitOK = exitOK
	h.mu.Unlock()

	if done != nil {
		<-done
	}
	h.write()

	// Release the claim only after the terminal state is on disk, so a
	// follow-on run cannot adopt the path and overwrite running=0 with its
	// own state before this run's outcome was ever scraped.
	h.mu.Lock()
	lock := h.lock
	h.lock = nil
	h.mu.Unlock()
	if lock != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}
}

// render produces the textfile body for the heartbeat's current state.
// Split out from write so the format is unit-testable without a
// filesystem.
func (h *JobHeartbeat) render() string {
	h.mu.Lock()
	job, started, total, cursor := h.job, h.started, h.total, h.cursor
	running, finished, exitOK := h.running, h.finished, h.exitOK
	h.mu.Unlock()

	var b strings.Builder
	// `ops_job`, never the reserved `job` — see [jobHeartbeatLabel].
	//
	// A run that lost the flock race writes a `.pidN.prom` sibling in the
	// same collector directory. node_exporter concatenates every file in
	// that directory into ONE exposition, so two runs of the same
	// subcommand emitting identical label sets would be a duplicate sample
	// and node_exporter fails the WHOLE scrape on that (not just the
	// series). The `pid` label keeps the two apart. The primary run omits
	// it, so the steady-state series stays label-stable.
	label := fmt.Sprintf("{%s=%q}", jobHeartbeatLabel, job)
	if h.lock == nil && strings.Contains(h.path, ".pid") {
		label = fmt.Sprintf("{%s=%q,pid=%q}", jobHeartbeatLabel, job, strconv.Itoa(os.Getpid()))
	}
	writeGauge := func(name, help string, value string) {
		fmt.Fprintf(&b, "# HELP %s %s\n", name, help)
		fmt.Fprintf(&b, "# TYPE %s gauge\n", name)
		fmt.Fprintf(&b, "%s%s %s\n", name, label, value)
	}
	one := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	writeGauge("stellarindex_ops_job_running",
		"1 while a long-running stellarindex-ops job is executing, 0 once it has exited.",
		one(running))
	writeGauge("stellarindex_ops_job_heartbeat_unix",
		"Unix time of the most recent liveness write by a running stellarindex-ops job. Rewritten on a timer INDEPENDENT of progress, so a stale value means the process is gone, not idle.",
		fmt.Sprintf("%d", h.now().Unix()))
	writeGauge("stellarindex_ops_job_started_unix",
		"Unix time the current (or most recent) stellarindex-ops job run started.",
		fmt.Sprintf("%d", started.Unix()))
	writeGauge("stellarindex_ops_job_progress_total",
		"Units of work (ledgers, rows) a stellarindex-ops job has completed in the current run. Flat while running = hung.",
		fmt.Sprintf("%d", total))
	writeGauge("stellarindex_ops_job_progress_cursor",
		"Highest ledger sequence a stellarindex-ops job has reached in the current run.",
		fmt.Sprintf("%d", cursor))
	// last_finish/last_exit are only meaningful once a run has ended.
	// Emitting them mid-run (as 0, or as the PREVIOUS run's values) would
	// be worse than absent: a 0 finish timestamp reads as 1970 to every
	// `time() - x` expression.
	if finished {
		writeGauge("stellarindex_ops_job_last_finish_unix",
			"Unix time the most recent stellarindex-ops job run exited.",
			fmt.Sprintf("%d", h.now().Unix()))
		writeGauge("stellarindex_ops_job_last_exit_ok",
			"1 when the most recent stellarindex-ops job run exited cleanly, 0 when it errored.",
			one(exitOK))
	}
	return b.String()
}

// write renders + atomically replaces the textfile. Fail-soft by
// contract — see the type doc.
func (h *JobHeartbeat) write() {
	body := h.render()
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil { //nolint:gosec // world-readable metrics file by design, same as verify-served-values
		return
	}
	if err := os.Rename(tmp, h.path); err != nil {
		_ = os.Remove(tmp)
	}
}

// sanitizeJobName reduces a subcommand name to the characters that are
// safe in a filename, so the default path can be derived from the
// subcommand without an injection seam.
func sanitizeJobName(job string) string {
	var b strings.Builder
	for _, r := range job {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
