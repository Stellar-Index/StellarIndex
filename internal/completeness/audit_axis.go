package completeness

import (
	"fmt"
	"regexp"
	"strconv"
)

// SystemRecognitionSource is the reserved `source` value of the SYSTEM recognition row in
// completeness_snapshots: the ADR-0033 recognition audit's census of event shapes on contracts no
// registry source owns, not a source.
//
// Its per-source fields are meaningless (substrate_ok / projection_ok hardcoded true, complete /
// lake_complete false by construction). The `WHERE source <> 'recognition'` clauses in
// configs/ansible/roles/archival-node/files/data-freshness.sh exclude it from the per-source
// gauges, and [IsAuditAxis] is the Go twin so /v1/coverage's public headline agrees.
const SystemRecognitionSource = "recognition"

// IsAuditAxis reports whether a completeness_snapshots row is a SYSTEM AUDIT AXIS rather than an
// indexed source's verdict. Fail-loud by design: an unclassified name counts as a source, since a
// pseudo-row quietly excusing itself from the trust surface is the dangerous direction.
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

// recognitionDetailRE parses [FormatRecognitionDetail]'s output; TestRecognitionDetail_roundTrip
// keeps producer and parser from drifting apart.
var recognitionDetailRE = regexp.MustCompile(
	`^(\d+) unrecognized shape\(s\) on (\d+) unowned contract\(s\) \(earliest ledger (\d+)\)`)

// FormatRecognitionDetail renders the census as the `detail` string stored on the recognition
// snapshot and served verbatim on /v1/coverage.
//
// WIRE CONTRACT: the string MUST lead with the shape count as bare digits. The data-freshness
// exporter (configs/ansible/roles/archival-node/files/data-freshness.sh) reads it with
// `substring(detail FROM '^[0-9]+')`; a non-numeric prefix silently reports 0 shapes.
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
// ok=false when the string is not in [FormatRecognitionDetail]'s shape. Callers MUST then omit
// the typed counts rather than publish zeroes, which would claim cleanliness from a parse failure.
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
