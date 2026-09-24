package migrations

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestWebhookDeliveriesRunbookColumnsExist guards RLT-226: the
// customer-webhook-delivery-failing runbook's `_mark_errors` diagnostic
// query named columns webhook_deliveries has never had (`updated_at`).
// The table (0027_platform_v1_schema.up.sql) is the schema of record; no
// later migration adds an updated_at column. This parses both and fails
// if the runbook ever references a column the table doesn't declare.
func TestWebhookDeliveriesRunbookColumnsExist(t *testing.T) {
	cols := webhookDeliveriesColumns(t)
	query := webhookDeliveriesMarkErrorsQuery(t)

	for _, col := range queryColumnRefs(query) {
		if !cols[col] {
			t.Errorf("runbook _mark_errors query references webhook_deliveries.%s, which 0027_platform_v1_schema.up.sql does not declare (columns: %v)", col, sortedKeys(cols))
		}
	}
}

// webhookDeliveriesColumns parses the CREATE TABLE webhook_deliveries
// block in 0027 and returns its declared column names.
func webhookDeliveriesColumns(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("0027_platform_v1_schema.up.sql")
	if err != nil {
		t.Fatalf("read 0027_platform_v1_schema.up.sql: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "CREATE TABLE webhook_deliveries (")
	if start == -1 {
		t.Fatal("0027_platform_v1_schema.up.sql has no `CREATE TABLE webhook_deliveries (` — update this test's anchor")
	}
	end := strings.Index(src[start:], ");")
	if end == -1 {
		t.Fatal("webhook_deliveries CREATE TABLE block has no closing `);`")
	}
	block := src[start : start+end]

	cols := map[string]bool{}
	colLine := regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\s+\S`)
	for _, line := range strings.Split(block, "\n")[1:] {
		if m := colLine.FindStringSubmatch(line); m != nil {
			cols[m[1]] = true
		}
	}
	if len(cols) == 0 {
		t.Fatal("parsed zero columns from webhook_deliveries — the anchor/regex drifted from the SQL shape")
	}
	return cols
}

// webhookDeliveriesMarkErrorsQuery returns the SELECT ... ORDER BY body
// of the `_mark_errors` diagnostic query in the runbook.
func webhookDeliveriesMarkErrorsQuery(t *testing.T) string {
	t.Helper()
	path := "../docs/operations/runbooks/customer-webhook-delivery-failing.md"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(raw)
	start := strings.Index(src, "SELECT id, webhook_id, event_type, attempt_count, next_attempt_at")
	if start == -1 {
		t.Fatalf("%s has no `_mark_errors` SELECT on webhook_deliveries — update this test's anchor", path)
	}
	end := strings.Index(src[start:], `LIMIT 20"`)
	if end == -1 {
		t.Fatalf("%s: `_mark_errors` query has no `LIMIT 20\"` terminator", path)
	}
	return src[start : start+end]
}

// queryColumnRefs extracts bare column names from a SELECT ... ORDER BY
// query's select-list and ORDER BY clause (the two clauses RLT-226's
// stale `updated_at` reference could hide in).
func queryColumnRefs(query string) []string {
	var refs []string

	selectList := query
	if i := strings.Index(query, "FROM"); i != -1 {
		selectList = query[len("SELECT"):i]
	}
	for _, col := range strings.Split(selectList, ",") {
		col = strings.TrimSpace(col)
		if col != "" {
			refs = append(refs, col)
		}
	}

	if i := strings.Index(query, "ORDER BY"); i != -1 {
		orderBy := strings.TrimSpace(query[i+len("ORDER BY"):])
		orderBy = strings.TrimSuffix(strings.TrimSpace(orderBy), "DESC")
		refs = append(refs, strings.TrimSpace(orderBy))
	}
	return refs
}

func sortedKeys(m map[string]bool) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
