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

// DefaultTextfileDir is the node_exporter textfile directory; heartbeats default here and are inert where it is absent.
const DefaultTextfileDir = "/var/lib/node_exporter/textfile_collector"

// jobHeartbeatInterval must stay well under the stale alert window (15 min) so a scrape gap never reads as a dead process.
const jobHeartbeatInterval = 60 * time.Second

// jobHeartbeatLabel is `ops_job`, NOT the reserved `job`: Prometheus renames a scraped `job` to `exported_job`, which broke the
// alert joins. Changing it is a wire-format change; the alert exprs join `on (ops_job, instance, pid)`.
const jobHeartbeatLabel = "ops_job"

// JobHeartbeat publishes one long ops job's liveness and progress as node_exporter gauges; one run per textfile, fail-soft.
type JobHeartbeat struct {
	job  string
	path string
	now  func() time.Time

	mu       sync.Mutex
	started  time.Time
	total    uint64
	cursor   uint64
	bytes    uint64
	running  bool
	finished bool
	exitOK   bool

	lock *os.File // held for the process lifetime; see claimPath
	stop chan struct{}
	done chan struct{}
}

// NewJobHeartbeat builds a heartbeat for `job`; an empty path resolves under [DefaultTextfileDir] only if it exists, else the heartbeat is INERT.
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

// claimPath takes a non-blocking flock on `<path>.lock`; a loser falls back to `<path>.pid<N>.prom` carrying a `pid` label,
// since identical label sets across files make node_exporter fail the whole scrape and the alert joins need pid.
// Fail-soft: if the lock cannot be created the original path is used unlocked.
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

// sweepStalePIDFiles deletes `.pid<N>.prom` siblings whose process is gone, bounding `pid` label cardinality.
// Liveness is kill(pid, 0) with EPERM counted as ALIVE, so errors keep a file rather than blind a live run.
// Unreadable directories and unremovable files are skipped.
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

// Enabled reports whether this heartbeat will write anything.
func (h *JobHeartbeat) Enabled() bool { return h != nil && h.path != "" }

// Path is the textfile this heartbeat writes ("" when inert).
func (h *JobHeartbeat) Path() string {
	if h == nil {
		return ""
	}
	return h.path
}

// Start stamps running=1 and launches the ticker that keeps the heartbeat fresh; call Stop exactly once.
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
				h.sweepIfPrimary()
			}
		}
	}()
}

// sweepIfPrimary reaps dead `.pidN.prom` siblings, only from the lock holder, on each tick and at Stop.
func (h *JobHeartbeat) sweepIfPrimary() {
	h.mu.Lock()
	primary := h.lock != nil
	path := h.path
	h.mu.Unlock()
	if primary {
		sweepStalePIDFiles(path)
	}
}

// Progress records completed units and the ledger reached; cursor is a MAXIMUM because parallel walkers report out of order.
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

// ProgressBytes records cumulative storage bytes observed to move, a second dimension for phases that complete no units (e.g. decompress), so the stall alert stays armed.
func (h *JobHeartbeat) ProgressBytes(moved uint64) {
	if !h.Enabled() {
		return
	}
	h.mu.Lock()
	h.bytes = moved
	h.mu.Unlock()
}

// Stop halts the ticker and writes running=0 with last_finish_unix and last_exit_ok; safe on an inert heartbeat and when called twice.
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
	// Sweep before releasing the claim: this is the primary's last chance
	// to reap dead siblings for this path without waiting on a future
	// contention that may never come.
	h.sweepIfPrimary()

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
	job, started, total, cursor, moved := h.job, h.started, h.total, h.cursor, h.bytes
	running, finished, exitOK := h.running, h.finished, h.exitOK
	h.mu.Unlock()

	var b strings.Builder
	// `ops_job`, never the reserved `job` (see [jobHeartbeatLabel]). A `.pidN` fallback file carries a `pid` label because
	// identical label sets across files make node_exporter fail the whole scrape; the primary omits it to stay label-stable.
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
	// Emitted UNCONDITIONALLY (0 when unused): the alert joins the two counters and an absent series would silently drop jobs from it.
	writeGauge("stellarindex_ops_job_progress_bytes_total",
		"Bytes of storage a stellarindex-ops job has OBSERVED move in the current run during a phase that completes no units of work (a Timescale chunk decompress). Flat TOGETHER with progress_total while running = hung.",
		fmt.Sprintf("%d", moved))
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
