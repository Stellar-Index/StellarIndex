// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
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

	// The ops env file, as docs/operations/clickhouse-ops-batch-profile.md
	// tells the operator to write it.
	const opsUser, opsPass = "ops_batch", "vault-generated-hex"

	var liveChecked, batchChecked []string
	for _, u := range units {
		env := u.resolveEnv(map[string]string{OpsUserEnv: opsUser, OpsPasswordEnv: opsPass})
		auth, err := opsAuthFrom(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("%s: opsAuthFrom on the resolved unit environment: %v", u.rel, err)
		}

		switch {
		case u.runsAnyOf(serving):
			liveChecked = append(liveChecked, u.rel)
			want := clickhouse.Auth{Database: "stellar"}
			if auth != want {
				t.Errorf("%s starts a live serving binary but would authenticate to ClickHouse as %+v, want %+v (CH's unauthenticated `default` user).\n"+
					"  It sources %v, and the ops-batch pair is templated into /etc/default/%s, so every ClickHouse connection this daemon opens would run at the LOW-priority ops_batch tier — the inverse of the 2026-08-28 r1 incident (#243, #292).\n"+
					"  Fix: add `UnsetEnvironment=%s %s` to the unit's [Service] section (or stop sourcing the batch env file).",
					u.rel, auth, want, u.envFile, opsEnvFileBase, OpsUserEnv, OpsPasswordEnv)
			}
		case u.sourcesOpsEnvFile():
			batchChecked = append(batchChecked, u.rel)
			want := clickhouse.Auth{Database: "stellar", Username: opsUser, Password: opsPass}
			if auth != want {
				t.Errorf("%s is a batch unit sourcing /etc/default/%s but would authenticate as %+v, want %+v.\n"+
					"  Batch jobs are exactly who the low-priority profile is FOR (#243); stripping the pair here re-creates the incident it fixed.",
					u.rel, opsEnvFileBase, auth, want)
			}
		}
	}

	// Non-vacuity: both halves must have had something to assert, and the
	// six known live-daemon units must be among them.
	sort.Strings(liveChecked)
	for _, want := range []string{
		"deploy/systemd/stellarindex-indexer.service",
		"deploy/systemd/stellarindex-aggregator.service",
		"deploy/systemd/stellarindex-api.service",
		"configs/ansible/roles/archival-node/templates/systemd/stellarindex-indexer.service.j2",
		"configs/ansible/roles/archival-node/templates/systemd/stellarindex-aggregator.service.j2",
		"configs/ansible/roles/archival-node/templates/systemd/stellarindex-api.service.j2",
	} {
		if sort.SearchStrings(liveChecked, want) >= len(liveChecked) || liveChecked[sort.SearchStrings(liveChecked, want)] != want {
			t.Fatalf("live-daemon unit %s was not among the units examined (%v) — the discovery drifted and this test is no longer covering it", want, liveChecked)
		}
	}
	if len(batchChecked) == 0 {
		t.Fatalf("no batch unit sourcing /etc/default/%s was found — the ops-batch identity has nowhere left to land, or the discovery drifted", opsEnvFileBase)
	}
}

// resolveEnv models the environment systemd hands the unit's process,
// for the ops-batch pair only: every EnvironmentFile= naming the ops env
// file contributes `contents`, then UnsetEnvironment= is applied last
// (systemd.exec(5)).
func (u systemdUnit) resolveEnv(contents map[string]string) map[string]string {
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
	for _, tok := range strings.FieldsFunc(u.exec, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\'' || r == '"'
	}) {
		if strings.HasPrefix(tok, "/") && bins[filepath.Base(tok)] {
			return true
		}
	}
	return false
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
