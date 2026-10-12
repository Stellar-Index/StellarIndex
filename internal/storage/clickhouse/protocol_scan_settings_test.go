package clickhouse

import (
	"strings"
	"testing"
)

// Pins the two clauses that make the RAW protocol-analytics reads survivable (losing either is silent in
// review and unit tests): cold over 90 days on a busy contract each read scans ~1B rows (breakdown ~373
// GiB) against max_execution_time = 30, and all three run concurrently per page build.

// protocolRawScanQueries is every raw (non-pre-aggregated) protocol-analytics
// query, by the name of the reader that issues it.
func protocolRawScanQueries() map[string]string {
	return map[string]string{
		"protocolEventBreakdownQuery(windowed)": protocolEventBreakdownQuery(true),
		"protocolEventBreakdownQuery(all-time)": protocolEventBreakdownQuery(false),
		"protocolDailyActivityQuery":            protocolDailyActivityQuery,
		"protocolContractActivityQuery":         protocolContractActivityQuery,
	}
}

// TestProtocolRawScanQueries_CarryRowCeiling — the ceiling turns a scan the connection's own 30s
// limit would kill anyway into an immediate refusal. Without it a single protocol page build
// spends hundreds of GiB and ~250 threads to produce nothing, starving the customer API.
func TestProtocolRawScanQueries_CarryRowCeiling(t *testing.T) {
	for name, q := range protocolRawScanQueries() {
		if !strings.Contains(q, "max_rows_to_read = 600000000") {
			t.Errorf("%s lost its row ceiling — a busy protocol's 90-day window "+
				"(1.09 BILLION rows) would again be read to death by a connection "+
				"pinned to max_execution_time=30:\n%s", name, q)
		}
		// The mode is the load-bearing half: 'throw' REFUSES the read so the
		// caller degrades honestly. Any truncating mode ('break') would serve a
		// silently short protocol event count as if it were complete.
		if !strings.Contains(q, "read_overflow_mode = 'throw'") {
			t.Errorf("%s must REFUSE (throw), never truncate — a truncated count "+
				"is served to the caller as a complete one:\n%s", name, q)
		}
	}
}

// TestProtocolRawScanQueries_KeepFinal asserts FINAL is PRESENT: contract_events has large unmerged
// duplicates, so dropping FINAL overstates event counts and saves nothing on a busy contract.
func TestProtocolRawScanQueries_KeepFinal(t *testing.T) {
	for name, q := range protocolRawScanQueries() {
		if !strings.Contains(q, "stellar.contract_events FINAL") {
			t.Errorf("%s dropped FINAL — the un-merged ReplacingMergeTree duplicates "+
				"would overcount this protocol's events by 45-94%% (measured on r1):\n%s",
				name, q)
		}
	}
}

// TestProtocolEventBreakdownQuery_WindowIsOptionalAndBound pins that the windowed arm carries the ledger
// bound (pruning to the 90-day working set) and the all-time arm does not, else args and placeholders
// mismatch.
func TestProtocolEventBreakdownQuery_WindowIsOptionalAndBound(t *testing.T) {
	if got := protocolEventBreakdownQuery(true); !strings.Contains(got, "ledger_seq >= ?") {
		t.Errorf("windowed breakdown lost its ledger bound — it would scan the whole "+
			"12.9B-row lake instead of the caller's 90-day window:\n%s", got)
	}
	if got := protocolEventBreakdownQuery(false); strings.Contains(got, "ledger_seq >= ?") {
		t.Errorf("all-time breakdown grew a ledger placeholder with no arg to bind it:\n%s", got)
	}
}

// TestProtocolContractActivityQuery_ExcludesPartialDay: the roster reader must share the complete-days-only
// bound of its sibling queries, or the still-accumulating current day makes the count wobble on every read.
func TestProtocolContractActivityQuery_ExcludesPartialDay(t *testing.T) {
	if !strings.Contains(protocolContractActivityQuery, "close_time < toStartOfDay(now())") {
		t.Errorf("protocolContractActivityQuery lost its complete-days-only bound — "+
			"the per-contract roster would include the current partial day, "+
			"unlike protocolEventBreakdownQuery and protocolDailyActivityQuery:\n%s",
			protocolContractActivityQuery)
	}
}

// TestProtocolRawScanQueries_NoExplorerScanSettings guards against a measured-wrong fix: adding
// explorerScanSettings (max_threads = 4) ran nearly 2x slower for identical I/O, as its byte fan-out is a
// ledger_entries_current part-layout effect.
func TestProtocolRawScanQueries_NoExplorerScanSettings(t *testing.T) {
	for name, q := range protocolRawScanQueries() {
		if strings.Contains(q, "max_threads = 4") {
			t.Errorf("%s pinned max_threads=4: measured on r1 that makes this shape "+
				"~2x SLOWER (105,649ms vs 58,460ms) for identical bytes read:\n%s", name, q)
		}
	}
}
