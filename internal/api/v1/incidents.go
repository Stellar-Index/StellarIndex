package v1

import (
	"net/http"

	"github.com/RatesEngine/rates-engine/internal/incidents"
)

// IncidentsList is the wire shape returned by /v1/incidents.
// `incidents` is sorted started_at desc by the loader.
type IncidentsList struct {
	Incidents []incidents.Incident `json:"incidents"`
	Count     int                  `json:"count"`
}

// handleIncidents serves GET /v1/incidents.
//
// Returns every customer-facing incident post the binary has
// embedded (`internal/incidents/data/*.md`), parsed at startup
// and cached on the Server. New posts ship with a redeploy.
//
// No filtering / pagination today — the corpus is small enough
// (few entries per year of operation) that a flat list is fine.
// If we ever cross 100 entries, an `?since=` filter is the
// natural next step.
func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, IncidentsList{
		Incidents: s.incidents,
		Count:     len(s.incidents),
	}, Flags{})
}
