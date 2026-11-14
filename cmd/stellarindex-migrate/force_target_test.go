package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4/database"
)

// A forced version with no migration file makes every later `up` fail, and 0 is not the nothing-applied state.
func TestForceTarget(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"0001_init.up.sql", "0001_init.down.sql", "0218_last.up.sql"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		arg  string
		want int
		ok   bool
	}{
		{"none", database.NilVersion, true},
		{"1", 1, true},
		{"218", 218, true},
		{"0", 0, false},
		{"2180", 0, false},
		{"-1", 0, false},
		{"x", 0, false},
	} {
		got, err := forceTarget(dir, tc.arg)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("forceTarget(%q) = %d, %v; want %d, ok=%v", tc.arg, got, err, tc.want, tc.ok)
		}
	}
}
