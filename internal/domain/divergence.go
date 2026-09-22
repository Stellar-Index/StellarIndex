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
//
// OurPrice / RefPrice / DeltaPct are decimal strings (ADR-0003 lists
// oracle prices among the values no code path may hold in float64):
// the worker formats its computed values at this boundary so the
// write path (RecordObservation) never binds a raw float64 into the
// NUMERIC columns it feeds, matching the decimal-string convention
// the read side ([DivergenceRow]) already uses for the same table.
type DivergenceObservationRecord struct {
	Pair       canonical.Pair
	Reference  string
	OurPrice   string
	RefPrice   string
	DeltaPct   string
	Firing     bool
	ObservedAt time.Time
}
