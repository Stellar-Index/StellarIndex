package v1

import (
	"context"
	"net/http"
	"time"

	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// CursorsReader is the seam the /v1/diagnostics/cursors handler reads
// through. timescale.Store satisfies it via ListCursors.
type CursorsReader interface {
	ListCursors(ctx context.Context) ([]timescale.Cursor, error)
}

// Cursor is the wire shape of one row in the
// /v1/diagnostics/cursors response. last_updated is RFC 3339; lag
// is reported as seconds-since-update so operators can spot stuck
// sources without wall-clock math.
type Cursor struct {
	Source      string `json:"source"`
	SubSource   string `json:"sub_source,omitempty"`
	LastLedger  uint32 `json:"last_ledger"`
	LastUpdated string `json:"last_updated"`
	LagSeconds  int64  `json:"lag_seconds"`
}

// statusActiveMaxAge is the lag-seconds ceiling that the
// `?status=active` filter uses to distinguish a live, actively-
// writing cursor from a stale / completed one. 10 minutes is a
// generous-but-not-excessive window for the live indexer
// (production cursor updates every ~5s) and reliably excludes
// completed backfill cursors that linger in the table for days
// or weeks before manual cleanup. R-015 in the 2026-05-10 review.
const statusActiveMaxAge = 10 * time.Minute

// handleCursors serves GET /v1/diagnostics/cursors — every row of
// `ingestion_cursors` so operators (and the explorer /diagnostics
// page) can see per-source ingest progress at a glance. Not a hot
// path; the table is small (one row per (source, sub_source)).
//
// Optional query params:
//
//   - status — convenience filter. Values:
//
//   - "active"    → only rows with lag_seconds <= 600 (10m).
//     Excludes completed backfill cursors that
//     linger after their range finished.
//
//   - "stale"     → only rows with lag_seconds > 600 (the
//     complement of "active"); useful for
//     spotting dead ingest paths.
//
//   - "" / omitted → return everything.
//     Invalid values return 400 invalid-status. R-015.
//
//   - max_age — Go-duration string (e.g. "1h", "30m", "5m"). When
//     present, rows with lag_seconds greater than this value are
//     excluded. Lower-level than `status` — use this when you
//     need an arbitrary threshold (e.g. "5 min" or "2h") rather
//     than the active/stale boundary. Setting both `status` and
//     `max_age` returns the intersection. Invalid duration →
//     400 invalid-max-age.
//
//   - source — exact-match filter on the `source` column. Today's
//     production values are "ledgerstream" (the live indexer) and
//     "backfill" (one row per backfill range). Useful for
//     `?source=ledgerstream` to isolate the live cursor from the
//     ~50 backfill rows. Empty / omitted = return all sources.
//     Unknown values return an empty array (not 400) — keeps the
//     surface predictable when an operator typos vs. a brand-new
//     source we haven't seen yet.
func (s *Server) handleCursors(w http.ResponseWriter, r *http.Request) {
	if s.cursors == nil {
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/cursors-unavailable",
			"Cursors unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the cursors reader yet.")
		return
	}

	var maxAge time.Duration
	if raw := r.URL.Query().Get("max_age"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			writeProblem(w, r,
				"https://api.ratesengine.net/errors/invalid-max-age",
				"Invalid max_age", http.StatusBadRequest,
				"max_age must be a positive Go-duration string (e.g. \"1h\", \"30m\", \"5m\")")
			return
		}
		maxAge = d
	}

	// status: "active" / "stale" / "" — semantic convenience layer
	// over max_age, R-015. Active = lag <= 10 min (caps maxAge);
	// stale = the complement (handled inside the row loop). Both
	// can combine with an explicit max_age — for "active" the
	// effective ceiling is the tighter of the two; for "stale" the
	// window becomes [statusActiveMaxAge, max_age].
	var statusStale bool
	switch r.URL.Query().Get("status") {
	case "":
		// no-op — return everything (subject to max_age + source).
	case "active":
		if maxAge == 0 || maxAge > statusActiveMaxAge {
			maxAge = statusActiveMaxAge
		}
	case "stale":
		statusStale = true
	default:
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/invalid-status",
			"Invalid status", http.StatusBadRequest,
			`status must be one of: "active", "stale", or omitted`)
		return
	}

	sourceFilter := r.URL.Query().Get("source")

	rows, err := s.cursors.ListCursors(r.Context())
	if err != nil {
		s.logger.Warn("cursors list", "err", err)
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/cursors-error",
			"Cursors listing failed", http.StatusInternalServerError,
			"Storage layer returned an error.")
		return
	}

	now := time.Now().UTC()
	out := make([]Cursor, 0, len(rows))
	for _, c := range rows {
		if sourceFilter != "" && c.Source != sourceFilter {
			continue
		}
		lag := now.Sub(c.UpdatedAt)
		if maxAge > 0 && lag > maxAge {
			continue
		}
		// Stale filter is inverse: keep rows OLDER than the active
		// threshold. Combined with an explicit max_age, the resulting
		// window is [statusActiveMaxAge, max_age].
		if statusStale && lag <= statusActiveMaxAge {
			continue
		}
		out = append(out, Cursor{
			Source:      c.Source,
			SubSource:   c.Sub,
			LastLedger:  c.LastLedger,
			LastUpdated: c.UpdatedAt.UTC().Format(time.RFC3339),
			LagSeconds:  int64(lag.Seconds()),
		})
	}
	writeJSON(w, out, Flags{})
}
