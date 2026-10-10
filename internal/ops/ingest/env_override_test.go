package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

// writeIngestConfig drops a minimal, otherwise-valid stellarindex.toml
// carrying a KNOWN-GOOD postgres DSN, so a bare config.Load() accepts the
// file and only an APPLIED env override can make config load fail.
func writeIngestConfig(t *testing.T) string {
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
