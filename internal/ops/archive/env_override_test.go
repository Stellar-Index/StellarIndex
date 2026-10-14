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

// TestArchiveSubcommands_HonorEnvOverride proves trim and rehydrate load config via LoadWithEnv: an
// invalid STELLARINDEX_POSTGRES_DSN override surfaces as a postgres_dsn error before the cold-tier gate.
func TestArchiveSubcommands_HonorEnvOverride(t *testing.T) {
	cases := []struct {
		name string
		run  func(cfgPath string) error
	}{
		{"trim", func(p string) error {
			return trimGalexieArchive([]string{"-config", p, "-older-than-ledger", "1000"})
		}},
		{"rehydrate", func(p string) error {
			return rehydrateGalexieArchive([]string{"-config", p, "-from", "100", "-to", "200"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STELLARINDEX_POSTGRES_DSN", "mysql://injected-but-invalid")
			err := tc.run(writeArchiveConfig(t))
			if err == nil {
				t.Fatal("expected an error: the env-injected DSN must be rejected once overrides are applied")
			}
			if !strings.Contains(err.Error(), "postgres_dsn") || strings.Contains(err.Error(), "cold tier not configured") {
				t.Fatalf("env override not honored: err=%q (bare Load ignores the variable and stops at the cold-tier gate)", err.Error())
			}
		})
	}
}
