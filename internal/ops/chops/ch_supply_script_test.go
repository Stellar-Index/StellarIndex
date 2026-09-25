// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// run-ch-supply.sh is the daily supply_flows gap-fill around `ch-supply
// -seed-flows`. Its exit status is the only signal the
// stellarindex_ch_supply_gapfill_failed alert sees, so a ClickHouse probe
// that fails must fail the unit, not read an error body as a watermark and
// report "seed complete". These tests EXECUTE the shipped script's body
// (everything from its CH() helper on; the lines above it only resolve
// host paths) against stubs for curl, psql, sleep and the ops binary. The
// curl stub behaves like real curl on an HTTP error: the body on stdout and
// exit 0 without -f, nothing and exit 22 with it.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const chSupplyScriptPath = "../../../configs/ansible/roles/archival-node/files/run-ch-supply.sh"

const stubCHSupplyCurl = `#!/usr/bin/env bash
fail=0 q=""
while [ $# -gt 0 ]; do
  case "$1" in
    --data-binary) q="$2"; shift ;;
    --fail) fail=1 ;;
    --*) ;;
    -*f*) fail=1 ;;
  esac
  shift
done
case "$q" in
  *supply_flows*) reply="$STUB_FROM" ;;
  *system.processes*) reply="$STUB_MEM" ;;
  *) reply="" ;;
esac
if [ "$reply" = HTTP500 ]; then
  [ "$fail" = 1 ] && { echo "curl: (22) The requested URL returned error: 500" >&2; exit 22; }
  echo "Code: 60. DB::Exception: Table stellar.supply_flows does not exist. (UNKNOWN_TABLE)"
  exit 0
fi
printf '%s\n' "$reply"
`

const stubCHSupplyPsql = `#!/usr/bin/env bash
printf '%s\n' "$STUB_TIP"
`

const stubCHSupplyOps = `#!/usr/bin/env bash
echo "$*" >> "$STUB_CALLS"
`

const stubCHSupplySleep = `#!/usr/bin/env bash
echo x >> "$STUB_SLEEPS"
`

type chSupplyRun struct {
	exit   int
	out    string
	seeds  []string
	sleeps int
}

// chSupplyScriptBody returns the shipped script from its CH() helper to EOF,
// after checking the premises the harness relies on.
func chSupplyScriptBody(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(chSupplyScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "\nset -uo pipefail\n") {
		t.Fatal("run-ch-supply.sh no longer declares 'set -uo pipefail'; this harness's preamble is stale")
	}
	i := strings.Index(s, "\nCH() {")
	if i < 0 {
		t.Fatal("run-ch-supply.sh has no CH() helper; this harness's extraction point moved")
	}
	head := s[:i]
	for _, v := range []string{"PSQL=", "DSN=", "OPS=", "CONFIG=", "CHADDR=", "CHUNK=", "MEMGUARD="} {
		if !strings.Contains(head, "\n"+v) {
			t.Fatalf("run-ch-supply.sh no longer assigns %s before CH(); the harness preamble would shadow nothing", v)
		}
	}
	return s[i+1:]
}

func runCHSupplyScript(t *testing.T, env map[string]string) chSupplyRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the ops script is bash; r1 and CI are Linux")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash not found — this test must EXECUTE the script: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{"curl": stubCHSupplyCurl, "psql": stubCHSupplyPsql, "stellarindex-ops": stubCHSupplyOps, "sleep": stubCHSupplySleep}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil { //nolint:gosec // an executable test stub
			t.Fatal(err)
		}
	}
	preamble := "set -uo pipefail\n" +
		"PSQL=" + filepath.Join(bin, "psql") + "\nDSN=postgres://stub-host/stubdb\n" +
		"OPS=" + filepath.Join(bin, "stellarindex-ops") + "\nCONFIG=/stub.toml\nCHADDR=127.0.0.1:9300\n" +
		"CHUNK=25000\nMEMGUARD=6442450944\n"
	script := filepath.Join(dir, "run-ch-supply.sh")
	if err := os.WriteFile(script, []byte(preamble+chSupplyScriptBody(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"PATH":        bin + ":/usr/bin:/bin",
		"STUB_CALLS":  filepath.Join(dir, "calls.txt"),
		"STUB_SLEEPS": filepath.Join(dir, "sleeps.txt"),
		"STUB_TIP":    "60100",
		"STUB_FROM":   "100",
		"STUB_MEM":    "0",
	}
	for k, v := range env {
		vars[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, script) //nolint:gosec // test-built script, test-controlled env
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the script did not terminate within 60s (env %v)", env)
	}
	run := chSupplyRun{out: string(out)}
	if runErr != nil {
		ee, ok := runErr.(*exec.ExitError) //nolint:errorlint // exec returns the concrete type
		if !ok {
			t.Fatalf("run script: %v\n%s", runErr, out)
		}
		run.exit = ee.ExitCode()
	}
	if calls := strings.TrimSpace(readIfExists(t, vars["STUB_CALLS"])); calls != "" {
		run.seeds = strings.Split(calls, "\n")
	}
	run.sleeps = strings.Count(readIfExists(t, vars["STUB_SLEEPS"]), "x")
	return run
}

func seedRange(from, to string) string {
	return "ch-supply -config /stub.toml -ch-addr 127.0.0.1:9300 -from " + from + " -to " + to + " -seed-flows"
}

func TestCHSupplyScript_WatermarkProbeFailureFailsTheUnit(t *testing.T) {
	for name, reply := range map[string]string{
		"http error":        "HTTP500",
		"non-numeric reply": `\N`,
		"empty reply":       "",
	} {
		t.Run(name, func(t *testing.T) {
			run := runCHSupplyScript(t, map[string]string{"STUB_FROM": reply})
			if run.exit != 1 {
				t.Fatalf("exit = %d, want 1 — a failed supply_flows watermark probe must fail the unit so ch_supply_gapfill_failed can fire\n%s", run.exit, run.out)
			}
			if strings.Contains(run.out, "seed complete") {
				t.Fatalf("script reported success after a failed probe:\n%s", run.out)
			}
			if len(run.seeds) != 0 {
				t.Fatalf("script seeded %v from an unresolved watermark; want nothing", run.seeds)
			}
		})
	}
}

func TestCHSupplyScript_HealthyRunSeedsWatermarkToTip(t *testing.T) {
	run := runCHSupplyScript(t, nil)
	if run.exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", run.exit, run.out)
	}
	want := []string{seedRange("100", "25100"), seedRange("25100", "50100"), seedRange("50100", "60100")}
	if strings.Join(run.seeds, "\n") != strings.Join(want, "\n") {
		t.Fatalf("seeds = %q, want %q", run.seeds, want)
	}
	if run.sleeps != 0 {
		t.Fatalf("slept %d times under a 0-byte memory reading, want 0", run.sleeps)
	}
}

func TestCHSupplyScript_EmptyTableSeedsFromLedgerTwo(t *testing.T) {
	// max() over an empty non-Nullable column is 0, so the probe answers 1.
	run := runCHSupplyScript(t, map[string]string{"STUB_FROM": "1", "STUB_TIP": "1000"})
	if run.exit != 0 || len(run.seeds) != 1 || run.seeds[0] != seedRange("2", "1000") {
		t.Fatalf("exit=%d seeds=%q, want one seed of [2,1000]\n%s", run.exit, run.seeds, run.out)
	}
}

func TestCHSupplyScript_UnreadableMemoryIsNotZero(t *testing.T) {
	for name, reply := range map[string]string{"http error": "HTTP500", "empty reply": ""} {
		t.Run(name, func(t *testing.T) {
			run := runCHSupplyScript(t, map[string]string{"STUB_MEM": reply, "STUB_TIP": "1000"})
			if run.sleeps != 30 {
				t.Fatalf("slept %d times, want the full 30-try wait — an unreadable memory figure must not read as 0 bytes\n%s", run.sleeps, run.out)
			}
			if run.exit != 0 || len(run.seeds) != 1 {
				t.Fatalf("exit=%d seeds=%q, want the guard to give up waiting and seed once", run.exit, run.seeds)
			}
		})
	}
}
