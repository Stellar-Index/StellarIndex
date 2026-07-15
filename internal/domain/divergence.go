package domain

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// DivergenceObservationRecord is one per-reference cross-check
// comparison the divergence worker computes, persisted to
// divergence_observations for the /v1/divergence read path.
// Canonical home of internal/divergence.ObservationRecord — see
// doc.go.
type DivergenceObservationRecord struct {
	Pair       canonical.Pair
	Reference  string
	OurPrice   float64
	RefPrice   float64
	DeltaPct   float64
	Firing     bool
	ObservedAt time.Time
}
