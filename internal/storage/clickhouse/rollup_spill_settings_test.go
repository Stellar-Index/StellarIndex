package clickhouse

import (
	"strings"
	"testing"
	"time"
)

// TestRollupStatementsKeepTheSpillValve refuses a statement that sets both
// max_bytes_before_external_group_by and optimize_aggregation_in_order = 1.
//
// The two cancel: in-order aggregation cannot spill to disk, so it disables the external
// group-by limit beside it and turns an aggregation that would have spilled into a hard
// max_memory_usage failure. ClickHouse accepts both settings and reports nothing. The rule is
// general, so any rollup step that pairs them fails here rather than at runtime.
func TestRollupStatementsKeepTheSpillValve(t *testing.T) {
	lists := map[string][]string{}
	for _, s := range creatorsRollupStatements(P23BoundaryLedger) {
		lists["creators"] = append(lists["creators"], s.sql)
	}
	for _, s := range sponsorsRollupStatements {
		lists["sponsors"] = append(lists["sponsors"], s.sql)
	}
	lists["holders"] = append(lists["holders"], holdersRollupStatements(time.Now())...)

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
