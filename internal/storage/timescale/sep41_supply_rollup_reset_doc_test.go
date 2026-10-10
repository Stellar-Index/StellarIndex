package timescale

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSEP41SupplyRollupMigrationsNeverPrescribeDestructiveReset pins the
// migrations' own guidance to the non-destructive reset. Migration
// 0085's header told operators a below-checkpoint re-derive "requires a
// `TRUNCATE sep41_supply_rollup`", which also wipes migration 0088's seeded
// genesis baseline and silently under-reports supply.
func TestSEP41SupplyRollupMigrationsNeverPrescribeDestructiveReset(t *testing.T) {
	paths, err := filepath.Glob("../../../migrations/*.sql")
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob migrations: %d files, err=%v", len(paths), err)
	}
	destructive := regexp.MustCompile("(?i)(TRUNCATE\\s+(TABLE\\s+)?|DELETE\\s+FROM\\s+)`?sep41_supply_rollup")
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if loc := destructive.FindIndex(raw); loc != nil {
			t.Errorf("%s: %q — never TRUNCATE/DELETE sep41_supply_rollup; it drops the migration-0088 "+
				"genesis baseline. Reset the fold via Store.ResetSEP41SupplyRollupFold instead",
				filepath.Base(p), raw[loc[0]:loc[1]])
		}
	}

	header, err := os.ReadFile("../../../migrations/0085_create_sep41_supply_rollup.up.sql")
	if err != nil {
		t.Fatalf("read migration 0085: %v", err)
	}
	if !strings.Contains(string(header), "ResetSEP41SupplyRollupFold") {
		t.Error("migration 0085's header no longer names ResetSEP41SupplyRollupFold as the below-checkpoint reset")
	}
}
