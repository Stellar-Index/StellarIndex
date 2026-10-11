package clickhouse

import (
	"strings"
	"testing"
)

// These tests pin the two clauses that make the RAW protocol-analytics reads survivable, in the
// same convention as explorer_scan_settings_test.go: losing either is silent in review and in
// unit tests, and expensive only in production.
//
// Cold, over a 90-day window on a busy contract, each read scans about a billion rows (the
// breakdown reads about 373 GiB), against a connection that pins max_execution_time = 30. The
// breakdown cannot complete, and all three run CONCURRENTLY per page build.

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

// TestProtocolRawScanQueries_KeepFinal — asserts FINAL is PRESENT.
//
// Deliberately the opposite of what a "FINAL is slow" reading suggests.
// stellar.contract_events is ReplacingMergeTree(ingested_at) and its unmerged duplicates are
// large (tens of percent over a 90-day window). Dropping FINAL would overstate every protocol's
// headline event count, and on a BUSY contract it buys nothing anyway: the bloom prunes little
// there, so FINAL's PrimaryKeyExpand re-expands no extra granules.
func TestProtocolRawScanQueries_KeepFinal(t *testing.T) {
	for name, q := range protocolRawScanQueries() {
		if !strings.Contains(q, "stellar.contract_events FINAL") {
			t.Errorf("%s dropped FINAL — the un-merged ReplacingMergeTree duplicates "+
				"would overcount this protocol's events by 45-94%% (measured on r1):\n%s",
				name, q)
		}
	}
}

// TestProtocolEventBreakdownQuery_WindowIsOptionalAndBound — the windowed arm
// must carry the ledger bound (it is what prunes partitions to the 90-day
// working set), and the all-time arm must NOT, since it binds no such arg.
// Getting this backwards is an argument/placeholder mismatch, not a slow query.
func TestProtocolEventBreakdownQuery_WindowIsOptionalAndBound(t *testing.T) {
	if got := protocolEventBreakdownQuery(true); !strings.Contains(got, "ledger_seq >= ?") {
		t.Errorf("windowed breakdown lost its ledger bound — it would scan the whole "+
			"12.9B-row lake instead of the caller's 90-day window:\n%s", got)
	}
	if got := protocolEventBreakdownQuery(false); strings.Contains(got, "ledger_seq >= ?") {
		t.Errorf("all-time breakdown grew a ledger placeholder with no arg to bind it:\n%s", got)
	}
}

// TestProtocolContractActivityQuery_ExcludesPartialDay — the per-contract
// roster reader must share the complete-days-only bound its siblings (protocolEventBreakdownQuery,
// protocolDailyActivityQuery) carry. Without it, the roster count includes the still-accumulating
// current day and wobbles on every re-read, the same phantom-cliff those siblings avoid.
func TestProtocolContractActivityQuery_ExcludesPartialDay(t *testing.T) {
	if !strings.Contains(protocolContractActivityQuery, "close_time < toStartOfDay(now())") {
		t.Errorf("protocolContractActivityQuery lost its complete-days-only bound — "+
			"the per-contract roster would include the current partial day, "+
			"unlike protocolEventBreakdownQuery and protocolDailyActivityQuery:\n%s",
			protocolContractActivityQuery)
	}
}

// TestProtocolRawScanQueries_NoExplorerScanSettings — a guard against the
// plausible-but-measured-wrong "fix".
//
// Adding explorerScanSettings (max_threads = 4) to the breakdown measured nearly 2x SLOWER for
// identical I/O. The byte fan-out that clause exists for is a ledger_entries_current part-layout
// effect and does not apply to this shape.
func TestProtocolRawScanQueries_NoExplorerScanSettings(t *testing.T) {
	for name, q := range protocolRawScanQueries() {
		if strings.Contains(q, "max_threads = 4") {
			t.Errorf("%s pinned max_threads=4: measured on r1 that makes this shape "+
				"~2x SLOWER (105,649ms vs 58,460ms) for identical bytes read:\n%s", name, q)
		}
	}
}
