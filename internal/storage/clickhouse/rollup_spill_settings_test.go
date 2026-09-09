package clickhouse

import (
	"strings"
	"testing"
)

// TestRollupStatementsKeepTheSpillValve refuses a statement that sets
// both max_bytes_before_external_group_by and
// optimize_aggregation_in_order = 1.
//
// The two cancel: in-order aggregation cannot spill to disk, so setting
// it disables the external-group-by limit sitting beside it and turns an
// aggregation that would have gone to disk into a hard
// max_memory_usage failure. Nothing reports the conflict — ClickHouse
// accepts both settings and simply never spills.
//
// This is not theoretical. The creators rollup's edge step carried both
// from the day it shipped and died at step 10/12 with "Query memory
// limit exceeded: would use 8.00 GiB" on the first cycle after deploy
// (2026-09-09), leaving stellar.account_creator_edges empty and
// /v1/accounts/{g}/graph serving its warming 503. Measured on r1: with
// the setting removed, the same 24.8 M-row aggregation completes inside
// a 512 MiB budget.
//
// The rule is general, so the test is too: any future step that pairs
// them fails here rather than on r1 at 3 a.m.
func TestRollupStatementsKeepTheSpillValve(t *testing.T) {
	lists := map[string][]string{}
	for _, s := range creatorsRollupStatements {
		lists["creators"] = append(lists["creators"], s.sql)
	}
	for _, s := range sponsorsRollupStatements {
		lists["sponsors"] = append(lists["sponsors"], s.sql)
	}
	lists["holders"] = append(lists["holders"], holdersRollupStatements...)

	checked := 0
	for name, stmts := range lists {
		for i, sql := range stmts {
			lower := strings.ToLower(sql)
			if !strings.Contains(lower, "max_bytes_before_external_group_by") {
				continue
			}
			checked++
			if !strings.Contains(lower, "optimize_aggregation_in_order") {
				continue
			}
			// Only `= 1` conflicts; an explicit 0 is the safe pin.
			norm := strings.NewReplacer(" ", "", "\t", "", "\n", "").Replace(lower)
			if strings.Contains(norm, "optimize_aggregation_in_order=1") {
				t.Errorf("%s rollup step %d sets optimize_aggregation_in_order = 1 beside "+
					"max_bytes_before_external_group_by: in-order aggregation cannot spill, so the "+
					"external-group-by limit is silently cancelled and the step fails at "+
					"max_memory_usage instead of going to disk", name, i+1)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no rollup statement carries max_bytes_before_external_group_by — " +
			"this test would pass vacuously; the settings constants have moved")
	}
	t.Logf("checked %d statement(s) carrying an external-group-by limit", checked)
}
