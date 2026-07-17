package timescale

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration0113DropsClassicMovements guards the C2-18 / DAT-03
// cleanup: migration 0113 must DROP the dead classic_movements table
// (superseded by ADR-0048 D2 — the pre-P23 movement archive moved to
// ClickHouse-native stellar.account_movements, leaving this Postgres
// hypertable with no live writer or reader) and its .down.sql must
// RECREATE the full 0105 schema so the drop is reversible.
//
// This is the fast, no-Docker guard; test/integration/migrations_test.go
// additionally proves the drop actually executes against real
// TimescaleDB (assertTableAbsent(..., "classic_movements") after the
// full up stack). Without the 0113 migration files this test is RED
// (os.ReadFile below fails on the missing up.sql), so it cannot pass
// against the pre-fix tree.
func TestMigration0113DropsClassicMovements(t *testing.T) {
	root := findRepoRoot(t)

	upPath := filepath.Join(root, "migrations", "0113_drop_classic_movements.up.sql")
	up, err := os.ReadFile(upPath)
	if err != nil {
		t.Fatalf("read 0113 up migration: %v (the C2-18/DAT-03 cleanup migration must exist)", err)
	}
	upSQL := string(up)
	if !strings.Contains(upSQL, "DROP TABLE IF EXISTS classic_movements") {
		t.Errorf("0113 up must drop the dead table; got:\n%s", upSQL)
	}
	// The up path must NOT recreate the table (that belongs in .down.sql).
	if strings.Contains(upSQL, "CREATE TABLE classic_movements") {
		t.Errorf("0113 up must not CREATE classic_movements — only DROP it")
	}

	downPath := filepath.Join(root, "migrations", "0113_drop_classic_movements.down.sql")
	down, err := os.ReadFile(downPath)
	if err != nil {
		t.Fatalf("read 0113 down migration: %v (the drop must be reversible)", err)
	}
	downSQL := string(down)
	// Reversibility: the down recreates the full 0105 schema, not a stub —
	// the table itself, its hypertable, and its compression policy.
	for _, want := range []string{
		"CREATE TABLE classic_movements",
		"create_hypertable(",
		"add_compression_policy(",
	} {
		if !strings.Contains(downSQL, want) {
			t.Errorf("0113 down must restore the 0105 schema; missing %q", want)
		}
	}
}
