//go:build t424evidence

package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── T424 / T449, the leg that is still OPEN: the execution TRIGGER ──────
//
// Listing a package in INT_TEST_PKGS makes every path that runs the
// integration suite run it (integration_pkgs_coverage_test.go guards that).
// It does not make the suite RUN. Two classifiers decide that from the diff:
//
//   - CI: the `integration` class of ci.yml's preflight path filter. When it
//     is false the Docker shards skip their suite and report success.
//   - local: scripts/ci/prepush-integration-required.sh, which decides
//     whether `make prepush` adds the Docker-backed tests.
//
// Neither lists every INT_TEST_PKGS package. So a change confined to
// scripts/ops/fx-history-backfill/main.go — e.g. dropping the
// SetDeriveGeneration call the INV-3 test exists to pin — compiles the test
// (the compile gate is unconditional) but EXECUTES it nowhere: not in PR CI,
// not in the local pre-push gate. cmd/stellarindex-ops and
// internal/ops/archive (F-1334, W6-tst-1) have the same hole.
//
// Build-tagged, per this package's convention (see deployed_controls_test.go),
// because it is RED and its fix lives in files another unit must own: the CI
// filter is restated in scripts/ci/check-change-class.sh and a ci.yml step
// fails the job when the two disagree, so neither can be edited alone.
// Print the live status with:
//
//	go test -tags t424evidence ./test/controlwiring/ -run TestT424Trigger -v
//
// When both subtests are green, drop the build tag: this file is then the
// regression guard for the trigger.

// ciIntegrationFilterGlobs returns the globs of the `integration` class in
// ci.yml's preflight path filter.
func ciIntegrationFilterGlobs(t *testing.T, root string) []string {
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
		if len(classes["integration"]) == 0 {
			t.Fatal("preflight filter has no `integration` class")
		}
		return classes["integration"]
	}
	t.Fatal("ci.yml: no preflight step with id `filter`")
	return nil
}

var prepushCaseLine = regexp.MustCompile(`(?m)^\s*(migrations/\*\|[^)]*)\)`)

// prepushIntegrationGlobs returns the `dir/*` alternatives of the case arm in
// prepush-integration-required.sh that marks a path as needing Docker tests.
func prepushIntegrationGlobs(t *testing.T, root string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "ci", "prepush-integration-required.sh"))
	if err != nil {
		t.Fatalf("read prepush classifier: %v", err)
	}
	m := prepushCaseLine.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("prepush-integration-required.sh: could not find the path-class case arm")
	}
	return strings.Split(m[1], "|")
}

// triggerCovers reports whether a classifier glob ("dir/**" in the CI
// filter, "dir/*" in the shell case arm — both match everything beneath
// dir) fires for EVERY file under the package pattern's base directory. A
// glob rooted below the base covers only part of it and does not count.
func triggerCovers(glob, pkgPattern string) bool {
	base := strings.TrimSuffix(pkgPattern, "/...")
	dir := strings.TrimSuffix(strings.TrimSuffix(glob, "/**"), "/*")
	if dir == glob {
		return false // an exact-file glob such as go.mod
	}
	return base == dir || strings.HasPrefix(base, dir+"/")
}

func TestT424TriggerCoversEveryIntTestPkg(t *testing.T) {
	root := repoRoot(t)
	patterns := intTestPkgPatterns(t, root)

	classifiers := []struct {
		name  string
		globs []string
	}{
		{"ci.yml preflight `integration` filter", ciIntegrationFilterGlobs(t, root)},
		{"scripts/ci/prepush-integration-required.sh", prepushIntegrationGlobs(t, root)},
	}
	for _, c := range classifiers {
		t.Run(c.name, func(t *testing.T) {
			for _, p := range patterns {
				covered := false
				for _, g := range c.globs {
					if triggerCovers(g, p) {
						covered = true
						break
					}
				}
				if !covered {
					t.Errorf("a change under ./%s does not trigger the integration suite (%s matches: %s) — "+
						"its integration-tagged tests compile but are executed by no gate for that change",
						strings.TrimSuffix(p, "/..."), c.name, strings.Join(c.globs, " "))
				}
			}
		})
	}
}
