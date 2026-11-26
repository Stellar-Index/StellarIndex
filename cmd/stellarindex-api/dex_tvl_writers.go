package main

import (
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

// aquariusReserveWriters names the ingestion cursors of whatever writes aquarius_reserves under this config.
func aquariusReserveWriters(p config.ProjectorConfig) []v1.TVLWriterCursor {
	return projectedSourceWriters(p, "aquarius")
}

// lendingEventWriters names, per lending source, the cursors of whatever writes its event tables under this config.
func lendingEventWriters(p config.ProjectorConfig) map[string][]v1.TVLWriterCursor {
	return map[string][]v1.TVLWriterCursor{
		blend.SourceName: projectedSourceWriters(p, blend.SourceName),
		blnt.SourceName:  projectedSourceWriters(p, blnt.SourceName),
	}
}

// projectedSourceWriters names the writers of a projected source that is not a sole-writer source, as
// pipeline.SinkModeForProjector decides it: the dispatcher alone with the projector off, both in parallel mode, the projector
// alone once per-source persistence is off.
func projectedSourceWriters(p config.ProjectorConfig, source string) []v1.TVLWriterCursor {
	dispatcher := v1.TVLWriterCursor{Source: "ledgerstream"}
	projector := v1.TVLWriterCursor{Source: "projector", Sub: source}
	switch {
	case !p.Enabled:
		return []v1.TVLWriterCursor{dispatcher}
	case p.PersistPerSource:
		return []v1.TVLWriterCursor{projector, dispatcher}
	}
	return []v1.TVLWriterCursor{projector}
}
