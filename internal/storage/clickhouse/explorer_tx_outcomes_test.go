package clickhouse

import (
	"strings"
	"testing"
)

// These pin the SQL shape of txOutcomesByHashQuery, as explorer_scan_settings_test.go does: the
// clause that makes it cheap is invisible at the call site, and losing it is silent (same rows,
// orders of magnitude more scanned).
//
// An operation-list page is not contiguous, so a [lo,hi] ledger span is every ledger between the
// page's oldest and newest op, millions for a sparse account (over a billion rows scanned versus
// about 500k for ledger_seq IN (<the page's ledgers>)). The tx_hash bloom filter cannot rescue
// the span: 50 hash probes false-positive on a large share of granules. The span form blew
// stampTxOutcomes' budget on idle accounts, so the whole page rendered with an UNKNOWN outcome.

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
	// FINAL is required, and deliberately not rewritten to ORDER BY ingested_at DESC LIMIT 1 BY:
	//
	//   - stellar.transactions is ReplacingMergeTree(ingested_at) and holds real duplicates, so
	//     without read-time dedup this serves a duplicate or stale verdict.
	//   - ingested_at is DateTime with one-second resolution: a re-ingest batch rewriting many
	//     rows inside one second ties on the version column, and a bare SELECT cannot break the
	//     tie (txByLedgerAndHash documents the same trap). FINAL uses real insertion order.
	//
	// FINAL is only ruinous when a skip index over a wide primary-key range picks the granules,
	// because PrimaryKeyExpand re-expands them. With ledger_seq a point set there is nothing to
	// expand, which keeps FINAL affordable.
	if !strings.Contains(txOutcomesByHashQuery, "FINAL") {
		t.Errorf("txOutcomesByHashQuery dropped FINAL — a same-second re-ingest tie "+
			"would now serve the STALE transaction verdict (audit DAT-10):\n%s",
			txOutcomesByHashQuery)
	}
}
