package v1

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// handleSourceHealth serves GET /v1/sources/{name}/health — one
// source's live health row: the same [SourceHealthRow] shape the
// `sources` section of /v1/diagnostics/ingestion carries (registry
// metadata + trailing-24h entries / trades / volume / markets), but
// addressable per source so the explorer's /sources/{name} page can
// poll a single venue without pulling the full operator snapshot.
//
// Data path: the background-refreshed ingestion snapshot (15s cadence,
// see StartIngestionSnapshotRefresh) when populated — sub-millisecond;
// falls back to an inline buildSourceHealth (8s ceiling, soft-fails to
// zeroed stats) right after process start.
//
// Unknown source → 404. The registry is static per binary, so the 404
// set only changes on deploy.
func (s *Server) handleSourceHealth(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := external.Registry[name]; !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/source-not-found",
			"Source not found", http.StatusNotFound,
			fmt.Sprintf("no registered source named %q — see /v1/sources for the catalogue", name))
		return
	}

	var rows []SourceHealthRow
	if entry := s.ingestionSnapshot.Load(); entry != nil {
		rows = entry.snap.Sources
	}
	if len(rows) == 0 {
		// Cold start: the refresher hasn't fired yet. Same ceiling as
		// the snapshot's sources filler; buildSourceHealth soft-fails
		// its stat reads so the worst case is registry metadata with
		// zeroed counters, not an error.
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		rows = buildSourceHealth(ctx, s)
	}
	for i := range rows {
		if rows[i].Name == name {
			writeJSON(w, rows[i], Flags{})
			return
		}
	}

	// Registry lists the source but the health rows don't — can't
	// happen today (buildSourceHealth iterates the same registry) but
	// serve the static projection rather than 500 if the two ever
	// drift (e.g. a future filtered snapshot).
	md := external.Registry[name]
	writeJSON(w, SourceHealthRow{
		Name:          name,
		Class:         string(md.Class),
		Subclass:      string(md.Subclass),
		IncludeInVWAP: md.IncludeInVWAP,
		BackfillSafe:  md.BackfillSafe,
	}, Flags{})
}
