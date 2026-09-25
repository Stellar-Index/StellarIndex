package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// migrateChangedWhenRE matches a migrate task's changed_when and captures
// the marker it looks for in stdout. A negated (`not in`) test has no match.
var migrateChangedWhenRE = regexp.MustCompile(
	`changed_when:\s*"'([^']+)' in \(?migrate_result\.stdout`)

// TestAnsibleMigrateChangedWhenMatchesCmdUp: every Ansible task that runs
// `stellarindex-migrate up` reports changed from cmdUp's stdout, so its
// marker must appear in the applied line and not in the no-change line.
// A marker the binary never prints makes the task lie on every run.
func TestAnsibleMigrateChangedWhenMatchesCmdUp(t *testing.T) {
	applied := fmt.Sprintf(upAppliedFormat, 142, false)
	root := filepath.Join("..", "..", "configs", "ansible")
	checked := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".yml" {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, "changed_when:") || !strings.Contains(line, "migrate_result") {
				continue
			}
			checked++
			m := migrateChangedWhenRE.FindStringSubmatch(line)
			switch {
			case m == nil:
				t.Errorf("%s:%d: %s — want `changed_when: \"'<marker>' in migrate_result.stdout\"`", p, i+1, strings.TrimSpace(line))
			case !strings.Contains(applied, m[1]):
				t.Errorf("%s:%d: marker %q is not in cmdUp's applied output %q", p, i+1, m[1], applied)
			case strings.Contains(upNoChangeMsg, m[1]):
				t.Errorf("%s:%d: marker %q also matches the no-change output %q", p, i+1, m[1], upNoChangeMsg)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if checked < 2 {
		t.Fatalf("found %d migrate changed_when lines, want >= 2 (deploy-binary.yml, archival-node); the scan is broken", checked)
	}
}
