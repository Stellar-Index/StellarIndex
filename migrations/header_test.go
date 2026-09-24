package migrations

import (
	"os"
	"strings"
	"testing"
)

// TestMigration0150HeaderNamesPopulatedNodeApply guards 0150's header
// against dropping the operator step for its bare in-transaction
// trades_signer_idx build: the up body is immutable (README "Amending a
// shipped migration"), so the header and register row are the only place
// a populated-node operator learns that a pre-build fails the migration (T444).
func TestMigration0150HeaderNamesPopulatedNodeApply(t *testing.T) {
	header := upHeader(t, "0150_add_trades_signer.up.sql")
	for _, want := range []string{
		"APPLYING TO AN ALREADY-POPULATED DATABASE",
		"trades_signer_idx",
		"no `IF NOT EXISTS`",
		"Do NOT pre-build",
		"CONCURRENTLY",
		"transaction_per_chunk",
		"pg_stat_activity",
		"ingest writers stopped",
	} {
		if !strings.Contains(header, want) {
			t.Errorf("0150 up header missing %q — the populated-node apply note for trades_signer_idx is incomplete", want)
		}
	}
	row := extractRow(t, readReadme(t), "0150")
	for _, want := range []string{"Do NOT pre-build", "ingest writers stopped", "pg_stat_activity"} {
		if !strings.Contains(row, want) {
			t.Errorf("README 0150 register row missing %q — it must agree with the file header", want)
		}
	}
}

// upHeader returns the comment block above a migration's first `BEGIN;`.
func upHeader(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	idx := strings.Index(string(raw), "\nBEGIN;")
	if idx == -1 {
		t.Fatalf("%s has no BEGIN; line — update this test's anchor", name)
	}
	return string(raw[:idx])
}
