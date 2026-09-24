package timescale

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// oracle_updates is the one served-tier table that can hold rows whose
// `asset` is not an asset at all. Since the capture-totality change, an
// oracle symbol that maps to no canonical asset is recorded verbatim as
// `raw:<symbol>` rather than dropped, so the evidence survives a
// mapping gap. Those rows are RECORD-layer only: nothing that
// interprets a price may treat one as an asset.
//
// A read of this table is safe by one of two mechanisms, and which one
// applies is a property of the QUERY:
//
//   - It keys by canonical asset (`asset = $n`, `asset = ANY($n)`). A
//     raw row then cannot come back unless a caller explicitly asked
//     for one, and the callers build keys from canonical.Pair legs,
//     which Pair.Validate refuses to construct from a raw asset.
//   - Or it scans, and something else must handle the raw rows —
//     either a filter in the query or an explicit contract above it.
//
// The second kind is where a defect can hide, because the query looks
// perfectly ordinary. This guard makes every scanning read declare
// itself, so adding one is a deliberate act with a written reason
// rather than an oversight.
//
// Why a guard and not a fix: audited 2026-09-01 against HEAD and live
// production, every existing scan is already correct — see the
// exemption table below for the per-query verdict. The gap was never
// the code; it was that #305's squash merge deleted the tests proving
// it (issue #339), leaving the property unpinned.

// scanningOracleReads are the reads in oracle.go that do NOT key by
// canonical asset, each with the reason it is nonetheless safe.
//
// Adding a function here is the deliberate act: if a new scanning read
// appears without an entry, the test fails and asks for the reasoning.
var scanningOracleReads = map[string]string{
	"CountOracleUpdates": "a bare row count for diagnostics, explicitly " +
		"documented as not for production hot paths. A count of what the " +
		"table holds SHOULD include raw rows — excluding them would under-" +
		"report what was captured, which is the opposite of the point.",

	"LatestOracleStreams": "backs /v1/oracle/streams, whose contract is that " +
		"raw rows are omitted unless include_unmapped=true and every reading " +
		"carries a `mapped` discriminator. The filter is applied ABOVE this " +
		"query, deliberately, so the endpoint can offer the opt-in at all — " +
		"a query-level filter would make include_unmapped unimplementable. " +
		"Verified live 2026-09-01: default returned 119 rows with 0 raw, " +
		"include_unmapped=true returned 120 including raw:USDT0. Covered by " +
		"v1.TestOracleStreams_UnmappedRowsOptIn.",

	"bespoke_oracle.go:oracleWindowKPIQuery": "the oracle bespoke page is " +
		"explicitly COUNT+TIMESTAMP totality over one source — the file doc " +
		"documents that unmapped feeds stay in every count/table and are " +
		"surfaced as their own 'Unmapped feeds' KPI rather than filtered.",
	"bespoke_oracle.go:oracleMedianIntervalQuery": "same totality contract as " +
		"oracleWindowKPIQuery: median publish cadence for a source includes " +
		"every publication, mapped or not.",
	"bespoke_oracle.go:oracleAllTimeKPIQuery": "same totality contract: " +
		"all-time update count for a source includes unmapped feeds.",
	"bespoke_oracle.go:oracleUpdatesSeriesQuery": "same totality contract: " +
		"the total-updates series for a source includes unmapped feeds.",
	"bespoke_oracle.go:oraclePerFeedSeriesQuery": "same totality contract: " +
		"per-feed series are keyed by (asset, quote) as returned, including " +
		"raw:<symbol> feeds, which the page renders reference-only.",
	"bespoke_oracle.go:oracleFeedBreakdownQuery": "same totality contract: " +
		"the updates-by-feed breakdown includes unmapped feeds.",
	"bespoke_oracle.go:oracleFeedsTableQuery": "same totality contract: the " +
		"per-feed cadence table includes unmapped feeds.",
	"bespoke_oracle.go:oracleLatestPricesQuery": "same totality contract: the " +
		"latest-observation table shows the raw integer verbatim per feed, " +
		"including unmapped ones — never compared or aggregated (file doc).",

	"diagnostics.go:LedgerRangeToOracleTimeRange": "returns only MIN/MAX(ts) " +
		"over a ledger range for backfill windowing — no asset or price is " +
		"read out, so an unmapped feed's timestamp contributing to the true " +
		"range is correct, not a leak.",
	"diagnostics.go:SourceEntryCounts": "seedSourceEntryCountsSQL: a diagnostics " +
		"reseed of source_entry_counts, a bare per-source row count — like " +
		"CountOracleUpdates, undercounting by excluding raw rows would " +
		"misreport what was actually captured.",

	"protocol_stats.go:<package level>": "countRecentEventsQuery: the trailing-" +
		"24h events_24h census is a bare per-source row count feeding " +
		"/v1/protocols — excluding unmapped rows would undercount what the " +
		"oracle source actually published, the same reasoning as " +
		"CountOracleUpdates.",
}

// oracleUpdatesReaderFiles are every file in this package with at least
// one `FROM oracle_updates` read, discovered by a repo-wide grep — NOT
// just oracle.go. A file dropping off this list without its reads
// disappearing would make the guard scan the wrong set and pass
// vacuously, which is exactly the failure mode oracle.go alone had.
var oracleUpdatesReaderFiles = []string{
	"oracle.go",
	"bespoke_oracle.go",
	"diagnostics.go",
	"mev.go",
	"protocol_stats.go",
}

// fromOracleUpdates finds each read of the table and the enclosing
// function, so a failure names the function a reviewer has to look at
// rather than a line number. funcDeclRe matches both `Store` methods
// (oracle.go) and the plain query-builder functions bespoke_oracle.go
// splits its SQL into.
var (
	funcDeclRe = regexp.MustCompile(`(?m)^func (?:\(s \*Store\) )?(\w+)\(`)
	assetKeyRe = regexp.MustCompile(`asset\s*(=|IN)\s*(\$\d+|ANY\(\$\d+\))`)
	rawGuardRe = regexp.MustCompile(`raw:%|IsMapped`)
)

func TestOracleUpdatesReadsAreKeyedOrDeclared(t *testing.T) {
	seen := map[string]bool{}

	for _, path := range oracleUpdatesReaderFiles {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(src)

		// Map each byte offset to the function it sits in.
		type fn struct {
			name  string
			start int
		}
		var fns []fn
		for _, m := range funcDeclRe.FindAllStringSubmatchIndex(text, -1) {
			fns = append(fns, fn{name: text[m[2]:m[3]], start: m[0]})
		}
		enclosing := func(off int) string {
			name := "<package level>"
			for _, f := range fns {
				if f.start <= off {
					name = f.name
				} else {
					break
				}
			}
			return path + ":" + name
		}

		idxs := []int{}
		for i := 0; ; {
			j := strings.Index(text[i:], "FROM oracle_updates")
			if j < 0 {
				break
			}
			idxs = append(idxs, i+j)
			i += j + 1
		}
		if len(idxs) == 0 {
			t.Fatalf("no `FROM oracle_updates` found in %s — it no longer reads "+
				"the table; remove it from oracleUpdatesReaderFiles", path)
		}

		for _, off := range idxs {
			name := enclosing(off)
			seen[name] = true

			// The query body: from the read to the end of its SQL literal.
			end := off + 900
			if end > len(text) {
				end = len(text)
			}
			body := text[off:end]
			if cut := strings.Index(body, "`"); cut > 0 {
				body = body[:cut]
			}

			keyed := assetKeyRe.MatchString(body)
			guarded := rawGuardRe.MatchString(body)
			_, declared := scanningOracleReads[name]
			// oracle.go's original two entries predate the multi-file
			// guard and are stored unqualified.
			if !declared && path == "oracle.go" {
				_, declared = scanningOracleReads[strings.TrimPrefix(name, path+":")]
			}

			if keyed || guarded || declared {
				continue
			}
			t.Errorf("%s reads oracle_updates without keying on a canonical asset "+
				"(`asset = $n` / `asset = ANY($n)`), without a raw filter, and without "+
				"an entry in scanningOracleReads.\n\n"+
				"oracle_updates can hold `raw:<symbol>` rows — an unmapped oracle "+
				"symbol recorded as evidence, not an asset. A scanning read will "+
				"return them. If that is correct here, add %s to scanningOracleReads "+
				"with the reason; if it is not, key the query by asset or filter "+
				"`asset NOT LIKE 'raw:%%'`.", name, name)
		}
	}

	// The exemption list must not outlive its entries: a stale entry is
	// a reader being told a query is safe for a reason that no longer
	// applies to any query.
	for name := range scanningOracleReads {
		qualified := seen[name]
		if !qualified {
			// oracle.go's two unqualified legacy entries.
			qualified = seen["oracle.go:"+name]
		}
		if !qualified {
			t.Errorf("scanningOracleReads names %q, but no read of oracle_updates "+
				"sits in a function by that name — the exemption is stale and "+
				"should be removed", name)
		}
	}
}
