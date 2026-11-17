package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestDirtyGuidanceNamesTheForceTarget: the guidance must name the version just below the failed one, which is the
// only force argument that lets `up` run that migration again.
func TestDirtyGuidanceNamesTheForceTarget(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"0001_a.up.sql", "0002_b.up.sql", "0005_c.up.sql", "0005_c.down.sql"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for v, want := range map[int]string{5: "2", 2: "1", 1: "none"} {
		if got := previousMigration(dir, v); got != want {
			t.Errorf("previousMigration(%d) = %q, want %q", v, got, want)
		}
	}
	g := dirtyGuidance(dir, 5)
	if !strings.Contains(g, "migration 5 failed") || !strings.Contains(g, "force 2`") {
		t.Errorf("guidance = %q, want it to name migration 5 and `force 2`", g)
	}
}

// After `up -dsn X` fails, the suggested force must carry -dsn too: pasted bare, it targets $STELLARINDEX_POSTGRES_DSN.
func TestDirtyGuidanceNamesTheTargetDatabase(t *testing.T) {
	dir := t.TempDir()
	defer func(s string) { dsnSource = s }(dsnSource)

	dsnSource = "-dsn"
	if g := dirtyGuidance(dir, 5); !strings.Contains(g, "stellarindex-migrate -dsn <the same DSN as this run> -migrations") {
		t.Errorf("guidance after -dsn = %q; the force command must carry -dsn", g)
	}
	dsnSource = "$STELLARINDEX_POSTGRES_DSN"
	if g := dirtyGuidance(dir, 5); !strings.Contains(g, "$STELLARINDEX_POSTGRES_DSN names") || strings.Contains(g, "-dsn <") {
		t.Errorf("guidance after the env DSN = %q; want it to name the env var and no -dsn", g)
	}
}

// force refuses a database whose state does not call for it, rather than overwriting its version.
func TestCheckForceTargetRefusesWhatTheStateDoesNotCallFor(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"0001_a.up.sql", "0002_b.up.sql", "0005_c.up.sql"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		v      int
		cur    uint
		dirty  bool
		verErr error
		ok     bool
	}{
		{"dirty, force to the version below", 2, 5, true, nil, true},
		{"dirty, force to the dirty version", 5, 5, true, nil, true},
		{"dirty at the first, force none", -1, 1, true, nil, true},
		{"clean database", 2, 5, false, nil, false},
		{"dirty, force skips a migration", 1, 5, true, nil, false},
		{"nothing applied", 2, 0, false, migrate.ErrNilVersion, false},
		{"version unreadable", 2, 0, false, errors.New("boom"), false},
	} {
		if err := checkForceTarget(dir, tc.v, tc.cur, tc.dirty, tc.verErr); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
