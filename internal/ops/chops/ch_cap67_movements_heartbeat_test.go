package chops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gaugeValue pulls one metric's sample from a textfile body, failing the test if the series is absent.
// Local copy: opsutil.gaugeValue is unexported across the package boundary.
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

// TestCap67Progress_AccumulatesAcrossWindowsAndTicks pins the cap67-movements progress metric: a wedged -follow daemon
// holds the watermark fixed, so it needs a heartbeat that differs from a healthy one. record runs per derive window and
// per tick, so total must be the running SUM, not the last count.
func TestCap67Progress_AccumulatesAcrossWindowsAndTicks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops_job_ch_cap67_movements.prom")

	prog := newCap67Progress(path)
	if !prog.hb.Enabled() {
		t.Fatal("explicit path must enable the heartbeat")
	}

	// Two windows within one catch-up, then a later -follow tick — the
	// shape runCap67CatchUp / runCap67Follow drive this callback with.
	prog.record(5, 63_100_000)
	prog.record(3, 63_150_000)
	prog.record(2, 63_150_100)
	prog.stop(true)

	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("heartbeat textfile not written: %v", err)
	}
	body := string(raw)

	if got := gaugeValue(t, body, "stellarindex_ops_job_progress_total"); got != "10" {
		t.Errorf("progress_total = %s, want 10 (5+3+2, cumulative across calls)", got)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_progress_cursor"); got != "63150100" {
		t.Errorf("progress_cursor = %s, want the highest ledger reached (63150100)", got)
	}
	if !strings.Contains(body, `{ops_job="ch-cap67-movements"}`) {
		t.Errorf("series must carry ops_job=\"ch-cap67-movements\":\n%s", body)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_running"); got != "0" {
		t.Errorf("running after stop = %s, want 0", got)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_last_exit_ok"); got != "1" {
		t.Errorf("last_exit_ok = %s, want 1", got)
	}
}

// TestCap67Progress_InertWithoutHeartbeatPath: an empty -heartbeat flag (the default, and every non-r1 environment)
// makes the heartbeat a safe no-op, with no directory created and no panic on record/stop with an unresolved path.
func TestCap67Progress_InertWithoutHeartbeatPath(t *testing.T) {
	prog := newCap67Progress("")
	if prog.hb.Enabled() {
		t.Fatal("empty path without DefaultTextfileDir present must stay inert")
	}
	prog.record(1, 100)
	prog.stop(false)
}
