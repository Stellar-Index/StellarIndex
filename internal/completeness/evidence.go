package completeness

import "time"

// MaxProjectionCarryAge bounds how long an incremental -pass audit may carry a
// clean projection claim forward before it re-reconciles the whole served range.
// Each carry re-proves only the newest ledgers, so without a bound the oldest
// evidence behind `projection_ok` grows without limit while computed_at stays fresh.
const MaxProjectionCarryAge = 7 * 24 * time.Hour

// ProjectionEvidenceExpired reports whether a projection claim last reconciled
// in full at evidencedAt is older than maxAge at now. Unknown evidence (zero)
// is expired: an unknown age cannot be claimed fresh. maxAge <= 0 disables the bound.
func ProjectionEvidenceExpired(evidencedAt, now time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return false
	}
	return evidencedAt.IsZero() || now.Sub(evidencedAt) > maxAge
}
