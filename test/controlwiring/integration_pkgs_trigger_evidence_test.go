package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── T424 / T449: an INT_TEST_PKGS package must also TRIGGER the suite ───
//
// Listing a package in INT_TEST_PKGS makes every path that runs the
// integration suite run it (integration_pkgs_coverage_test.go guards that).
// It does not make the suite RUN for a given diff. Three classifiers decide
// that, and all three must name every INT_TEST_PKGS directory:
//
//   - ci.yml's preflight `integration` path filter — the Docker shards' work
//     steps are gated on `needs.preflight.outputs.integration == 'true'`, so
//     when it is false they skip their suite and report success.
//   - scripts/ci/check-change-class.sh — the offline restatement of that
//     filter; a preflight step runs both against the same diff and fails the
//     job when they disagree, so the two can only ever be edited together.
//   - scripts/ci/prepush-integration-required.sh — whether `make prepush`
//     adds the Docker-backed tests locally.
//
// Until this guard, none of them listed scripts/ops, cmd/stellarindex-ops,
// internal/ops/archive or (in CI) test/harness. So a change confined to
// scripts/ops/fx-history-backfill/main.go — e.g. dropping the
// SetDeriveGeneration call that generation_test.go exists to pin, the INV-3
// money invariant that operator fx_quotes corrections carry a positive
// derive generation — COMPILED the test (the compile gate is unconditional)
// and EXECUTED it nowhere: not in PR CI, not in the local pre-push gate.
// F-1334 (cmd/stellarindex-ops) and W6-tst-1 (internal/ops/archive) were the
// same hole, found one package at a time; this closes the class.
//
// Untagged on purpose: it must run in the default suite, the one place a
// newly untriggered package is guaranteed to be noticed.

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

var prepushCaseLine = regexp.MustCompile(`(?m)^\s*(migrations/\*\|[^)\n]*)\)`)

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

var checkClassIntegrationBody = regexp.MustCompile(`class_integration\(\)\s*\{\s*\n\s*grep -E '([^']+)'`)

// checkChangeClassIntegrationRE returns the extended regular expression that
// scripts/ci/check-change-class.sh's `integration` class matches changed
// paths against. Go's RE2 accepts this pattern with the same meaning grep -E
// gives it (alternation, anchors, character classes only); a pattern that
// stopped being expressible here would fail to compile and fail this test
// loudly rather than silently matching nothing.
func checkChangeClassIntegrationRE(t *testing.T, root string) *regexp.Regexp {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "ci", "check-change-class.sh"))
	if err != nil {
		t.Fatalf("read check-change-class.sh: %v", err)
	}
	m := checkClassIntegrationBody.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("check-change-class.sh: could not find the class_integration() grep pattern")
	}
	re, err := regexp.Compile(m[1])
	if err != nil {
		t.Fatalf("compile class_integration pattern %q: %v", m[1], err)
	}
	return re
}

// globFires reports whether a classifier glob ("dir/**" in the CI filter,
// "dir/*" in the shell case arm — both match everything beneath dir) fires
// for the slash-separated repo-relative path. An exact-file glob such as
// go.mod fires only for that path; "**/seg/**" fires for a seg directory at
// any depth.
func globFires(glob, path string) bool {
	if seg, ok := strings.CutPrefix(glob, "**/"); ok && strings.HasSuffix(seg, "/**") {
		seg = strings.TrimSuffix(seg, "/**")
		return strings.HasPrefix(path, seg+"/") || strings.Contains(path, "/"+seg+"/")
	}
	dir := strings.TrimSuffix(strings.TrimSuffix(glob, "/**"), "/*")
	if dir == glob {
		return path == glob
	}
	return strings.HasPrefix(path, dir+"/")
}

func globsFire(globs []string, path string) bool {
	for _, g := range globs {
		if globFires(g, path) {
			return true
		}
	}
	return false
}

// probePaths returns the changed-file paths a diff confined to an
// INT_TEST_PKGS package pattern can consist of: one directly in the package
// directory and one nested below it. A classifier rooted below the base
// would cover only part of the tree and must not count as coverage.
func probePaths(pkgPattern string) []string {
	base := strings.TrimSuffix(pkgPattern, "/...")
	return []string{base + "/probe.go", base + "/nested/probe.go"}
}

func TestT424TriggerCoversEveryIntTestPkg(t *testing.T) {
	root := repoRoot(t)
	patterns := intTestPkgPatterns(t, root)

	ciGlobs := ciIntegrationFilterGlobs(t, root)
	prepushGlobs := prepushIntegrationGlobs(t, root)
	classRE := checkChangeClassIntegrationRE(t, root)

	classifiers := []struct {
		name  string
		rule  string
		fires func(path string) bool
	}{
		{
			"ci.yml preflight `integration` filter",
			strings.Join(ciGlobs, " "),
			func(p string) bool { return globsFire(ciGlobs, p) },
		},
		{
			"scripts/ci/check-change-class.sh class_integration",
			classRE.String(),
			classRE.MatchString,
		},
		{
			"scripts/ci/prepush-integration-required.sh",
			strings.Join(prepushGlobs, " "),
			func(p string) bool { return globsFire(prepushGlobs, p) },
		},
	}
	for _, c := range classifiers {
		t.Run(c.name, func(t *testing.T) {
			for _, p := range patterns {
				for _, probe := range probePaths(p) {
					if c.fires(probe) {
						continue
					}
					t.Errorf("a change to %s does not trigger the integration suite (%s matches: %s) — "+
						"./%s is in INT_TEST_PKGS, so its integration-tagged tests compile but are "+
						"executed by no gate for that change",
						probe, c.name, c.rule, strings.TrimSuffix(p, "/..."))
				}
			}
		})
	}
}

// TestT424TriggerIsNotUniversal keeps the guard above honest in the other
// direction: a classifier that fires for everything would satisfy it while
// making every docs-only PR pay a 20-minute Docker round-trip, the cost the
// path filter exists to avoid.
func TestT424TriggerIsNotUniversal(t *testing.T) {
	root := repoRoot(t)
	ciGlobs := ciIntegrationFilterGlobs(t, root)
	prepushGlobs := prepushIntegrationGlobs(t, root)
	classRE := checkChangeClassIntegrationRE(t, root)

	for _, path := range []string{
		"docs/architecture/ingest-pipeline.md",
		"CHANGELOG.md",
		"web/explorer/src/app/page.tsx",
		"scripts/ci/check-change-class.sh",
		"internal/platform/logging.go",
	} {
		if globsFire(ciGlobs, path) {
			t.Errorf("ci.yml preflight `integration` filter fires for %s — the filter has been widened past its purpose", path)
		}
		if classRE.MatchString(path) {
			t.Errorf("check-change-class.sh class_integration matches %s — the class has been widened past its purpose", path)
		}
		if globsFire(prepushGlobs, path) {
			t.Errorf("prepush-integration-required.sh requires integration for %s — the policy has been widened past its purpose", path)
		}
	}
}

func TestGlobFires(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"internal/storage/**", "internal/storage/pricebook.go", true},
		{"internal/storage/**", "internal/storage/timescale/x.go", true},
		{"internal/storage/**", "internal/storagex/x.go", false},
		{"scripts/ops/*", "scripts/ops/fx-history-backfill/main.go", true},
		{"go.mod", "go.mod", true},
		{"go.mod", "internal/go.mod", false},
	}
	for _, c := range cases {
		if got := globFires(c.glob, c.path); got != c.want {
			t.Errorf("globFires(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}
