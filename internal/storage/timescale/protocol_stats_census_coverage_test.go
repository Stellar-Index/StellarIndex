package timescale

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The census's two leg shapes. A leg either labels its rows with a
// literal source name (`SELECT 'blend', count(*) FROM blend_positions`)
// or groups by a `source` column on a table that holds several sources'
// rows (`SELECT source, count(*) FROM trades … GROUP BY source`).
//
// The guards below PARSE countRecentEventsQuery rather than restating
// its legs: a hand-written list of expected arms only ever asserts what
// its author already remembered, which is exactly how sorocredit shipped
// four hypertables and a permanent events_24h of 0 while the rollup and
// the lake-derived activity series disagreed in the same payload.
var (
	censusLabelledLeg = regexp.MustCompile(`SELECT\s+'([a-z0-9_-]+)',\s*count\(\*\)\s+FROM\s+([a-z][a-z0-9_]*)`)
	censusGroupedLeg  = regexp.MustCompile(`SELECT\s+source,\s*count\(\*\)(?:\s+AS\s+n)?\s+FROM\s+([a-z][a-z0-9_]*)`)
)

// excludedFromEventCensus lists gap-detector sources that legitimately
// carry no census leg. Each entry MUST say why; a source omitted by
// accident belongs in the query, not here.
var excludedFromEventCensus = map[string]string{
	"sep41_transfers": "token-standard stream, not a protocol: SEP-41 transfers are emitted by every Soroban token and are served through the asset/token surfaces. /v1/protocols has no sep41 entry to attribute a count to.",
	"sep41_supply":    "token-standard stream, same reason as sep41_transfers — mint/burn/clawback feed the supply surfaces, not the protocol directory.",
	"soroban_events":  "the ADR-0029 landing zone — the raw superset every decoded source is projected FROM. Counting it as a protocol would double-count every other leg.",
}

// TestCountRecentEventsQuery_countsEveryIngestedSource is the per-source
// coverage guard for the trailing-24h census behind /v1/protocols'
// events_24h.
//
// [DefaultGapDetectorTargets] is the registry every per-source hypertable
// must join (ADR-0030), so it is the authoritative answer to "which
// sources does this deployment write rows for". A source that writes rows
// but has no census leg reports events_24h: 0 forever — next to a
// lake-derived activity series showing thousands of events a day, in the
// same response.
//
// Table-driven off that registry rather than off a list maintained here,
// so adding a source's hypertable without adding its census arm fails
// this test instead of shipping a self-contradicting payload.
func TestCountRecentEventsQuery_countsEveryIngestedSource(t *testing.T) {
	t.Parallel()

	covered := censusCoveredSources()
	tables := sourceTables()

	seen := make(map[string]bool, len(DefaultGapDetectorTargets))
	for _, target := range DefaultGapDetectorTargets {
		source := target.SourceNetKey()
		if seen[source] {
			continue
		}
		seen[source] = true

		t.Run(source, func(t *testing.T) {
			t.Parallel()
			if reason := excludedFromEventCensus[source]; reason != "" {
				t.Skipf("no census leg by design: %s", reason)
			}
			if covered[source] {
				return
			}
			t.Errorf(
				"source %q writes %s but the trailing-24h census has no leg for it, so "+
					"/v1/protocols/%s serves events_24h: 0 forever.\n"+
					"Add to countRecentEventsQuery in internal/storage/timescale/protocol_stats.go:\n"+
					"  UNION ALL\n"+
					"  SELECT '%s', count(*) FROM %s\n"+
					"   WHERE ledger_close_time >= now() - interval '24 hours'\n"+
					"(one arm per table), OR add %q to excludedFromEventCensus with a documented reason.",
				source, strings.Join(tables[source], ", "), source, source, tables[source][0], source,
			)
		})
	}
}

// TestExcludedFromEventCensus_hasNoStaleEntries keeps the exclusion map
// honest: an entry for a source that no longer writes a hypertable is a
// silent licence to skip, so it must be deleted with the source.
func TestExcludedFromEventCensus_hasNoStaleEntries(t *testing.T) {
	t.Parallel()

	registered := make(map[string]bool, len(DefaultGapDetectorTargets))
	for _, target := range DefaultGapDetectorTargets {
		registered[target.SourceNetKey()] = true
	}
	for source := range excludedFromEventCensus {
		if !registered[source] {
			t.Errorf("excludedFromEventCensus[%q] names a source with no gap-detector target — delete the entry", source)
		}
	}
}

// censusCoveredSources derives the set of logical source names
// countRecentEventsQuery can emit a row for.
//
// Labelled legs contribute their literal. A GROUP BY source leg
// contributes whichever sources land in that table — which
// [DefaultGapDetectorTargets] already records, since a source scanned
// through a shared table registers a target on it (sdex/aquarius/
// soroswap/phoenix/comet on `trades`, the five oracle feeds on
// `oracle_updates`).
func censusCoveredSources() map[string]bool {
	covered := make(map[string]bool, 32)
	for _, m := range censusLabelledLeg.FindAllStringSubmatch(countRecentEventsQuery, -1) {
		covered[m[1]] = true
	}
	grouped := make(map[string]bool, 4)
	for _, m := range censusGroupedLeg.FindAllStringSubmatch(countRecentEventsQuery, -1) {
		grouped[m[1]] = true
	}
	for _, target := range DefaultGapDetectorTargets {
		if grouped[target.Table] {
			covered[target.SourceNetKey()] = true
		}
	}
	return covered
}

// sourceTables groups the registered hypertables by the source they
// belong to, for the failure message's copy-pasteable arm.
func sourceTables() map[string][]string {
	out := make(map[string][]string, len(DefaultGapDetectorTargets))
	for _, target := range DefaultGapDetectorTargets {
		source := target.SourceNetKey()
		out[source] = append(out[source], target.Table)
	}
	for source := range out {
		sort.Strings(out[source])
	}
	return out
}
