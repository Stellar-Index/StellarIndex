package controlwiring

import (
	"strings"
	"testing"
)

// GH-1347: the only documentation of how to retire a data source
// without the watchdog alarming on it forever was a comment inside
// data-freshness.sh — unreachable from the ticket the alert raises. This
// pins the runbook's "Retiring a source" section (both exit mechanisms)
// and the alert annotation that links to it, so either regressing away.
const (
	dataSourceStaleRunbookPath = "docs/operations/runbooks/data-freshness.md"
	dataFreshnessRulesPath     = "configs/prometheus/rules.r1/data-freshness.yml"
)

func TestDataSourceStaleRunbook_DocumentsRetiringASource(t *testing.T) {
	t.Parallel()
	runbook := readRepoFile(t, dataSourceStaleRunbookPath)

	_, after, ok := strings.Cut(runbook, "## Retiring a source")
	if !ok {
		t.Fatal(dataSourceStaleRunbookPath + " has no '## Retiring a source' section")
	}
	section, _, _ := strings.Cut(after, "\n## ")
	section = strings.Join(strings.Fields(section), " ")

	// Both exit mechanisms data-freshness.sh's own comment names
	// (configs/ansible/roles/archival-node/files/data-freshness.sh)
	// must be spelled out, not merely alluded to.
	for _, want := range []string{
		"Delete the source's rows",
		"Exclude it in the SQL",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("%s: 'Retiring a source' section does not mention %q — "+
				"an operator following it is missing one of the two ways out",
				dataSourceStaleRunbookPath, want)
		}
	}
	// The section must warn against the operationally tempting shortcut
	// that hides OTHER sources' real outages.
	if !strings.Contains(section, "silence") {
		t.Errorf("%s: 'Retiring a source' section does not warn against silencing the alert at Alertmanager",
			dataSourceStaleRunbookPath)
	}
}

func TestDataFreshnessAlert_LinksToRetiringASource(t *testing.T) {
	t.Parallel()
	rules := readRepoFile(t, dataFreshnessRulesPath)

	_, after, ok := strings.Cut(rules, "- alert: stellarindex_data_source_stale")
	if !ok {
		t.Fatal(dataFreshnessRulesPath + " has no 'stellarindex_data_source_stale' alert")
	}
	block, _, _ := strings.Cut(after, "\n      - alert:")

	if !strings.Contains(block, "data-freshness.md#retiring-a-source") {
		t.Errorf("%s: stellarindex_data_source_stale's annotations do not link "+
			"to the runbook's 'Retiring a source' section — a retired source's "+
			"permanent ticket has no path back to the fix (#1347)", dataFreshnessRulesPath)
	}
}
