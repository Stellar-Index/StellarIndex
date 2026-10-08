package controlwiring

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── no gate job may be continue-on-error ────────────────────
//
// A job-level `continue-on-error: true` makes the job's conclusion
// irrelevant to the run. branch-protection-status was the one such job
// in ci.yml, and it only emitted a ::warning:: on an unprotected main,
// so it could not fail even without the key. This leg GRADUATED out of
// the k023evidence build tag (it lived in deployed_controls_test.go)
// once main carried an active ruleset and the probe became a gate.
func TestK023_NoContinueOnErrorJobsInCI(t *testing.T) {
	t.Parallel()
	var wf struct {
		Jobs map[string]struct {
			ContinueOnError any `yaml:"continue-on-error"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, ".github/workflows/ci.yml")), &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatal("ci.yml parsed to zero jobs — this test is asserting nothing")
	}
	for name, job := range wf.Jobs {
		if b, ok := job.ContinueOnError.(bool); ok && b {
			t.Errorf("ci.yml job %q is continue-on-error: true — its verdict cannot gate "+
				"anything (F133 / K023 enumeration)", name)
		}
	}
}

// TestK023_BranchProtectionStatusFailsOnZeroRules runs the shipped probe
// step against a stubbed curl: dropping continue-on-error gates nothing
// if the script itself still exits 0 on an unprotected or unreadable main.
func TestK023_BranchProtectionStatusFailsOnZeroRules(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not on PATH: %v (the continue-on-error leg runs unconditionally)", err)
	}
	script := branchProtectionStatusRun(t)
	cases := []struct {
		name     string
		curl     string
		wantFail bool
	}{
		{"zero rules", "printf '[]'", true},
		{"rules API unreachable", "echo 'curl: (22) 503' >&2; exit 22", true},
		{"active ruleset", `printf '[{"type":"deletion"},{"type":"non_fast_forward"}]'`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stubs := t.TempDir()
			if err := os.WriteFile(filepath.Join(stubs, "curl"), []byte("#!/bin/sh\n"+tc.curl+"\n"), 0o700); err != nil {
				t.Fatalf("write curl stub: %v", err)
			}
			cmd := exec.Command("bash", "-e", "-c", script) //nolint:gosec // the step under test, read from ci.yml
			cmd.Env = append(os.Environ(),
				"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
				"REPO=example/repo")
			out, err := cmd.CombinedOutput()
			if gotFail := err != nil; gotFail != tc.wantFail {
				t.Errorf("probe failed=%v, want %v (err=%v)\n%s", gotFail, tc.wantFail, err, out)
			}
		})
	}
}

// branchProtectionStatusRun returns the run body of the probe step, and
// fails if the step stops reading the repository from REPO, which the
// stubbed run above supplies.
func branchProtectionStatusRun(t *testing.T) string {
	t.Helper()
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Env map[string]string `yaml:"env"`
				Run string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readRepoFile(t, ".github/workflows/ci.yml")), &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	job, ok := wf.Jobs["branch-protection-status"]
	if !ok {
		t.Fatal("ci.yml has no branch-protection-status job — re-derive this test")
	}
	for _, step := range job.Steps {
		if strings.Contains(step.Run, "/rules/branches/main") {
			if _, ok := step.Env["REPO"]; !ok {
				t.Fatalf("branch-protection-status probe no longer takes REPO from env — re-derive this test:\n%s", step.Run)
			}
			return step.Run
		}
	}
	t.Fatal("branch-protection-status has no step querying /rules/branches/main — re-derive this test")
	return ""
}
