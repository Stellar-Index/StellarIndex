// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// ─── #292: the ops-batch identity must not reach a live daemon ──────
//
// [opsAuth] takes the low-priority `ops_batch` identity from the
// PROCESS ENVIRONMENT, and docs/operations/clickhouse-ops-batch-profile.md
// prescribes writing the pair into /etc/default/stellarindex-ops. On the
// ansible hosts that file reaches batch jobs only (the daemons get
// /etc/default/stellarindex). The deploy/systemd reference units share
// it — the indexer reads its MinIO creds and the API its SEP-10 seed
// out of the same file — so a self-hoster who follows the doc used to
// demote the LIVE ledger sink (NewLiveSink -> Open) and the aggregator's
// supply readers (NewExplorerReader) to the batch tier: the precise
// inverse of the 2026-08-28 r1 incident the profile exists to prevent.
//
// The units now strip the pair with `UnsetEnvironment=`, which systemd
// applies AFTER every Environment=/EnvironmentFile= (systemd.exec(5);
// v235+, the Ubuntu 22.04/24.04 targets ship 249/255). This test pins
// the GUARANTEE rather than the directive: it resolves the environment
// each unit would hand its process when the ops env file carries the
// pair, feeds it to [opsAuthFrom], and asserts the identity that comes
// out — CH's `default` user for the live daemons, `ops_batch` for the
// batch one-shots that share the file (so a future over-broad strip
// cannot quietly re-break #243 in the other direction).
//
// Deliberately NOT credited as a strip: `Environment=VAR=` neutralising.
// systemd.exec(5) documents EnvironmentFile= as overriding Environment=,
// so that idiom is version-dependent; UnsetEnvironment= is the only
// directive specified to run last.

// systemdUnitDirs are the two surfaces that define how a stellarindex
// binary is started: the self-host reference units and the units the
// archival-node role renders on r1.
var systemdUnitDirs = []struct{ dir, suffix string }{
	{"deploy/systemd", ".service"},
	{"configs/ansible/roles/archival-node/templates/systemd", ".service.j2"},
}

// nonServingCmds are the cmd/ binaries that are NOT long-running serving
// daemons and therefore SHOULD authenticate as ops_batch when the
// environment offers it: the admin CLI, the migrator, the SLA prober.
// Every other cmd/stellarindex-* is treated as a live daemon; adding one
// without classifying it here fails the test on purpose.
var nonServingCmds = map[string]bool{
	"stellarindex-ops":       true, // the batch CLI — the profile's whole point
	"stellarindex-migrate":   true, // one-shot, opens Postgres only
	"stellarindex-sla-probe": true, // external black-box prober
}

// opsEnvFileBase is the env file the ops-batch pair is templated into
// (09-minio.yml) and that the reference units share with batch jobs.
const opsEnvFileBase = "stellarindex-ops"

// opsBinary matches a unit whose ExecStart runs the ops CLI directly —
// such a unit IS the batch tier and must resolve to ops_batch (#113).
var opsBinary = map[string]bool{"stellarindex-ops": true}

type systemdUnit struct {
	rel     string // repo-relative path, for messages
	name    string // unit name without the .j2 suffix
	envFile []string
	unset   map[string]bool
	exec    string
}

func TestOpsBatchIdentityNeverReachesLiveDaemons(t *testing.T) {
	root := systemdRepoRoot(t)
	serving := servingBinaries(t, root)

	var units []systemdUnit
	for _, d := range systemdUnitDirs {
		matches, err := filepath.Glob(filepath.Join(root, d.dir, "*"+d.suffix))
		if err != nil {
			t.Fatalf("glob %s/*%s: %v", d.dir, d.suffix, err)
		}
		if len(matches) == 0 {
			t.Fatalf("no %s units found under %s — the glob has drifted and this test would pass vacuously", d.suffix, d.dir)
		}
		for _, m := range matches {
			units = append(units, parseSystemdUnit(t, root, m, d.suffix))
		}
	}

	requireHeavyJobWrapperImportsPair(t, root)
	scripts := loadOpsScripts(t, root)

	var liveChecked, batchChecked []string
	for _, u := range units {
		env := u.resolveEnv(opsEnvPair, scripts)
		auth, err := opsAuthFrom(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("%s: opsAuthFrom on the resolved unit environment: %v", u.rel, err)
		}
		arm, violation := identityViolation(u, auth, serving, scripts)
		switch arm {
		case armLive:
			liveChecked = append(liveChecked, u.rel)
		case armBatch:
			batchChecked = append(batchChecked, u.rel)
		}
		if violation != "" {
			t.Error(violation)
		}
	}

	// Non-vacuity: the six known live-daemon units, and batch units from
	// each batch route (direct, via a shipped script), must be examined.
	requireExamined(t, "live-daemon", liveChecked, []string{
		"deploy/systemd/stellarindex-indexer.service",
		"deploy/systemd/stellarindex-aggregator.service",
		"deploy/systemd/stellarindex-api.service",
		"configs/ansible/roles/archival-node/templates/systemd/stellarindex-indexer.service.j2",
		"configs/ansible/roles/archival-node/templates/systemd/stellarindex-aggregator.service.j2",
		"configs/ansible/roles/archival-node/templates/systemd/stellarindex-api.service.j2",
	})
	requireExamined(t, "batch", batchChecked, []string{
		"configs/ansible/roles/archival-node/templates/systemd/cap67-movements.service.j2",
		"configs/ansible/roles/archival-node/templates/systemd/ch-supply.service.j2",
		"configs/ansible/roles/archival-node/templates/systemd/compute-completeness.service.j2",
		"deploy/systemd/stellarindex-completeness.service",
	})
}

// opsEnvPair is the ops env file's pair, as
// docs/operations/clickhouse-ops-batch-profile.md tells the operator to write it.
var opsEnvPair = map[string]string{OpsUserEnv: "ops_batch", OpsPasswordEnv: "vault-generated-hex"}

const (
	armLive  = "live"
	armBatch = "batch"
)

// identityViolation classifies u (armLive, armBatch, or "" when the unit
// never reaches a ClickHouse-opening stellarindex binary) and describes
// why auth is the wrong identity for that arm, or returns "" when right.
func identityViolation(u systemdUnit, auth clickhouse.Auth, serving map[string]bool, scripts map[string]opsScript) (arm, violation string) {
	batchWant := clickhouse.Auth{Database: "stellar", Username: opsEnvPair[OpsUserEnv], Password: opsEnvPair[OpsPasswordEnv]}
	switch {
	case u.runsAnyOf(serving):
		want := clickhouse.Auth{Database: "stellar"}
		if auth != want {
			violation = fmt.Sprintf("%s starts a live serving binary but would authenticate to ClickHouse as %+v, want %+v (CH's unauthenticated `default` user).\n"+
				"  It sources %v, and the ops-batch pair is templated into /etc/default/%s, so every ClickHouse connection this daemon opens would run at the LOW-priority ops_batch tier — the inverse of the 2026-08-28 r1 incident (#243, #292).\n"+
				"  Fix: add `UnsetEnvironment=%s %s` to the unit's [Service] section (or stop sourcing the batch env file).",
				u.rel, auth, want, u.envFile, opsEnvFileBase, OpsUserEnv, OpsPasswordEnv)
		}
		return armLive, violation
	case u.runsAnyOf(opsBinary) || u.reachesOpsViaScript(scripts):
		// Running the ops CLI, directly or through a shipped script, IS
		// the batch tier: it MUST resolve to ops_batch whether or not it
		// sources the file (#113, #882 — batch units that sourced nothing
		// ran at CH `default`/serving priority and matched no arm here).
		if auth != batchWant {
			violation = fmt.Sprintf("%s runs stellarindex-ops (directly or via a shipped script) but would authenticate as %+v, want %+v.\n"+
				"  Fix: launch it through %s (imports only the pair), or add `EnvironmentFile=-/etc/default/%s` to the unit's [Service] section.",
				u.rel, auth, batchWant, heavyJobWrapper, opsEnvFileBase)
		}
		return armBatch, violation
	case u.sourcesOpsEnvFile():
		if auth != batchWant {
			violation = fmt.Sprintf("%s is a batch unit sourcing /etc/default/%s but would authenticate as %+v, want %+v.\n"+
				"  Batch jobs are exactly who the low-priority profile is FOR (#243); stripping the pair here re-creates the incident it fixed.",
				u.rel, opsEnvFileBase, auth, batchWant)
		}
		return armBatch, violation
	}
	return "", ""
}

func requireExamined(t *testing.T, kind string, examined, want []string) {
	t.Helper()
	sort.Strings(examined)
	for _, w := range want {
		if i := sort.SearchStrings(examined, w); i >= len(examined) || examined[i] != w {
			t.Fatalf("%s unit %s was not among the units examined (%v) — the discovery drifted and this test is no longer covering it", kind, w, examined)
		}
	}
}

// heavyJobWrapper is /usr/local/sbin/run-heavy-job.sh, rendered by
// 14-stellarindex-services.yml. It imports ONLY the ops-batch pair from
// the ops env file when the caller has not set it; the rest of that file
// (MinIO reader keys, DSN) deliberately stays out of the job
// (scripts/ci/run-heavy-job-test.sh pins the behaviour).
const heavyJobWrapper = "run-heavy-job.sh"

// requireHeavyJobWrapperImportsPair grounds resolveEnv's wrapper model in
// the shipped wrapper: if the import goes, the model is fiction.
func requireHeavyJobWrapperImportsPair(t *testing.T, root string) {
	t.Helper()
	const task = "configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml"
	b, err := os.ReadFile(filepath.Join(root, task))
	if err != nil {
		t.Fatalf("read %s: %v", task, err)
	}
	for _, want := range []string{
		`OPS_ENV="${HEAVY_JOB_OPS_ENV:-/etc/default/` + opsEnvFileBase + `}"`,
		OpsUserEnv + "=*|" + OpsPasswordEnv + "=*)",
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s no longer contains %q: %s may have stopped importing the ops-batch pair, so this test's model of wrapped units is no longer true", task, want, heavyJobWrapper)
		}
	}
}

// opsScript is what this test needs to know about a script a unit execs.
type opsScript struct {
	invokesOps    bool // runs /usr/local/bin/stellarindex-ops*
	exportsOpsEnv bool // `load_env_file /etc/default/stellarindex-ops export`
}

// opsScriptDirs are the repo directories the scripts named in unit
// ExecStart lines are installed from.
var opsScriptDirs = []string{"configs/ansible/roles/archival-node/files", "scripts/ops"}

// loadOpsScripts indexes every shipped script by basename (the name a
// unit's ExecStart uses once installed), from its non-comment lines.
func loadOpsScripts(t *testing.T, root string) map[string]opsScript {
	t.Helper()
	scripts := map[string]opsScript{}
	for _, d := range opsScriptDirs {
		matches, err := filepath.Glob(filepath.Join(root, d, "*"))
		if err != nil || len(matches) == 0 {
			t.Fatalf("glob %s/*: %d matches, err %v — script discovery drifted", d, len(matches), err)
		}
		for _, m := range matches {
			b, err := os.ReadFile(m)
			if err != nil {
				continue // a directory
			}
			s := scripts[filepath.Base(m)]
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "#") {
					continue
				}
				s.invokesOps = s.invokesOps || strings.Contains(line, "/usr/local/bin/stellarindex-ops")
				s.exportsOpsEnv = s.exportsOpsEnv || line == "load_env_file /etc/default/"+opsEnvFileBase+" export"
			}
			scripts[filepath.Base(m)] = s
		}
	}
	return scripts
}

// execScripts yields the shipped scripts named by absolute path in the
// unit's ExecStart (including as run-heavy-job.sh's command argument).
func (u systemdUnit) execScripts(scripts map[string]opsScript) []opsScript {
	var out []opsScript
	for _, tok := range execTokens(u.exec) {
		if s, ok := scripts[filepath.Base(tok)]; ok && strings.HasPrefix(tok, "/") {
			out = append(out, s)
		}
	}
	return out
}

func (u systemdUnit) reachesOpsViaScript(scripts map[string]opsScript) bool {
	for _, s := range u.execScripts(scripts) {
		if s.invokesOps {
			return true
		}
	}
	return false
}

// resolveEnv models the environment the unit's ClickHouse-opening child
// process sees, for the ops-batch pair only: every EnvironmentFile=
// naming the ops env file contributes `contents`, UnsetEnvironment= is
// applied after them (systemd.exec(5)), and then, inside the process,
// run-heavy-job.sh imports the pair when unset and a script that
// exports the ops env file loads it.
func (u systemdUnit) resolveEnv(contents map[string]string, scripts map[string]opsScript) map[string]string {
	env := map[string]string{}
	for _, f := range u.envFile {
		if filepath.Base(f) != opsEnvFileBase {
			continue
		}
		for k, v := range contents {
			env[k] = v
		}
	}
	for k := range u.unset {
		delete(env, k)
	}
	imports := u.runsAnyOf(map[string]bool{heavyJobWrapper: true}) && env[OpsUserEnv] == ""
	for _, s := range u.execScripts(scripts) {
		imports = imports || s.exportsOpsEnv
	}
	if imports {
		env[OpsUserEnv], env[OpsPasswordEnv] = contents[OpsUserEnv], contents[OpsPasswordEnv]
	}
	return env
}

func (u systemdUnit) sourcesOpsEnvFile() bool {
	for _, f := range u.envFile {
		if filepath.Base(f) == opsEnvFileBase {
			return true
		}
	}
	return false
}

// runsAnyOf reports whether the unit's ExecStart executes one of the
// given binaries, including through a `/bin/sh -c '...'` wrapper.
func (u systemdUnit) runsAnyOf(bins map[string]bool) bool {
	for _, tok := range execTokens(u.exec) {
		if strings.HasPrefix(tok, "/") && bins[filepath.Base(tok)] {
			return true
		}
	}
	return false
}

func execTokens(exec string) []string {
	return strings.FieldsFunc(exec, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\'' || r == '"'
	})
}

// parseSystemdUnit reads the [Service] directives this test reasons
// about. Unit files here are plain key=value with no line continuations;
// a continuation would be silently mis-parsed, so refuse one.
func parseSystemdUnit(t *testing.T, root, path, suffix string) systemdUnit {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("relativise %s: %v", path, err)
	}
	u := systemdUnit{rel: filepath.ToSlash(rel), name: strings.TrimSuffix(filepath.Base(path), suffix), unset: map[string]bool{}}

	section := ""
	execCont := false
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if execCont { // ExecStart wrapped with a trailing backslash
			u.exec += " " + trimmed
			execCont = strings.HasSuffix(trimmed, `\`)
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = trimmed
			continue
		}
		if section != "[Service]" {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		switch key {
		case "EnvironmentFile":
			// A leading `-` marks the file optional; the path is what matters.
			u.envFile = append(u.envFile, strings.TrimPrefix(strings.TrimSpace(value), "-"))
		case "UnsetEnvironment":
			// An empty assignment resets the list (systemd.exec(5)).
			if strings.TrimSpace(value) == "" {
				u.unset = map[string]bool{}
				continue
			}
			for _, name := range strings.Fields(value) {
				n, _, _ := strings.Cut(name, "=") // NAME or NAME=VALUE
				u.unset[n] = true
			}
		case "ExecStart":
			u.exec = value
			execCont = strings.HasSuffix(trimmed, `\`)
		}
	}
	return u
}

// servingBinaries is every cmd/stellarindex-* that is a live serving
// daemon, i.e. all of them minus the classified non-serving ones.
func servingBinaries(t *testing.T, root string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatalf("read cmd/: %v", err)
	}
	serving := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || nonServingCmds[e.Name()] {
			continue
		}
		serving[e.Name()] = true
	}
	if len(serving) == 0 {
		t.Fatal("no live serving binaries found under cmd/ — this test would pass vacuously")
	}
	return serving
}

func systemdRepoRoot(t *testing.T) string {
	t.Helper()
	// internal/storage/clickhouse -> repo root
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}

// TestOpsBatchIdentityScriptRoutedBatchUnits pins the arm for a unit that
// reaches stellarindex-ops through a shipped script (#882): one that gets
// the pair from nowhere must be reported, not silently skipped.
func TestOpsBatchIdentityScriptRoutedBatchUnits(t *testing.T) {
	scripts := map[string]opsScript{
		"run-job.sh":       {invokesOps: true},
		"run-exporting.sh": {invokesOps: true, exportsOpsEnv: true},
		"curl-only.sh":     {},
	}
	serving := map[string]bool{"stellarindex-api": true}
	cases := []struct {
		name, exec    string
		envFile       []string
		wantArm       string
		wantViolation bool
	}{
		{"script sourcing nothing", "/usr/local/sbin/run-job.sh", []string{"/etc/default/stellarindex"}, armBatch, true},
		{"script under the heavy-job wrapper", "/usr/local/sbin/run-heavy-job.sh job /usr/local/sbin/run-job.sh", []string{"/etc/default/stellarindex"}, armBatch, false},
		{"script that exports the ops env", "/usr/local/bin/run-exporting.sh", nil, armBatch, false},
		{"script with the ops EnvironmentFile", "/usr/local/sbin/run-job.sh", []string{"/etc/default/stellarindex-ops"}, armBatch, false},
		{"direct ops sourcing nothing", "/usr/local/bin/stellarindex-ops ch-supply", nil, armBatch, true},
		{"script that never runs the ops CLI", "/usr/local/sbin/curl-only.sh", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := systemdUnit{rel: tc.name, envFile: tc.envFile, unset: map[string]bool{}, exec: tc.exec}
			env := u.resolveEnv(opsEnvPair, scripts)
			auth, err := opsAuthFrom(func(k string) string { return env[k] })
			if err != nil {
				t.Fatal(err)
			}
			arm, violation := identityViolation(u, auth, serving, scripts)
			if arm != tc.wantArm || (violation != "") != tc.wantViolation {
				t.Fatalf("arm %q violation %q, want arm %q violation=%v", arm, violation, tc.wantArm, tc.wantViolation)
			}
		})
	}
}
