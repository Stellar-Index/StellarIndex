//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TestAccountBoardKeyedReadsPruneOnTheSkipIndex: the creator and sponsor
// boards are ORDER BY rank for their top-N page, so the ?account= read
// (WHERE creator|sponsor = ?) has no primary-key help. Both halves of each
// EXCHANGE pair must carry the bloom_filter skip index, and on a multi-granule
// board the planner must actually drop granules with it rather than reading
// the whole table to return at most one row.
func TestAccountBoardKeyedReadsPruneOnTheSkipIndex(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conn := dialClickHouse(t, ctx, "stellar")

	for _, tc := range []struct{ table, col, index string }{
		{"account_creators_rollup", "creator", "idx_creators_rollup_creator"},
		{"account_sponsors_rollup", "sponsor", "idx_sponsors_rollup_sponsor"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			for _, table := range []string{tc.table, tc.table + "_staging"} {
				var idxType, idxExpr string
				if err := conn.QueryRow(ctx, `SELECT type, expr FROM system.data_skipping_indices
					WHERE database = 'stellar' AND table = ? AND name = ?`, table, tc.index).
					Scan(&idxType, &idxExpr); err != nil {
					t.Fatalf("stellar.%s has no skip index %s — the keyed board read full-scans: %v", table, tc.index, err)
				}
				if idxType != "bloom_filter" || idxExpr != tc.col {
					t.Fatalf("stellar.%s %s = %s(%s), want bloom_filter(%s)", table, tc.index, idxType, idxExpr, tc.col)
				}
			}

			// A private clone inherits the definition without disturbing the
			// live board other tests read; 40,000 rows is five granules.
			probe := "stellar." + tc.table + "_keyed_probe"
			for _, q := range []string{
				`DROP TABLE IF EXISTS ` + probe,
				`CREATE TABLE ` + probe + ` AS stellar.` + tc.table,
				fmt.Sprintf(`INSERT INTO %s (rank, %s)
					SELECT toUInt32(number + 1), concat('GKEYEDPROBE', toString(number)) FROM numbers(40000)`, probe, tc.col),
			} {
				if err := conn.Exec(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			t.Cleanup(func() { _ = conn.Exec(context.Background(), `DROP TABLE IF EXISTS `+probe) }) //nolint:contextcheck // cleanup outlives the test context

			plan := explain(t, ctx, conn, fmt.Sprintf(`EXPLAIN indexes = 1 SELECT count() FROM %s
				WHERE %s = 'GKEYEDPROBE31000'`, probe, tc.col))
			selected, initial := skipIndexGranules(t, plan, tc.index)
			if initial < 2 || selected >= initial {
				t.Fatalf("%s dropped no granules (%d/%d) for a one-row keyed read:\n%s", tc.index, selected, initial, plan)
			}
		})
	}
}

// TestAccountBoardIndexUpgradeAppliesToALegacyBoard executes the operator
// files' own ALTER statements against boards shaped like an existing host's
// (created before the index, so IF NOT EXISTS left them unindexed) and
// requires the index to land on both halves of the EXCHANGE pair, twice over.
func TestAccountBoardIndexUpgradeAppliesToALegacyBoard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conn := dialClickHouse(t, ctx, "stellar")
	_, thisFile, _, _ := runtime.Caller(0)

	for _, tc := range []struct{ file, table, index string }{
		{"account_creators_rollup.sql", "account_creators_rollup", "idx_creators_rollup_creator"},
		{"account_sponsors_rollup.sql", "account_sponsors_rollup", "idx_sponsors_rollup_sponsor"},
	} {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "clickhouse", tc.file))
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		alters := map[string]string{}
		for _, stmt := range splitSQLStatements(string(raw)) {
			if f := strings.Fields(stmt); len(f) > 2 && f[0] == "ALTER" && f[1] == "TABLE" {
				alters[strings.TrimPrefix(f[2], "stellar.")] = stmt
			}
		}
		for _, target := range []string{tc.table, tc.table + "_staging"} {
			stmt, ok := alters[target]
			if !ok {
				t.Fatalf("%s carries no ALTER for stellar.%s — an existing host keeps an unindexed board", tc.file, target)
			}
			legacy := target + "_legacy_probe"
			for _, q := range []string{
				`DROP TABLE IF EXISTS stellar.` + legacy,
				`CREATE TABLE stellar.` + legacy + ` AS stellar.` + target,
				`ALTER TABLE stellar.` + legacy + ` DROP INDEX ` + tc.index,
			} {
				if err := conn.Exec(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			t.Cleanup(func() { _ = conn.Exec(context.Background(), `DROP TABLE IF EXISTS stellar.`+legacy) })
			if n := skipIndexCount(t, ctx, conn, legacy, tc.index); n != 0 {
				t.Fatalf("legacy probe stellar.%s still has %s", legacy, tc.index)
			}
			rewritten := strings.Replace(stmt, "stellar."+target, "stellar."+legacy, 1)
			for range 2 {
				if err := conn.Exec(ctx, rewritten); err != nil {
					t.Fatalf("%s's ALTER for %s failed on a legacy board: %v\n%s", tc.file, target, err, rewritten)
				}
			}
			if n := skipIndexCount(t, ctx, conn, legacy, tc.index); n != 1 {
				t.Fatalf("after %s's ALTER, stellar.%s has %d %s indices, want 1", tc.file, legacy, n, tc.index)
			}
		}
	}
}

func skipIndexCount(t *testing.T, ctx context.Context, conn driver.Conn, table, index string) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM system.data_skipping_indices
		WHERE database = 'stellar' AND table = ? AND name = ?`, table, index).Scan(&n); err != nil {
		t.Fatalf("count %s on %s: %v", index, table, err)
	}
	return n
}
