// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// A Healthchecks.io ping that never leaves the host and a service that
// is genuinely down produce the same thing at the other end: silence,
// then a "down" email. Until hc-ping.sh, this host could not tell the
// two apart either — every ping ended in `|| true` with curl's output
// on /dev/null, so a dropped ping left no journal line, no metric, and
// no way to answer "was the service actually down?" after the fact.
//
// These tests drive the real wrapper scripts against a ping endpoint
// that fails, and assert the failure is recorded in both places an
// operator can still read an hour later.

// startPingServer serves the metrics endpoint heartbeat.sh probes and a
// ping endpoint whose status the test controls.
func startPingServer(t *testing.T, pingStatus *atomic.Int32) (*httptest.Server, int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("up 1\n"))
	})
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(pingStatus.Load()))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	port, err := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	if err != nil {
		t.Fatalf("parse httptest port from %s: %v", srv.URL, err)
	}
	return srv, port
}

// runHeartbeat executes the production heartbeat.sh the same way the
// systemd unit does: script path, service instance argument, and the
// Healthchecks URL supplied through the environment.
func runHeartbeat(t *testing.T, port int, textfileDir, pingURL string) (stderr string) {
	t.Helper()
	script := filepath.Join(repoRoot(t), "configs/healthchecks/heartbeat.sh")
	cmd := exec.Command("bash", script, "indexer")
	cmd.Env = append(os.Environ(),
		"INDEXER_METRICS_PORT="+strconv.Itoa(port),
		"HEALTHCHECKS_URL_INDEXER="+pingURL,
		"TEXTFILE_DIR="+textfileDir,
	)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		// The unit contract is exit 0 regardless of what the probe found;
		// a non-zero exit here is itself the defect.
		t.Fatalf("heartbeat.sh exited non-zero (%v); stderr:\n%s", err, errBuf.String())
	}
	return errBuf.String()
}

func readPingMetric(t *testing.T, dir, check, metric string) float64 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "hc_ping_"+check+".prom"))
	if err != nil {
		t.Fatalf("read %s textfile: %v", check, err)
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(metric) + `\{[^}]*\}\s+(\S+)\s*$`)
	m := re.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("%s not found in:\n%s", metric, b)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("parse %s value %q: %v", metric, m[1], err)
	}
	return v
}

// TestUndeliveredPingIsRecorded is the case the old `|| true` erased: the
// service under the check is healthy, and the ping about it is lost.
func TestUndeliveredPingIsRecorded(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusInternalServerError)
	_, port := startPingServer(t, &status)
	dir := t.TempDir()

	stderr := runHeartbeat(t, port, dir, "http://127.0.0.1:"+strconv.Itoa(port)+"/ping")

	if !strings.Contains(stderr, "NOT DELIVERED") {
		t.Errorf("a lost ping left no journal line; stderr was:\n%s", stderr)
	}
	if got := readPingMetric(t, dir, "indexer", "stellarindex_healthcheck_ping_failures_total"); got != 1 {
		t.Errorf("failures_total = %v, want 1 after one undelivered ping", got)
	}
	if got := readPingMetric(t, dir, "indexer", "stellarindex_healthcheck_ping_last_success_unix"); got != 0 {
		t.Errorf("last_success_unix = %v, want 0 — no ping has ever been accepted", got)
	}

	// The counter has to survive the process that wrote it. These are
	// oneshot units: without reading the previous value back out of the
	// textfile, every run would publish 1 and `increase()` would see
	// nothing but resets.
	stderr = runHeartbeat(t, port, dir, "http://127.0.0.1:"+strconv.Itoa(port)+"/ping")
	if got := readPingMetric(t, dir, "indexer", "stellarindex_healthcheck_ping_failures_total"); got != 2 {
		t.Errorf("failures_total = %v after two undelivered pings, want 2 — the counter restarted; stderr:\n%s", got, stderr)
	}

	// A delivered ping stamps last_success and stops adding to the
	// counter, which is what lets an alert distinguish "egress is broken
	// now" from "egress was broken once, last week".
	status.Store(http.StatusOK)
	runHeartbeat(t, port, dir, "http://127.0.0.1:"+strconv.Itoa(port)+"/ping")
	if got := readPingMetric(t, dir, "indexer", "stellarindex_healthcheck_ping_failures_total"); got != 2 {
		t.Errorf("failures_total = %v after a successful ping, want it to hold at 2", got)
	}
	if got := readPingMetric(t, dir, "indexer", "stellarindex_healthcheck_ping_last_success_unix"); got <= 0 {
		t.Errorf("last_success_unix = %v after an accepted ping, want a real timestamp", got)
	}
}

// TestFailedProbeDoesNotPingSuccess guards the invariant the whole
// arrangement rests on: when the probe finds the service down, the
// script must not report success to Healthchecks.io. Reporting success
// there would keep the check green through an outage.
func TestFailedProbeDoesNotPingSuccess(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	_, port := startPingServer(t, &status)
	dir := t.TempDir()

	var paths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(_ http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
	})
	mux.HandleFunc("/ping/fail", func(_ http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
	})
	pingSrv := httptest.NewServer(mux)
	defer pingSrv.Close()

	// Point the metrics probe at a port with nothing on it: curl exits 7,
	// the service reads as down.
	deadPort := port + 1
	script := filepath.Join(repoRoot(t), "configs/healthchecks/heartbeat.sh")
	cmd := exec.Command("bash", script, "indexer")
	cmd.Env = append(os.Environ(),
		"INDEXER_METRICS_PORT="+strconv.Itoa(deadPort),
		"HEALTHCHECKS_URL_INDEXER="+pingSrv.URL+"/ping",
		"TEXTFILE_DIR="+dir,
	)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("heartbeat.sh exited non-zero (%v); stderr:\n%s", err, errBuf.String())
	}

	if len(paths) != 1 || paths[0] != "/ping/fail" {
		t.Fatalf("a down service pinged %v, want exactly [/ping/fail]; stderr:\n%s", paths, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "probe FAILED") {
		t.Errorf("a down service left no journal line; stderr:\n%s", errBuf.String())
	}
}
