package controlwiring

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ─── RLT-046: every non-.go file a Go test reads, and every file
// clickhouse-exporter-test.sh reads, must trigger the CI class that runs it ───
//
// The `go` class matched only *.go, go.mod/sum and openapi/**, so a diff
// confined to a go:embed input set `go=false` and skipped the test job that
// decodes it. The `ansible` class matched only configs/ansible/**, yet
// clickhouse-exporter-test.sh (run only by the ansible-check job) also reads
// configs/prometheus/** and deploy/monitoring/**.
//
// Both guards derive their inputs from the source of truth (every //go:embed
// directive; the paths named in the script) rather than a list, so a new
// embed or a new file read by the script fails here until a class covers it.
// Untagged on purpose, matching openapi_go_class_trigger_test.go.

var goEmbedDirective = regexp.MustCompile(`^//go:embed\s+(.+)$`)

// embedInputs returns the repo-relative path of every file matched by a
// //go:embed directive in the Go tree.
func embedInputs(t *testing.T, root string) []string {
	t.Helper()
	var inputs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "web", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		inputs = append(inputs, embedInputsOf(t, root, p)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return inputs
}

func embedInputsOf(t *testing.T, root, goFile string) []string {
	t.Helper()
	raw, err := os.ReadFile(goFile)
	if err != nil {
		t.Fatalf("read %s: %v", goFile, err)
	}
	var inputs []string
	for _, line := range strings.Split(string(raw), "\n") {
		m := goEmbedDirective.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		for _, pattern := range strings.Fields(m[1]) {
			matches, err := filepath.Glob(filepath.Join(filepath.Dir(goFile), strings.Trim(pattern, "`\"")))
			if err != nil || len(matches) == 0 {
				t.Fatalf("%s: //go:embed %s matches no file (err=%v)", goFile, pattern, err)
			}
			for _, match := range matches {
				inputs = append(inputs, filesUnder(t, root, match)...)
			}
		}
	}
	return inputs
}

func filesUnder(t *testing.T, root, path string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", path, err)
	}
	return files
}

// assertClassFires checks one path against both copies of a change class:
// ci.yml's preflight filter and scripts/ci/check-change-class.sh.
func assertClassFires(t *testing.T, root, class, path string) {
	t.Helper()
	if globs := ciFilterGlobs(t, root, class); !globsFire(globs, path) {
		t.Errorf("ci.yml preflight `%s` filter (%v) does not match %s — a diff "+
			"confined to it skips the job whose tests read it", class, globs, path)
	}
	cmd := exec.Command(filepath.Join(root, "scripts", "ci", "check-change-class.sh"), class, path)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("check-change-class.sh %s %s: %v %s — the offline restatement of "+
			"the `%s` filter does not fire for it", class, path, err, out, class)
	}
}

func TestEmbedInputsTriggerGoClass(t *testing.T) {
	root := repoRoot(t)
	inputs := embedInputs(t, root)
	// Self-accounting: the four production embeds plus the clickhouse
	// testdata fixtures. Zero would mean the walk, not the classes, broke.
	if len(inputs) < 7 {
		t.Fatalf("found %d //go:embed inputs (%v), want >= 7 — the directive scan is broken", len(inputs), inputs)
	}
	for _, p := range inputs {
		t.Run(p, func(t *testing.T) { assertClassFires(t, root, "go", p) })
	}
}

var exporterTestPath = regexp.MustCompile(`\b(?:configs|deploy)/[A-Za-z0-9_./-]+`)

func TestExporterTestInputsTriggerAnsibleClass(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "ci", "clickhouse-exporter-test.sh"))
	if err != nil {
		t.Fatalf("read clickhouse-exporter-test.sh: %v", err)
	}
	paths := exporterTestPath.FindAllString(string(raw), -1)
	want := map[string]bool{
		"configs/prometheus/prometheus.r1.yml":   false,
		"deploy/monitoring/rules/clickhouse.yml": false,
	}
	for _, p := range paths {
		want[p] = true
		t.Run(p, func(t *testing.T) { assertClassFires(t, root, "ansible", p) })
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("clickhouse-exporter-test.sh no longer names %s — the path scan is broken", p)
		}
	}
}
