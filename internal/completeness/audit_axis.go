package completeness

import (
	"fmt"
	"regexp"
	"strconv"
)

// SystemRecognitionSource is the reserved `source` value of the
// SYSTEM recognition row in completeness_snapshots.
//
// It is NOT a source. It is the ADR-0033 recognition audit's
// unattributed census: distinct (contract, topic) event shapes in the
// lake on contracts that NO source in the registry owns — foreign
// Soroban protocols we have not integrated. compute-completeness
// writes it through the same CompletenessSnapshot struct as a real
// source purely because that is the table it lives in, which forces
// six per-source fields onto it that have no meaning for a census:
// substrate_ok / projection_ok are hardcoded true, complete /
// lake_complete are false PERMANENTLY BY CONSTRUCTION (they can only
// be true if no un-indexed Soroban contract exists anywhere on the
// network), and coverage_pct measures ledgers until the first foreign
// contract appeared, so it only ever decreases.
//
// The deployed metrics already exclude this row from the per-source
// gauges for exactly this reason — see the two `WHERE source <>
// 'recognition'` clauses in
// configs/ansible/roles/archival-node/files/data-freshness.sh (PR
// #465). [IsAuditAxis] is the Go-side twin so /v1/coverage's public
// headline agrees with them.
const SystemRecognitionSource = "recognition"

// IsAuditAxis reports whether a completeness_snapshots row is a
// SYSTEM AUDIT AXIS rather than an indexed source's verdict.
//
// Fail-LOUD by design: a name this function does not classify is not
// an audit axis, so it counts as a source and can still fail the
// public "N of M complete" headline. The dangerous direction here is
// a pseudo-row quietly excusing itself from the trust surface, so the
// set is a closed, reserved-name list rather than a shape heuristic.
func IsAuditAxis(source string) bool {
	return source == SystemRecognitionSource
}

// RecognitionCensus is the unattributed-recognition audit's result:
// how many distinct event shapes, on how many distinct contracts, and
// the earliest ledger one was seen at.
//
// Zero shapes is the clean state ("every event shape on-chain belongs
// to a protocol we decode") — attainable in principle, not in practice
// on a public network.
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

// FormatRecognitionDetail renders the census as the `detail` string
// stored on the system recognition snapshot and served verbatim on
// /v1/coverage.
//
// WIRE CONTRACT — the string MUST keep leading with the shape count as
// bare digits. The deployed data-freshness exporter derives
// stellarindex_recognition_unattributed_shapes with
// `substring(detail FROM '^[0-9]+')`
// (configs/ansible/roles/archival-node/files/data-freshness.sh), and a
// non-numeric prefix silently reports 0 shapes — the exact
// under-reporting direction this surface exists to prevent.
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

// ParseRecognitionDetail recovers the census from a stored detail
// string, so /v1/coverage can publish the numbers as typed fields
// instead of making every consumer regex English.
//
// ok=false when the string is not in [FormatRecognitionDetail]'s
// shape — including rows written before this format existed. Callers
// MUST then omit the typed counts rather than publish zeroes: "0
// unrecognized shapes" is a claim of cleanliness, and inventing it
// from a parse failure is precisely the laundering this endpoint must
// not do. The raw detail string is still served, so nothing is lost.
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
