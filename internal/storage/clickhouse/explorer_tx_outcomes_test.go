package clickhouse

import (
	"strings"
	"testing"
)

// These pin the SQL shape of txOutcomesByHashQuery: it must filter by `ledger_seq IN (<page's ledgers>)`, not
// a [lo,hi] span, which scans billions of rows for a sparse account and blows the outcome-stamping budget.

func TestTxOutcomesByHashQuery_PrunesOnTheExactLedgerSet(t *testing.T) {
	q := txOutcomesByHashQuery

	// ledger_seq is the leading primary-key column AND the partition key, so
	// an IN-set of the page's exact ledgers prunes to that many point ranges.
	if !strings.Contains(q, "ledger_seq IN (?)") {
		t.Errorf("txOutcomesByHashQuery must prune on the exact ledger IN-set:\n%s", q)
	}

	// The regression guard: a [lo,hi] span over a non-contiguous page is the
	// defect, and it reads as a perfectly reasonable predicate in review.
	for _, banned := range []string{"ledger_seq >=", "ledger_seq <=", "BETWEEN"} {
		if strings.Contains(q, banned) {
			t.Errorf("txOutcomesByHashQuery reintroduced a ledger SPAN predicate %q — "+
				"cost then scales with the account's idleness, not the page size:\n%s", banned, q)
		}
	}

	// The hash filter still rides the tx_hash bloom skip-index within the
	// (now tiny) surviving granule set.
	if !strings.Contains(q, "tx_hash IN (?)") {
		t.Errorf("txOutcomesByHashQuery lost its tx_hash filter:\n%s", q)
	}
}

func TestTxOutcomesByHashQuery_KeepsFinal(t *testing.T) {
	// FINAL is required, not ORDER BY ingested_at DESC LIMIT 1 BY: transactions hold real duplicates, and
	// ingested_at has one-second resolution so a re-ingest batch ties on the version column.
	// FINAL is affordable here because ledger_seq is a point set, so there are no granules to re-expand.
	if !strings.Contains(txOutcomesByHashQuery, "FINAL") {
		t.Errorf("txOutcomesByHashQuery dropped FINAL — a same-second re-ingest tie "+
			"would now serve the STALE transaction verdict (audit DAT-10):\n%s",
			txOutcomesByHashQuery)
	}
}
