package controlwiring

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── RLT-169: an openapi/** diff must trigger the `go` change class ───
//
// internal/api/v1/handler_spec_fields_test.go (and its spec-parity
// siblings across internal/api/v1, cmd/stellarindex-aggregator and
// pkg/client) exist to catch a server response field, or any other server
// behaviour, drifting from openapi/stellar-index.v1.yaml. They are Go
// tests, so they run only when ci.yml's `test` job runs, which is gated
// on `needs.preflight.outputs.go == 'true'`. Before this guard, the `go`
// class matched only `.go` files, go.mod and go.sum — a diff confined to
// the OpenAPI spec set `go=false` and every one of those tests, this one
// included, was compiled by the unconditional compile gate and executed
// by nothing.
//
// Untagged on purpose, matching integration_pkgs_trigger_evidence_test.go:
// this must run in the default suite, the one place a regression here is
// guaranteed to be noticed.

// ciFilterGlobs returns the globs of the named class in ci.yml's preflight
// path filter.
func ciFilterGlobs(t *testing.T, root, class string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				ID   string `yaml:"id"`
				With struct {
					Filters string `yaml:"filters"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	for _, step := range wf.Jobs["preflight"].Steps {
		if step.ID != "filter" {
			continue
		}
		var classes map[string][]string
		if err := yaml.Unmarshal([]byte(step.With.Filters), &classes); err != nil {
			t.Fatalf("parse preflight filter block: %v", err)
		}
		if len(classes[class]) == 0 {
			t.Fatalf("preflight filter has no `%s` class", class)
		}
		return classes[class]
	}
	t.Fatal("ci.yml: no preflight step with id `filter`")
	return nil
}

func TestOpenAPIChangeTriggersGoClass(t *testing.T) {
	root := repoRoot(t)
	const specPath = "openapi/stellar-index.v1.yaml"

	t.Run("ci.yml preflight `go` filter", func(t *testing.T) {
		goGlobs := ciFilterGlobs(t, root, "go")
		if !globsFire(goGlobs, specPath) {
			t.Errorf("ci.yml preflight `go` filter (%v) does not match %s — "+
				"the `test` job skips on an openapi-only diff and "+
				"handler_spec_fields_test.go never runs", goGlobs, specPath)
		}
	})

	t.Run("scripts/ci/check-change-class.sh", func(t *testing.T) {
		script := filepath.Join(root, "scripts", "ci", "check-change-class.sh")
		cmd := exec.Command(script, "go", specPath)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("check-change-class.sh go %s: exit err=%v output=%s\n"+
				"the offline restatement of the `go` filter does not fire for "+
				"an openapi-only diff, and ci.yml's preflight step would fail "+
				"the cross-check against the filter above if it were fixed alone",
				specPath, err, out)
		}
	})
}
