package archive

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hotFloorConsumers render stellarindex_archive_hot_floor into the fill's
// static floor, archive-completeness's -from, and every verify-archive
// tier's cold-start -from.
var hotFloorConsumers = []string{
	"configs/ansible/roles/archival-node/tasks/07-galexie.yml",
	"configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml",
	"configs/ansible/roles/archival-node/templates/systemd/archive-completeness.service.j2",
	"configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-a.service.j2",
	"configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-b.service.j2",
}

// TestArchiveHotFloor_OneRoleDefault: the hot floor is one concept with
// several consumers. Per-consumer `| default(...)` fallbacks disagreed (0 on
// the fill, 2 on the verifiers) and hid that no defaults file defined it, so
// the value is pinned once in the role defaults and nowhere else.
func TestArchiveHotFloor_OneRoleDefault(t *testing.T) {
	t.Parallel()

	var defaults map[string]any
	if err := yaml.Unmarshal([]byte(readRepoFile(t, "configs/ansible/roles/archival-node/defaults/main.yml")), &defaults); err != nil {
		t.Fatalf("parse role defaults: %v", err)
	}
	floor, ok := defaults["stellarindex_archive_hot_floor"].(int)
	if !ok {
		t.Fatalf("roles/archival-node/defaults/main.yml does not define stellarindex_archive_hot_floor as an integer (got %#v)",
			defaults["stellarindex_archive_hot_floor"])
	}
	// 2 is the first ledger galexie exports: "no floor". Anything higher as a
	// ROLE default would make a greenfield node skip mirroring real history.
	if floor != 2 {
		t.Errorf("role default stellarindex_archive_hot_floor = %d, want 2 (no floor); a trimmed region sets its own in inventory", floor)
	}

	inline := regexp.MustCompile(`stellarindex_archive_hot_floor\s*\|\s*default\b`)
	use := regexp.MustCompile(`\{\{\s*stellarindex_archive_hot_floor\b`)
	for _, rel := range hotFloorConsumers {
		body := readRepoFile(t, rel)
		if !use.MatchString(body) {
			t.Errorf("%s no longer renders stellarindex_archive_hot_floor — update hotFloorConsumers", rel)
		}
		if loc := inline.FindStringIndex(body); loc != nil {
			t.Errorf("%s:%d carries an inline `| default(...)` for stellarindex_archive_hot_floor; the role default is the only producer",
				rel, 1+strings.Count(body[:loc[0]], "\n"))
		}
	}

	// No consumer outside the list above may carry its own fallback either.
	roleDir := filepath.Join(repoRootForTest(t), "configs", "ansible", "roles")
	err := filepath.WalkDir(roleDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if inline.Match(b) {
			t.Errorf("%s carries an inline `| default(...)` for stellarindex_archive_hot_floor", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", roleDir, err)
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
