package timescale

import (
	"strings"
	"testing"
)

// RLT-402: the trades delete-then-replay recipe in adr-0033-data-recovery.md
// must keep `ledger` as its correctness predicate, add a `ts` bound for
// chunk exclusion (trades is partitioned on ts, migrations/0001), and never
// take that bound from a subquery that can turn NULL and match nothing.
func TestDoc_TradesDeleteRecipe_RLT402(t *testing.T) {
	t.Parallel()

	doc := readRepoFile(t, "docs/operations/adr-0033-data-recovery.md")
	const ledgerPred = "AND ledger BETWEEN <from> AND <to>"

	del := docStatement(t, doc, "DELETE FROM trades")
	for _, want := range []string{ledgerPred, "AND ts >= '", "AND ts <= '"} {
		if !strings.Contains(del, want) {
			t.Errorf("trades DELETE is missing %q", want)
		}
	}
	if strings.Contains(strings.ToUpper(del), "SELECT") {
		t.Error("trades DELETE takes its ts bound from a subquery; a missing " +
			"ledger_ingest_log row makes it NULL, the DELETE matches nothing and " +
			"still reports success — use a checked literal")
	}

	endpoints := docStatement(t, doc, "SELECT ledger_seq, ledger_close_time")
	if !strings.Contains(endpoints, "ledger_seq IN (<from>, <to>)") {
		t.Error("recipe no longer resolves both ledger_ingest_log endpoint rows")
	}
	count := docStatement(t, doc, "SELECT count(*) FROM trades")
	if !strings.Contains(count, ledgerPred) || strings.Contains(count, "ts ") {
		t.Error("pre-count must use the ledger predicate alone, so it catches a wrong ts bound")
	}
	if !strings.Contains(doc, "DELETE n must equal step 2's count") {
		t.Error("recipe no longer tells the operator to check the DELETE count")
	}
}

func docStatement(t *testing.T, doc, start string) string {
	t.Helper()
	i := strings.Index(doc, start)
	if i < 0 {
		t.Fatalf("adr-0033-data-recovery.md: %q not found", start)
	}
	j := strings.Index(doc[i:], ";")
	if j < 0 {
		t.Fatalf("adr-0033-data-recovery.md: %q has no terminating ';'", start)
	}
	return doc[i : i+j]
}
