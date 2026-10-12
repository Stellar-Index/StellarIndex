package v1

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CursorsReader is the seam the /v1/diagnostics/cursors handler reads through; timescale.Store satisfies it.
type CursorsReader interface {
	ListCursors(ctx context.Context) ([]timescale.Cursor, error)
}

// Cursor is one /v1/diagnostics/cursors row; State is the derived lifecycle marker (see [cursorStateFor]) so readers need no threshold arithmetic.
type Cursor struct {
	Source      string `json:"source"`
	SubSource   string `json:"sub_source,omitempty"`
	LastLedger  uint32 `json:"last_ledger"`
	LastUpdated string `json:"last_updated"`
	LagSeconds  int64  `json:"lag_seconds"`
	State       string `json:"state"`
}

// statusActiveMaxAge is the lag ceiling for `?status=active`: generous for the live indexer (cursor updates ~5s), still
// excluding completed backfill cursors that linger for weeks.
const statusActiveMaxAge = 10 * time.Minute

// cursorAbandonedAge is the age past which a one-shot-namespace cursor is `abandoned` and dropped from the DEFAULT response
// (live namespaces exempt, see [cursorStateFor]); `ingestion_cursors` has no retention policy, so those rows would swamp every
// response.
const cursorAbandonedAge = 7 * 24 * time.Hour

// Cursor lifecycle states. Derived, not stored: an abandoned shard's writer went away without
// finishing, so it can never mark its own row dead.
const (
	cursorStateLive      = "live"      // lag <= statusActiveMaxAge — something is writing this row
	cursorStateStale     = "stale"     // behind, but a position something is still expected to advance
	cursorStateAbandoned = "abandoned" // past cursorAbandonedAge in a one-shot job's namespace — finished or dead work
)

// Response bound for /v1/diagnostics/cursors. Rows are one per (source, sub_source) with no
// retention, and sharded ops jobs mint thousands. Excluding the abandoned set keeps the DEFAULT
// response small; this cap bounds every response, including `?include_abandoned=true`, which
// pages via `pagination.next`.
const (
	cursorsDefaultLimit = 500
	cursorsMaxLimit     = 2000
)

// cursorStateFor derives lifecycle state from source and lag with the same thresholds `?status=` filters on. A live-namespace
// row ([timescale.LiveCursorSources]) is never `abandoned`: an old row there means ingest is STUCK, the condition this endpoint
// surfaces (same rule as `reap-cursors`).
func cursorStateFor(source string, lag time.Duration) string {
	switch {
	case lag <= statusActiveMaxAge:
		return cursorStateLive
	case lag <= cursorAbandonedAge || timescale.IsLiveCursorSource(source):
		return cursorStateStale
	default:
		return cursorStateAbandoned
	}
}

// cursorsQuery is the parsed and validated query of one /v1/diagnostics/cursors request, built by parseCursorsQuery.
type cursorsQuery struct {
	maxAge           time.Duration
	statusStale      bool
	statusAbandoned  bool
	source           string
	includeAbandoned bool
	limit            int
	offset           int
}

// keeps reports whether a row survives every filter; `state` and `lag` are both passed so the abandoned test reads the
// published state, not a re-derivation.
func (q cursorsQuery) keeps(source, state string, lag time.Duration) bool {
	switch {
	case q.source != "" && source != q.source:
		return false
	case state == cursorStateAbandoned && !q.includeAbandoned:
		return false
	case q.statusAbandoned && state != cursorStateAbandoned:
		return false
	case q.maxAge > 0 && lag > q.maxAge:
		return false
	// Stale filter is inverse: keep rows OLDER than the active
	// threshold. Combined with an explicit max_age, the resulting
	// window is [statusActiveMaxAge, max_age].
	case q.statusStale && lag <= statusActiveMaxAge:
		return false
	}
	return true
}

// parseCursorsQuery validates every handleCursors param, writing the Problem+JSON itself and returning ok=false when malformed.
func parseCursorsQuery(w http.ResponseWriter, r *http.Request) (cursorsQuery, bool) {
	var q cursorsQuery

	if raw := r.URL.Query().Get("max_age"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-max-age",
				"Invalid max_age", http.StatusBadRequest,
				"max_age must be a positive Go-duration string (e.g. \"1h\", \"30m\", \"5m\")")
			return q, false
		}
		q.maxAge = d
	}

	// include_abandoned is an opt-in because abandoned rows vastly outnumber live ones and
	// describe long-ended work.
	q.includeAbandoned = r.URL.Query().Get("include_abandoned") == "true"

	// status is a convenience layer over max_age: active caps maxAge at statusActiveMaxAge, stale
	// is the complement up to the abandoned boundary, and abandoned implies include_abandoned.
	switch r.URL.Query().Get("status") {
	case "":
		// no-op — return everything live + stale (subject to max_age + source).
	case "active":
		if q.maxAge == 0 || q.maxAge > statusActiveMaxAge {
			q.maxAge = statusActiveMaxAge
		}
	case "stale":
		q.statusStale = true
	case cursorStateAbandoned:
		q.statusAbandoned = true
		q.includeAbandoned = true
	default:
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-status",
			"Invalid status", http.StatusBadRequest,
			`status must be one of: "active", "stale", "abandoned", or omitted`)
		return q, false
	}

	q.source = r.URL.Query().Get("source")

	limit, ok := parseExplorerLimit(w, r, cursorsDefaultLimit, cursorsMaxLimit)
	if !ok {
		return q, false
	}
	q.limit = limit

	// `cursor` is the PAGINATION token — the offset of the next row in
	// the filtered listing, as emitted in `pagination.next`. Named for
	// the v1-wide pagination convention rather than for this endpoint's
	// payload; the ingest cursors are the rows, not the token.
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		off, err := strconv.Atoi(raw)
		if err != nil || off < 0 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-cursor",
				"Invalid cursor", http.StatusBadRequest,
				"cursor must be the non-negative integer offset returned in pagination.next")
			return q, false
		}
		q.offset = off
	}

	return q, true
}

// handleCursors serves GET /v1/diagnostics/cursors (params in OpenAPI). Defaults to non-abandoned rows; live namespaces are
// never `abandoned`, so stuck ingest cannot drop out. Unknown `source` returns an empty array, not 400.
func (s *Server) handleCursors(w http.ResponseWriter, r *http.Request) {
	if s.Cursors == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/cursors-unavailable",
			"Cursors unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the cursors reader yet.")
		return
	}

	q, ok := parseCursorsQuery(w, r)
	if !ok {
		return
	}

	listCtx, listCancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer listCancel()
	rows, err := s.Cursors.ListCursors(listCtx)
	if err != nil {
		s.writeCursorsListError(w, r, listCtx, err)
		return
	}

	now := time.Now().UTC()
	matched := make([]Cursor, 0, len(rows))
	for _, c := range rows {
		lag := now.Sub(c.UpdatedAt)
		state := cursorStateFor(c.Source, lag)
		if !q.keeps(c.Source, state, lag) {
			continue
		}
		matched = append(matched, Cursor{
			Source:      c.Source,
			SubSource:   c.Sub,
			LastLedger:  c.LastLedger,
			LastUpdated: c.UpdatedAt.UTC().Format(time.RFC3339),
			LagSeconds:  int64(lag.Seconds()),
			State:       state,
		})
	}

	page, next := pageCursors(matched, q.offset, q.limit)
	env := Envelope{Data: page, Flags: Flags{}}
	if next != "" {
		env.Pagination = &Pagination{Next: next}
	}
	writeEnvelope(w, env)
}

// pageCursors slices the filtered listing to one page and returns the `pagination.next` offset token (empty on the last page);
// always non-nil so `data` marshals as [] not null.
func pageCursors(rows []Cursor, offset, limit int) ([]Cursor, string) {
	if offset >= len(rows) {
		return []Cursor{}, ""
	}
	rows = rows[offset:]
	if len(rows) > limit {
		return rows[:limit], strconv.Itoa(offset + limit)
	}
	return rows, ""
}

// writeCursorsListError maps a ListCursors error to Problem+JSON: transient and timeout shapes give 503 (retry), not a generic
// 500 ("endpoint broken"). Split from handleCursors for the gocognit ceiling.
func (s *Server) writeCursorsListError(w http.ResponseWriter, r *http.Request, listCtx context.Context, err error) {
	if clientAborted(r, err) {
		return
	}
	if handlerTimedOut(listCtx, err) {
		s.logger.Warn("cursors list: deadline exceeded", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/cursors-timeout",
			"Cursors listing timed out", http.StatusServiceUnavailable,
			"the ingestion_cursors scan didn't return in 5s; retry shortly.")
		return
	}
	if transientStorageErr(err) {
		s.logger.Warn("cursors list: transient storage error", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/cursors-transient",
			"Cursors temporarily unavailable", http.StatusServiceUnavailable,
			"the storage layer hit a transient error; retry shortly.")
		return
	}
	s.logger.Warn("cursors list", "err", err)
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/cursors-error",
		"Cursors listing failed", http.StatusInternalServerError,
		"Storage layer returned an error.")
}
