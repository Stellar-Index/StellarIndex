package completeness

import (
	"fmt"
	"regexp"
	"strconv"
)

// SystemRecognitionSource is the reserved `source` of the SYSTEM recognition row in completeness_snapshots, not a source; its per-source fields are meaningless, so data-freshness.sh excludes it via `WHERE source <> 'recognition'` and IsAuditAxis is the Go twin.
const SystemRecognitionSource = "recognition"

// IsAuditAxis reports whether a completeness_snapshots row is a SYSTEM AUDIT AXIS rather than an indexed source's verdict; fail-loud by design, so an unclassified name counts as a source.
func IsAuditAxis(source string) bool {
	return source == SystemRecognitionSource
}

// RecognitionCensus is the unattributed-recognition audit's result: distinct event shapes, the contracts they sit on, and the earliest ledger one was seen at; zero shapes is the clean state.
type RecognitionCensus struct {
	Shapes         int
	Contracts      int
	EarliestLedger uint32
}

// recognitionDetailRE parses [FormatRecognitionDetail]'s output; TestRecognitionDetail_roundTrip
// keeps producer and parser from drifting apart.
var recognitionDetailRE = regexp.MustCompile(
	`^(\d+) unrecognized shape\(s\) on (\d+) unowned contract\(s\) \(earliest ledger (\d+)\)`)

// FormatRecognitionDetail renders the census as the `detail` string served on /v1/coverage; WIRE CONTRACT: it MUST lead with the shape count as bare digits, which data-freshness.sh reads via `substring(detail FROM '^[0-9]+')`.
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

// ParseRecognitionDetail recovers the census from a stored detail string; ok=false when it is not in FormatRecognitionDetail's shape, and callers MUST then omit the typed counts rather than publish zeroes that claim cleanliness.
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
