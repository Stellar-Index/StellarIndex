package main

import "testing"

// TestCheckGoSource_MissingGuardFails is the proven-red regression for T351:
// before this lint existed, nothing rejected a writer that upserts a
// derive_generation-carrying table with ON CONFLICT DO UPDATE but omits the
// `WHERE <table>.derive_generation <= EXCLUDED.derive_generation` guard — a
// corrected re-derive would silently no-op against a stale value forever.
func TestCheckGoSource_MissingGuardFails(t *testing.T) {
	tables := map[string]bool{"blend_positions": true}

	const missingGuard = "const q = `\n" +
		"    INSERT INTO blend_positions (pool, ledger, derive_generation)\n" +
		"    VALUES ($1, $2, $3)\n" +
		"    ON CONFLICT (pool, ledger) DO UPDATE SET\n" +
		"        derive_generation = EXCLUDED.derive_generation\n" +
		"`\n"

	failures := checkGoSource("fixture.go", missingGuard, tables)
	if len(failures) != 1 {
		t.Fatalf("checkGoSource on a guardless upsert: got %d failures, want 1 (%v)", len(failures), failures)
	}
}

// TestCheckGoSource_PresentGuardPasses is the green counterpart: the same
// writer, with the guard clause present, must not be flagged.
func TestCheckGoSource_PresentGuardPasses(t *testing.T) {
	tables := map[string]bool{"blend_positions": true}

	const withGuard = "const q = `\n" +
		"    INSERT INTO blend_positions (pool, ledger, derive_generation)\n" +
		"    VALUES ($1, $2, $3)\n" +
		"    ON CONFLICT (pool, ledger) DO UPDATE SET\n" +
		"        derive_generation = EXCLUDED.derive_generation\n" +
		"      WHERE blend_positions.derive_generation <= EXCLUDED.derive_generation\n" +
		"`\n"

	if failures := checkGoSource("fixture.go", withGuard, tables); len(failures) != 0 {
		t.Fatalf("checkGoSource on a guarded upsert: got failures %v, want none", failures)
	}
}

// TestCheckGoSource_UntrackedTableIgnored: a table not in the
// derive_generation set (e.g. one with no such column) is out of scope even
// if it lacks a guard — the lint must not false-positive on ordinary
// append-only upserts.
func TestCheckGoSource_UntrackedTableIgnored(t *testing.T) {
	tables := map[string]bool{"blend_positions": true}

	const otherTable = "const q = `\n" +
		"    INSERT INTO some_other_table (a, b)\n" +
		"    VALUES ($1, $2)\n" +
		"    ON CONFLICT (a) DO UPDATE SET b = EXCLUDED.b\n" +
		"`\n"

	if failures := checkGoSource("fixture.go", otherTable, tables); len(failures) != 0 {
		t.Fatalf("checkGoSource on an untracked table: got failures %v, want none", failures)
	}
}
