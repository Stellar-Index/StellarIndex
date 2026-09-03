package clickhouse

import (
	"strings"
	"testing"
)

// These pin the SQL shape of txOutcomesByHashQuery, in the same
// spirit-and-convention as explorer_scan_settings_test.go: the clause that
// makes this query cheap is invisible at the call site, and losing it is
// silent — the query still compiles, still returns the right rows, and costs
// four orders of magnitude more.
//
// The measured fact these encode (r1, 2026-09-03, cold, three real 50-op
// pages of idle accounts, use_query_condition_cache=0):
//
//	ledger_seq IN (<50 ledgers>)        319k-508k rows /  24-38 MiB /  32-61 ms
//	ledger_seq >= lo AND ledger_seq <= hi  1.69-2.04 BILLION rows / 122-147 GiB / >60s (did not finish)
//
// An operation-list page is not contiguous, so [lo,hi] is not "the page" — it
// is every ledger between the page's oldest and newest op, which for a sparse
// account is millions. The tx_hash bloom_filter(0.01) cannot rescue that: 50
// hash probes false-positive on ~39% of candidate granules. At the explorer's
// own PAGE_SIZE=50 the span form blew stampTxOutcomes' budget on every idle
// account, so 0 of 50 operations got transaction_successful and the whole page
// rendered with an UNKNOWN outcome behind the coverage note (#332 F1).

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
	// FINAL is REQUIRED here, not incidental, and it is deliberately NOT
	// rewritten to `ORDER BY ingested_at DESC LIMIT 1 BY tx_hash`:
	//
	//   - stellar.transactions is ReplacingMergeTree(ingested_at) and the
	//     duplicates are real — r1's partition 63 carries 183.5M duplicated
	//     (ledger_seq, tx_index) key-groups, so without read-time dedup this
	//     serves a duplicate/stale verdict.
	//   - `ingested_at` is DateTime, ONE-SECOND resolution. A re-ingest batch
	//     that rewrites many rows inside one wall-clock second TIES on the
	//     version column, and a bare SELECT cannot break that tie — it would
	//     silently keep serving the STALE pre-fix verdict. That is audit
	//     DAT-10; txByLedgerAndHash documents the same trap on this table.
	//     FINAL breaks the tie on real insertion order.
	//
	// FINAL is only ruinous when the granule selection comes from a SKIP index
	// over a WIDE primary-key range, because PrimaryKeyExpand then re-expands
	// it (measured on the old span form: 32,930 bloom-selected granules
	// expanded back to 74,938). Once ledger_seq is a point set that expansion
	// has nothing to expand, which is what makes keeping FINAL affordable:
	// 32-61 ms with it vs 17-18 ms without, on the same pages.
	if !strings.Contains(txOutcomesByHashQuery, "FINAL") {
		t.Errorf("txOutcomesByHashQuery dropped FINAL — a same-second re-ingest tie "+
			"would now serve the STALE transaction verdict (audit DAT-10):\n%s",
			txOutcomesByHashQuery)
	}
}
