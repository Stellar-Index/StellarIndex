package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func writeMigrations(t *testing.T, files map[string]string) []string {
	t.Helper()
	dir := t.TempDir()
	var out []string
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func TestLegacyBackfillViolations_Fixtures(t *testing.T) {
	const backfill = "-- DELETE FROM x; in a comment is not a wipe\nALTER TABLE x ADD COLUMN IF NOT EXISTS event_index integer NOT NULL DEFAULT 0;\n"
	cases := []struct {
		name     string
		files    map[string]string
		wantFail bool
	}{
		{"backfill with no wipe", map[string]string{"0001_a.up.sql": backfill}, true},
		{"wipe in a later migration", map[string]string{"0001_a.up.sql": backfill, "0002_b.up.sql": "SELECT decompress_chunk(c, true) FROM show_chunks('x') c;\nDELETE FROM x;\n"}, false},
		{"wipe in the same migration", map[string]string{"0001_a.up.sql": backfill + "TRUNCATE TABLE x;\n"}, false},
		{"partial delete is not a wipe", map[string]string{"0001_a.up.sql": backfill, "0002_b.up.sql": "DELETE FROM x WHERE event_index = 0;\n"}, true},
		{"wipe before the backfill does not count", map[string]string{"0001_a.up.sql": "DELETE FROM x;\n", "0002_b.up.sql": backfill}, true},
		{"multi-line ALTER", map[string]string{"0001_a.up.sql": "ALTER TABLE x\n    ADD COLUMN event_index integer NOT NULL DEFAULT 0 CHECK (event_index >= 0);\n"}, true},
		{"no DEFAULT is not a backfill", map[string]string{"0001_a.up.sql": "ALTER TABLE x ADD COLUMN event_index integer;\n"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fail, _, err := legacyBackfillViolations(writeMigrations(t, tc.files), []string{"x"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(fail) > 0; got != tc.wantFail {
				t.Fatalf("failed=%v, want %v (%v)", got, tc.wantFail, fail)
			}
		})
	}
}

// TestLegacyBackfillViolations_RealTree pins the lint against the shipped
// migrations: phoenix's 0060 backfill is flagged unless its TODO entry
// carries it, and the disarmed comet (0137) and cctp/rozo (0164) tables are
// not.
func TestLegacyBackfillViolations_RealTree(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join("..", "..", "..", "migrations", "*.up.sql"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(entries)

	fail, stale, err := legacyBackfillViolations(entries, protocolRowTables, legacyBackfill)
	if err != nil {
		t.Fatal(err)
	}
	if len(fail) > 0 || len(stale) > 0 {
		t.Fatalf("shipped allowlist does not match the tree: fail=%v stale=%v", fail, stale)
	}

	without := map[string]string{}
	for k, v := range legacyBackfill {
		if !strings.HasPrefix(k, "phoenix_") {
			without[k] = v
		}
	}
	fail, _, err = legacyBackfillViolations(entries, protocolRowTables, without)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(fail, "\n")
	for _, want := range []string{"phoenix_liquidity", "phoenix_stake_events", "0060_phoenix_event_index.up.sql"} {
		if !strings.Contains(joined, want) {
			t.Errorf("un-disarmed 0060 backfill not flagged: missing %q in %v", want, fail)
		}
	}
	for _, disarmed := range []string{"comet_liquidity", "cctp_events", "rozo_events"} {
		if strings.Contains(joined, disarmed) {
			t.Errorf("%s was disarmed by a whole-table DELETE but is still flagged: %v", disarmed, fail)
		}
	}
	if len(fail) != 2 {
		t.Errorf("want exactly the two phoenix tables flagged, got %d: %v", len(fail), fail)
	}
}
