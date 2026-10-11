package completeness

import (
	"fmt"
	"regexp"
	"strconv"
)

// SystemRecognitionSource is the reserved `source` value of the SYSTEM recognition row in
// completeness_snapshots. It is NOT a source but the ADR-0033 recognition audit's census of
// (contract, topic) event shapes on contracts no registry source owns (foreign Soroban protocols).
//
// Its per-source fields are meaningless: substrate_ok / projection_ok are hardcoded true, and
// complete / lake_complete are false by construction (true only if no un-indexed Soroban contract
// exists anywhere). The two `WHERE source <> 'recognition'` clauses in
// configs/ansible/roles/archival-node/files/data-freshness.sh exclude it from the per-source
// gauges, and [IsAuditAxis] is the Go twin so /v1/coverage's public headline agrees.
const SystemRecognitionSource = "recognition"

// IsAuditAxis reports whether a completeness_snapshots row is a SYSTEM AUDIT AXIS rather than an
// indexed source's verdict.
//
// Fail-loud by design: an unclassified name counts as a source and can still fail the public
// "N of M complete" headline. A pseudo-row quietly excusing itself from the trust surface is the
// dangerous direction, so the set is a closed reserved-name list, not a shape heuristic.
func IsAuditAxis(source string) bool {
	return source == SystemRecognitionSource
}

// RecognitionCensus is the unattributed-recognition audit's result: distinct event shapes, the
// distinct contracts they sit on, and the earliest ledger one was seen at. Zero shapes is the
// clean state, attainable in principle but not on a public network.
type RecognitionCensus struct {
	Shapes         int
	Contracts      int
	EarliestLedger uint32
}

// recognitionDetailRE parses [FormatRecognitionDetail]'s output back
// into a [RecognitionCensus]. Producer and parser live together so a
// reword cannot drift them apart; TestRecognitionDetail_roundTrip
// pins that.
var recognitionDetailRE = regexp.MustCompile(
	`^(\d+) unrecognized shape\(s\) on (\d+) unowned contract\(s\) \(earliest ledger (\d+)\)`)

// FormatRecognitionDetail renders the census as the `detail` string stored on the recognition
// snapshot and served verbatim on /v1/coverage.
//
// WIRE CONTRACT: the string MUST lead with the shape count as bare digits. The data-freshness
// exporter (configs/ansible/roles/archival-node/files/data-freshness.sh) derives
// stellarindex_recognition_unattributed_shapes with `substring(detail FROM '^[0-9]+')`, and a
// non-numeric prefix silently reports 0 shapes, the under-reporting this surface exists to prevent.
func FormatRecognitionDetail(c RecognitionCensus) string {
	if c.Shapes == 0 {
		return "0 unrecognized shape(s): every event shape in the lake belongs to a contract an indexed source owns"
	}
	return fmt.Sprintf(
		"%d unrecognized shape(s) on %d unowned contract(s) (earliest ledger %d) — "+
			"events on contracts no indexed source owns (foreign Soroban protocols); "+
			"a discovery backlog, not missing data — run verify-recognition",
		c.Shapes, c.Contracts, c.EarliestLedger)
}

// ParseRecognitionDetail recovers the census from a stored detail string so /v1/coverage can
// publish typed fields instead of making consumers regex English.
//
// ok=false when the string is not in [FormatRecognitionDetail]'s shape, including older rows.
// Callers MUST then omit the typed counts rather than publish zeroes: "0 unrecognized shapes"
// claims cleanliness, and inventing it from a parse failure is laundering. The raw detail is
// still served.
func ParseRecognitionDetail(detail string) (RecognitionCensus, bool) {
	m := recognitionDetailRE.FindStringSubmatch(detail)
	if m == nil {
		// The clean state carries no contract count or ledger.
		if detail == FormatRecognitionDetail(RecognitionCensus{}) {
			return RecognitionCensus{}, true
		}
		return RecognitionCensus{}, false
	}
	shapes, err := strconv.Atoi(m[1])
	if err != nil {
		return RecognitionCensus{}, false
	}
	contracts, err := strconv.Atoi(m[2])
	if err != nil {
		return RecognitionCensus{}, false
	}
	earliest, err := strconv.ParseUint(m[3], 10, 32)
	if err != nil {
		return RecognitionCensus{}, false
	}
	return RecognitionCensus{Shapes: shapes, Contracts: contracts, EarliestLedger: uint32(earliest)}, true
}
