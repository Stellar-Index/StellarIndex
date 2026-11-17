package holds

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCheck_RejectsAnInvalidFileAtBoot: at startup there is no previous list to keep, so a typo must stop the boot rather
// than start the API with every hold lifted.
func TestCheck_RejectsAnInvalidFileAtBoot(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("[[hold]\nreason = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Check(bad); err == nil {
		t.Error("Check accepted an unparseable holds file")
	}
	if err := Check(filepath.Join(dir, "absent.toml")); err != nil {
		t.Errorf("missing file: %v, want no holds", err)
	}
}
