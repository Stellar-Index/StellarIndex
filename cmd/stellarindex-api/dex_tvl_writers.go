package main

import (
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// aquariusReserveWriters names the ingestion cursors of whatever writes aquarius_reserves under this config, as
// pipeline.SinkModeForProjector decides it: the dispatcher alone with the projector off, both in parallel mode
// (aquarius is not a sole-writer source), the projector alone once per-source persistence is off.
func aquariusReserveWriters(p config.ProjectorConfig) []v1.TVLWriterCursor {
	dispatcher := v1.TVLWriterCursor{Source: "ledgerstream"}
	projector := v1.TVLWriterCursor{Source: "projector", Sub: "aquarius"}
	switch {
	case !p.Enabled:
		return []v1.TVLWriterCursor{dispatcher}
	case p.PersistPerSource:
		return []v1.TVLWriterCursor{projector, dispatcher}
	}
	return []v1.TVLWriterCursor{projector}
}
