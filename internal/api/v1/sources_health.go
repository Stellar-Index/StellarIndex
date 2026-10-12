package v1

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// handleSourceHealth serves GET /v1/sources/{name}/health: one [SourceHealthRow] from the 15s background ingestion snapshot, else an
// inline buildSourceHealth (8s ceiling, soft-fails to zeroed stats).
// An unknown source, or one absent on the configured network, is a 404 to agree with /v1/sources.
func (s *Server) handleSourceHealth(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	detail := ""
	if _, ok := external.Registry[name]; !ok {
		detail = fmt.Sprintf("no registered source named %q — see /v1/sources for the catalogue", name)
	} else if ok, _ := sourcenet.Applicable(name, s.Network); !ok {
		detail = fmt.Sprintf("source %q does not exist on network %s — see /v1/sources for the catalogue", name, s.Network)
	}
	if detail != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/source-not-found",
			"Source not found", http.StatusNotFound, detail)
		return
	}

	var rows []SourceHealthRow
	var asOf WireTime // zero stamps now on the inline build
	if entry := s.freshIngestionSnapshot(); entry != nil {
		rows, asOf = entry.snap.Sources, WireTime(entry.builtAt.UTC())
	}
	if len(rows) == 0 {
		asOf = WireTime{}
		// Cold start, or a dead refresher left the snapshot stale (see
		// freshIngestionSnapshot). buildSourceHealth soft-fails its stat reads, so
		// the worst case is registry metadata with zeroed counters, not an error.
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		rows = buildSourceHealth(ctx, s)
	}
	for i := range rows {
		if rows[i].Name == name {
			writeEnvelope(w, Envelope{Data: rows[i], AsOf: asOf})
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
		BackfillSafe:  md.BackfillSafe(),
	}, Flags{})
}
