package controlwiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The cross-check must list files the way dorny/paths-filter does: a rename
// as delete + add, paths unquoted. Otherwise moving a file out of a watched
// directory makes the two sides disagree and preflight fails on every run.
func TestCrossCheckDiffMatchesDornyListing(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	const step = "Cross-check the filter against check-change-class.sh"
	for _, s := range wf.Jobs["preflight"].Steps {
		if s.Name != step {
			continue
		}
		var diff string
		for _, line := range strings.Split(s.Run, "\n") {
			if strings.Contains(line, "git ") && strings.Contains(line, " diff ") {
				diff = line
			}
		}
		for _, flag := range []string{"--no-renames", "core.quotePath=false"} {
			if !strings.Contains(diff, flag) {
				t.Errorf("%q: diff command %q lacks %s", step, strings.TrimSpace(diff), flag)
			}
		}
		return
	}
	t.Fatalf("ci.yml preflight: no step named %q", step)
}
