package completeness

import "time"

// MaxProjectionCarryAge bounds how long an incremental -pass audit may carry a clean projection claim forward before re-reconciling the whole served range, so the oldest evidence behind `projection_ok` cannot grow unbounded.
const MaxProjectionCarryAge = 7 * 24 * time.Hour

// ProjectionEvidenceExpired reports whether a projection claim last fully reconciled at evidencedAt is older than maxAge at now; zero (unknown) evidence is expired, and maxAge <= 0 disables the bound.
func ProjectionEvidenceExpired(evidencedAt, now time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return false
	}
	return evidencedAt.IsZero() || now.Sub(evidencedAt) > maxAge
}
