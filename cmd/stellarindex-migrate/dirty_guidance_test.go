package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/database"
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

// force allows every recovery a dirty row calls for, refuses the rest, and names -force-unsafe when it refuses.
// cur is the driver's raw row, so a dirty none row (a failed down of the first migration) is seen as dirty.
func TestCheckForceTargetRefusesWhatTheStateDoesNotCallFor(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"0001_a.up.sql", "0002_b.up.sql", "0005_c.up.sql", "0007_d.up.sql"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	none := database.NilVersion
	for _, tc := range []struct {
		name          string
		v, cur        int
		dirty, unsafe bool
		ok            bool
		msg           string
	}{
		{"(a) failed up of 5, undone: force below", 2, 5, true, false, true, ""},
		{"(b) failed up of 5, finished by hand", 5, 5, true, false, true, ""},
		{"(a) failed up of the first, undone: force none", none, 1, true, false, true, ""},
		{"(c) failed down from 7 leaves 5 dirty: force 7", 7, 5, true, false, true, ""},
		{"(e) failed down of the first leaves none dirty: force 1", 1, none, true, false, true, ""},
		{"(e) failed down of the first, finished: force none", none, none, true, false, true, ""},
		{"dirty, force skips a migration", 1, 5, true, false, false, "force it to 2 (a failed up undone), 5 (finished by hand) or 7"},
		{"dirty, force skips a migration upward", 7, 2, true, false, false, "-force-unsafe"},
		{"(d) clean row refused", 2, 5, false, false, false, "-force-unsafe"},
		{"(d) clean row with -force-unsafe", 2, 5, false, true, true, ""},
		{"(d) clean none row refused", 7, none, false, false, false, "at version none and not dirty"},
		{"-force-unsafe skips a dirty row's check", 1, 7, true, true, true, ""},
	} {
		err := checkForceTarget(dir, tc.v, tc.cur, tc.dirty, tc.unsafe)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: err = %q, want it to contain %q", tc.name, err, tc.msg)
		}
	}
}
