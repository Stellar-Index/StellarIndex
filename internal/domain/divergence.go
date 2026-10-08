package domain

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// DivergenceObservationRecord is one per-reference cross-check row in
// divergence_observations. Prices are decimal strings (ADR-0003) so the write
// path never binds a float64 into the NUMERIC columns.
type DivergenceObservationRecord struct {
	Pair       canonical.Pair
	Reference  string
	OurPrice   string
	RefPrice   string
	DeltaPct   string
	Firing     bool
	ObservedAt time.Time
	// RefObservedAt is when the reference's upstream observed RefPrice
	// (the quote's AsOf); ObservedAt is the comparison time.
	RefObservedAt time.Time
}
