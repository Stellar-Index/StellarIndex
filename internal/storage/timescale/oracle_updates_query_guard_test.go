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
}

// fromOracleUpdates finds each read of the table and the enclosing
// function, so a failure names the function a reviewer has to look at
// rather than a line number.
var (
	funcDeclRe = regexp.MustCompile(`(?m)^func \(s \*Store\) (\w+)\(`)
	assetKeyRe = regexp.MustCompile(`asset\s*(=|IN)\s*(\$\d+|ANY\(\$\d+\))`)
	rawGuardRe = regexp.MustCompile(`raw:%|IsMapped`)
)

func TestOracleUpdatesReadsAreKeyedOrDeclared(t *testing.T) {
	const path = "oracle.go"
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
		return name
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
		t.Fatal("no `FROM oracle_updates` found in oracle.go — this guard is " +
			"scanning the wrong file and would pass vacuously; update the path")
	}

	seen := map[string]bool{}
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

	// The exemption list must not outlive its entries: a stale entry is
	// a reader being told a query is safe for a reason that no longer
	// applies to any query.
	for name := range scanningOracleReads {
		if !seen[name] {
			t.Errorf("scanningOracleReads names %q, but no read of oracle_updates in "+
				"%s sits in a function by that name — the exemption is stale and "+
				"should be removed", name, path)
		}
	}
}
