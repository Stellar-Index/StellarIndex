package defindex

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var (
	flaggedCensusRow = regexp.MustCompile("(?m)^\\| `(C[A-Z2-7]{55})` \\|[^|]*\\|[^|]*\\| (\\d+) → \\d+ \\|.*FLAGGED")
	strkeyLiteral    = regexp.MustCompile(`'(C[A-Z2-7]{55})'`)
	ledgerLiteral    = regexp.MustCompile(`\b\d{7,}\b`)
)

// RLT-402: the flagged-contract DELETE in defindex.md's operator rollout
// must carry a ledger_close_time bound (defindex_flows is partitioned on
// it) that can never skip a flagged row, and must not take that bound from
// a subquery that can turn NULL and silently match nothing.
func TestDoc_DefindexFlowsDeleteRecipe_RLT402(t *testing.T) {
	t.Parallel()

	doc := readDefindexDoc(t)
	flagged, earliest := flaggedCensus(t, doc)

	del := sqlStatement(t, doc, "DELETE FROM defindex_flows")
	if !strings.Contains(del, "ledger_close_time >=") {
		t.Error("defindex_flows DELETE has no ledger_close_time bound: no chunk exclusion")
	}
	if strings.Contains(strings.ToUpper(del), "SELECT") {
		t.Error("defindex_flows DELETE takes its bound from a subquery; a NULL result " +
			"matches nothing and still reports success — use a checked literal")
	}
	for _, m := range ledgerLiteral.FindAllString(del, -1) {
		n, _ := strconv.Atoi(m)
		if n > earliest {
			t.Errorf("defindex_flows DELETE is bounded at ledger %d, after the earliest "+
				"flagged census row at %d: those rows would be left behind", n, earliest)
		}
	}
	if got := contractSet(del); !equalSets(got, flagged) {
		t.Errorf("DELETE contract list %v != census FLAGGED set %v", got, flagged)
	}

	pre := sqlStatement(t, doc, "SELECT count(*), min(ledger), min(ledger_close_time)\n     FROM defindex_flows")
	if got := contractSet(pre); !equalSets(got, flagged) {
		t.Errorf("pre-count contract list %v != census FLAGGED set %v", got, flagged)
	}
	if !strings.Contains(doc, "DELETE n must equal step 1's count") {
		t.Error("recipe no longer tells the operator to check the DELETE count")
	}
}

func flaggedCensus(t *testing.T, doc string) ([]string, int) {
	t.Helper()
	earliest := 0
	var ids []string
	for _, m := range flaggedCensusRow.FindAllStringSubmatch(doc, -1) {
		ids = append(ids, m[1])
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("census ledger %q: %v", m[2], err)
		}
		if earliest == 0 || n < earliest {
			earliest = n
		}
	}
	if len(ids) == 0 {
		t.Fatal("no FLAGGED rows parsed from the census table; this test asserts nothing")
	}
	sort.Strings(ids)
	return ids, earliest
}

func sqlStatement(t *testing.T, doc, start string) string {
	t.Helper()
	i := strings.Index(doc, start)
	if i < 0 {
		t.Fatalf("defindex.md: %q not found", start)
	}
	j := strings.Index(doc[i:], ";")
	if j < 0 {
		t.Fatalf("defindex.md: %q has no terminating ';'", start)
	}
	return doc[i : i+j]
}

func contractSet(stmt string) []string {
	var ids []string
	for _, m := range strkeyLiteral.FindAllStringSubmatch(stmt, -1) {
		ids = append(ids, m[1])
	}
	sort.Strings(ids)
	return ids
}

func equalSets(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}
