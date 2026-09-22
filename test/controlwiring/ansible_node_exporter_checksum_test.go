package controlwiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── Q259: a supply-chain checksum var must guard a real download ──
//
// node_exporter installs via the Debian `prometheus-node-exporter` package
// (archival-node/tasks/10-observability.yml, #33 cutover 2026-05-20),
// unpinned, so the distro can ship CVE fixes. No task reads a
// node_exporter_version or node_exporter_release_sha256 var. Despite that,
// all four test-net inventories declared the identical literal
// node_exporter_release_sha256, styled next to the real (and genuinely
// per-file distinct) minio/mc/pgbackrest_exporter checksums — a false
// signal that the binary is pinned and verified when it is neither.
//
// This asserts the class stays fixed: no tracked ansible config may declare
// a node_exporter checksum/version var, so a copy-paste from another
// exporter block can't reintroduce a checksum that verifies nothing.
func TestQ259_NoDeadNodeExporterChecksumVar(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	dead := []string{"node_exporter_release_sha256", "node_exporter_version"}

	err := filepath.Walk(filepath.Join(root, "configs", "ansible"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || (!strings.HasSuffix(path, ".yml") && !strings.HasSuffix(path, ".yaml")) {
			return nil
		}
		body, rerr := os.ReadFile(path) //nolint:gosec // repo-relative, test-only
		if rerr != nil {
			return rerr
		}
		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue // explanatory comments are fine; only a live key:value is the defect
			}
			for _, name := range dead {
				if strings.HasPrefix(trimmed, name+":") {
					t.Errorf("%s declares %q as a live var, but no ansible task reads it — "+
						"node_exporter installs unpinned via the Debian package (10-observability.yml, "+
						"#33 cutover); a checksum/version var here verifies nothing and falsely implies "+
						"a supply-chain pin", path, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk configs/ansible: %v", err)
	}
}
