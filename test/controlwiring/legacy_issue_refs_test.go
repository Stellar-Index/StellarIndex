package controlwiring

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Tracker numbers written before the repo moved to its current GitHub home
// now resolve to unrelated (often dependabot) PRs there. These are the
// citation families that were repointed to commit shas or plain text; a
// blanket bare-#N ban is not possible because current-era #N refs are valid.
var legacyIssueRefFamilies = []struct {
	re    *regexp.Regexp
	allow map[string]bool // files where the same label means something else
	why   string
}{
	{
		re: regexp.MustCompile(`BACKLOG #60\b`),
		allow: map[string]bool{
			"docs/adr/0045-sep40-oracle-read-adapter.md":     true,
			"docs/architecture/sep40-oracle-read-adapter.md": true,
		},
		why: "price alerts landed in commit 7145a7e51; the private backlog number reads as gh#60",
	},
	{re: regexp.MustCompile(`Wiring PR \| #\d`), why: "cite the wiring commit sha"},
	{re: regexp.MustCompile(`landed in #4[01]\b`), why: "CCTP/Rozo wiring is 1b9a594b4 / 46e0087e8"},
	{re: regexp.MustCompile(`open under Task #72`), why: "the HA sub-roles' code has landed"},
	{re: regexp.MustCompile(`#32 ?/ ?#37b`), why: "pre-migration task numbers for the usage rollup"},
	{re: regexp.MustCompile(`#43(\)| rollup)`), why: "the 24h rollups are 78dff337b / e0fbbbc3b"},
	{re: regexp.MustCompile("(?i)(defense-in-depth for\\s+|post-)`?#26\\b|#26 was a 23-day"), why: "the archive-stall fix is f12289f6d"},
}

// A number named after its tracker ("BACKLOG #43", "board #43") is the
// accepted citation form, so only a bare #N counts.
var trackerLabelSuffix = regexp.MustCompile(`(?i)\b(backlog|board|tasks?|inventory|roadmap|adr-\d+|gap)(\s*#\d+\s*/)*\s*$`)

func legacyIssueRefHit(re *regexp.Regexp, line string) string {
	for _, loc := range re.FindAllStringIndex(line, -1) {
		if line[loc[0]] == '#' && trackerLabelSuffix.MatchString(line[:loc[0]]) {
			continue
		}
		return line[loc[0]:loc[1]]
	}
	return ""
}

// Applied migrations are checksum-pinned and CHANGELOG is release history;
// both keep their original wording.
func legacyIssueRefSkipped(rel string) bool {
	return rel == "CHANGELOG.md" ||
		strings.HasPrefix(rel, "migrations/") && strings.HasSuffix(rel, ".sql")
}

func TestNoLegacyIssueReferences(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	exts := map[string]bool{
		".go": true, ".md": true, ".yml": true, ".yaml": true, ".toml": true, ".sh": true,
		".j2": true, ".service": true, ".timer": true, ".ts": true, ".tsx": true, ".json": true, ".sql": true,
	}
	self := "test/controlwiring/legacy_issue_refs_test.go"
	dirs := []string{
		"cmd", "configs", "deploy", "docs", "examples", "internal", "migrations",
		"openapi", "pkg", "scripts", "test", "web/explorer/src",
	}
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if !exts[filepath.Ext(rel)] || rel == self || legacyIssueRefSkipped(rel) {
				return nil
			}
			b, err := os.ReadFile(p) //nolint:gosec // repo-relative, test-only
			if errors.Is(err, fs.ErrNotExist) {
				// Another verify lane's fixture, deleted between ReadDir and here.
				return nil
			}
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(b), "\n") {
				for _, f := range legacyIssueRefFamilies {
					if hit := legacyIssueRefHit(f.re, line); hit != "" && !f.allow[rel] {
						t.Errorf("%s:%d cites %q, a pre-migration tracker number (%s)", rel, i+1, hit, f.why)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}
