package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeArchiveConfig writes a minimal valid stellarindex.toml with a known-good postgres DSN and cold tiering off, so commands stop at the ColdTieringEnabled gate before network I/O.
func writeArchiveConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stellarindex.toml")
	body := `
[storage]
postgres_dsn = "postgres://good:good@localhost/stellarindex?sslmode=disable"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTrimGalexieArchive_HonorsEnvOverride proves trim loads config via LoadWithEnv: an invalid STELLARINDEX_POSTGRES_DSN override must surface a postgres_dsn error, which bare config.Load would ignore.
func TestTrimGalexieArchive_HonorsEnvOverride(t *testing.T) {
	cfgPath := writeArchiveConfig(t)
	t.Setenv("STELLARINDEX_POSTGRES_DSN", "mysql://injected-but-invalid")

	err := trimGalexieArchive([]string{"-config", cfgPath, "-older-than-ledger", "1000"})
	if err == nil {
		t.Fatal("expected an error (invalid env-injected DSN should be rejected once overrides are applied)")
	}
	if !strings.Contains(err.Error(), "postgres_dsn") {
		t.Fatalf("env override not honored — err=%q does not mention postgres_dsn "+
			"(bare Load ignores STELLARINDEX_POSTGRES_DSN and stops at the cold-tier gate instead)", err.Error())
	}
	// And the override-driven failure must beat the cold-tier gate:
	// LoadWithEnv rejects at config-load time, before ColdTieringEnabled.
	if strings.Contains(err.Error(), "cold tier not configured") {
		t.Fatalf("reached the cold-tier gate — the DSN override was never applied: %v", err)
	}
}

// TestRehydrateGalexieArchive_HonorsEnvOverride is the rehydrate twin of the trim test above.
func TestRehydrateGalexieArchive_HonorsEnvOverride(t *testing.T) {
	cfgPath := writeArchiveConfig(t)
	t.Setenv("STELLARINDEX_POSTGRES_DSN", "mysql://injected-but-invalid")

	err := rehydrateGalexieArchive([]string{"-config", cfgPath, "-from", "100", "-to", "200"})
	if err == nil {
		t.Fatal("expected an error (invalid env-injected DSN should be rejected once overrides are applied)")
	}
	if !strings.Contains(err.Error(), "postgres_dsn") {
		t.Fatalf("env override not honored — err=%q does not mention postgres_dsn "+
			"(bare Load ignores STELLARINDEX_POSTGRES_DSN and stops at the cold-tier gate instead)", err.Error())
	}
	if strings.Contains(err.Error(), "cold tier not configured") {
		t.Fatalf("reached the cold-tier gate — the DSN override was never applied: %v", err)
	}
}
