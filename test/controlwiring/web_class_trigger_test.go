package controlwiring

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// explorerExternalInputs are files outside web/ and openapi/ that explorer
// tests, its build or its postbuild read. A diff confined to one must set
// web=true, or the change merges green and breaks the next full run.
var explorerExternalInputs = []string{
	"CHANGELOG.md",
	"docs/adr/0001-x.md",
	"docs/architecture/overview.md",
	"docs/blog/post.md",
	"docs/operations/api-explorer-coverage.md",
	"internal/incidents/data/2026-01-01-x.md",
	"internal/storage/timescale/cursors.go",
	"scripts/ci/explorer-file-budget.sh",
	"scripts/ci/explorer-seo-lint.sh",
	"scripts/ci/explorer-openapi-check.sh",
	"scripts/ci/explorer-shell-fallback-check.sh",
	"scripts/ci/check-redirects-cap.sh",
	".github/workflows/ci.yml",
}

func TestExplorerExternalInputsTriggerWebClass(t *testing.T) {
	root := repoRoot(t)
	webGlobs := ciFilterGlobs(t, root, "web")
	script := filepath.Join(root, "scripts", "ci", "check-change-class.sh")
	for _, p := range explorerExternalInputs {
		if !globsFire(webGlobs, p) {
			t.Errorf("ci.yml preflight `web` filter (%v) does not match %s", webGlobs, p)
		}
		cmd := exec.Command(script, "web", p)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("check-change-class.sh web %s: err=%v output=%s", p, err, out)
		}
	}
}
