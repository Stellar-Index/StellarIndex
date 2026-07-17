package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeArchiveConfig drops a minimal, otherwise-valid stellarindex.toml
// (everything else falls back to config.Default(), which validates) with
// a KNOWN-GOOD postgres DSN in the file so a bare config.Load() accepts
// it. Cold tiering is left off (no s3_cold_bucket_archive) so the trim /
// rehydrate commands stop at the ColdTieringEnabled gate before any
// network I/O.
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

// TestTrimGalexieArchive_HonorsEnvOverride proves the trim command loads
// config via LoadWithEnv (Load + ApplyEnvOverrides + re-Validate), not
// bare config.Load (C3-14). We inject a DELIBERATELY INVALID
// STELLARINDEX_POSTGRES_DSN override: only a code path that APPLIES the
// override and re-validates surfaces the postgres_dsn error. A bare
// config.Load ignores the override entirely — the file DSN is valid, so
// it sails past config load and fails later at the ColdTieringEnabled
// gate with a completely different ("cold tier not configured") error,
// never mentioning postgres_dsn. So this assertion is red on the
// pre-fix code and green after.
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

// TestRehydrateGalexieArchive_HonorsEnvOverride is the rehydrate-command
// twin of the trim test above (C3-14) — same red→green logic on the same
// invalid-DSN-override signal.
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
