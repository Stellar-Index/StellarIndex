package timescale

import (
	"fmt"
	"strings"
	"testing"
)

// TestCountDistinctLedgersQueryRoutesOnlySorobanEventsToCensus is the
// differential guard for the 2026-08-28 r1 incident fix: the density
// numerator for soroban-events comes from the ledger_ingest_log census
// (PK range scan) instead of a full scan of the index-less 257 GB
// soroban_events hypertable — and EVERY other registered target keeps
// the generic COUNT(DISTINCT <ledger>) over its own table, filter
// included, byte-for-byte. Gauge / snapshot / density_pct semantics
// for the rest of the registry must not move.
func TestCountDistinctLedgersQueryRoutesOnlySorobanEventsToCensus(t *testing.T) {
	t.Parallel()

	var redirected []string
	for _, target := range DefaultGapDetectorTargets {
		got := countDistinctLedgersQuery(target)
		if target.Source == "soroban-events" {
			redirected = append(redirected, target.Source)
			const want = `SELECT COUNT(*) FROM ledger_ingest_log WHERE ledger_seq BETWEEN $1 AND $2 AND soroban_event_count > 0`
			if got != want {
				t.Errorf("soroban-events count query:\n got %q\nwant %q", got, want)
			}
			if strings.Contains(got, "soroban_events") {
				t.Errorf("soroban-events count query must not touch the soroban_events hypertable: %q", got)
			}
			continue
		}
		if target.DistinctLedgerCountSQL != "" {
			redirected = append(redirected, target.Source)
		}
		filter := ""
		if target.WhereFilter != "" {
			filter = " AND (" + target.WhereFilter + ")"
		}
		want := fmt.Sprintf(`SELECT COUNT(DISTINCT %[1]s) FROM %[2]s WHERE %[1]s BETWEEN $1 AND $2%[3]s`,
			target.LedgerColumn, target.Table, filter)
		if got != want {
			t.Errorf("%s/%s count query changed:\n got %q\nwant %q", target.Source, target.Table, got, want)
		}
	}
	if len(redirected) != 1 || redirected[0] != "soroban-events" {
		t.Errorf("exactly soroban-events must carry a DistinctLedgerCountSQL override; got %v", redirected)
	}
}

// TestCountDistinctLedgersQueryOverrideBindsRange pins the override
// contract the integration test relies on: $1/$2 are the inclusive
// ledger range, and a target with no override still gets the generic
// statement (an unregistered ad-hoc target must not silently route to
// the census).
func TestCountDistinctLedgersQueryOverrideBindsRange(t *testing.T) {
	t.Parallel()
	for _, ph := range []string{"$1", "$2"} {
		if !strings.Contains(sorobanEventsDistinctLedgerCountSQL, ph) {
			t.Errorf("override must bind %s: %q", ph, sorobanEventsDistinctLedgerCountSQL)
		}
	}
	adHoc := GapDetectorTarget{Source: "x", Table: "ledger_ingest_log", LedgerColumn: "ledger_seq"}
	if got, want := countDistinctLedgersQuery(adHoc), `SELECT COUNT(DISTINCT ledger_seq) FROM ledger_ingest_log WHERE ledger_seq BETWEEN $1 AND $2`; got != want {
		t.Errorf("ad-hoc target:\n got %q\nwant %q", got, want)
	}
}
