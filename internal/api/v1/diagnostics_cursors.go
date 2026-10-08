package v1

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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
//
// State is the derived lifecycle marker (see [cursorStateFor]) — the
// field that tells a dead one-shot shard apart from a live position
// without the reader having to do age arithmetic against two
// thresholds it has no way to know.
type Cursor struct {
	Source      string `json:"source"`
	SubSource   string `json:"sub_source,omitempty"`
	LastLedger  uint32 `json:"last_ledger"`
	LastUpdated string `json:"last_updated"`
	LagSeconds  int64  `json:"lag_seconds"`
	State       string `json:"state"`
}

// statusActiveMaxAge is the lag-seconds ceiling that the
// `?status=active` filter uses to distinguish a live, actively-
// writing cursor from a stale / completed one. 10 minutes is a
// generous-but-not-excessive window for the live indexer
// (production cursor updates every ~5s) and reliably excludes
// completed backfill cursors that linger in the table for days
// or weeks before manual cleanup.
const statusActiveMaxAge = 10 * time.Minute

// cursorAbandonedAge is the boundary past which a cursor in a one-shot
// job's namespace is reported as `abandoned` and dropped from the
// DEFAULT response. It does not apply to the live namespaces — see
// [cursorStateFor].
//
// Nothing in this system checkpoints weekly: the live indexer writes a
// cursor every ~5s, the projector every cycle, and a backfill shard
// that is still walking advances continuously. A row untouched for a
// week is therefore a record of FINISHED OR ABANDONED work, not a
// position anything is still writing to — `ingestion_cursors` has no
// retention policy, so those records accumulate forever.
//
// Measured on r1, 4,703 of 4,815 rows were past this line, among them
// SDEX backfill shards carrying a ~9.7M-second lag. Unfiltered, every
// public request would carry all of them (~520 KB before compression),
// with nothing on the wire to say which of them anything is still
// working on.
const cursorAbandonedAge = 7 * 24 * time.Hour

// Cursor lifecycle states. Derived, not stored — `ingestion_cursors`
// has no state column, and a cursor's own writer has no chance to mark
// it dead (a shard that is abandoned is, by definition, one whose
// process went away without finishing).
const (
	cursorStateLive      = "live"      // lag <= statusActiveMaxAge — something is writing this row
	cursorStateStale     = "stale"     // behind, but a position something is still expected to advance
	cursorStateAbandoned = "abandoned" // past cursorAbandonedAge in a one-shot job's namespace — finished or dead work
)

// Response bound for /v1/diagnostics/cursors. The row count is
// operationally unbounded — one row per (source, sub_source) with no
// retention — and every ops job that shards its work mints rows in the
// thousands (projected-rebuild alone accounted for 4,523 of r1's
// 4,815). Excluding the abandoned set is what makes the DEFAULT
// response small; this cap is what stops any single response growing
// without limit, including `?include_abandoned=true`, which pages
// via `pagination.next`.
const (
	cursorsDefaultLimit = 500
	cursorsMaxLimit     = 2000
)

// cursorStateFor derives a row's lifecycle state from its source and
// its lag. The two thresholds are the same ones `?status=` filters on,
// so the state field and the filters can never disagree.
//
// A row in a live cursor namespace ([timescale.LiveCursorSources]) is
// never `abandoned`, at any age. Nothing abandons the live indexer's
// or the projector's own resume point, so an old row there means ingest
// is STUCK — the single condition this endpoint exists to surface.
// Ageing it out of the default response (and out of `?status=stale`,
// documented as the view for spotting dead ingest paths) would hide the
// incident behind the cleanup. It is also the same rule `reap-cursors`
// applies to the same list, from the same premise: those rows are
// resume state, not a record of finished work.
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

// cursorsQuery is the parsed + validated query string of one
// /v1/diagnostics/cursors request. Built by parseCursorsQuery so
// handleCursors stays a fetch-filter-page function.
type cursorsQuery struct {
	maxAge           time.Duration
	statusStale      bool
	statusAbandoned  bool
	source           string
	includeAbandoned bool
	limit            int
	offset           int
}

// keeps reports whether a row survives every filter this query
// carries. `state` and `lag` describe the same row; both are passed
// so the abandoned test reads off the published state rather than
// re-deriving it.
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

// parseCursorsQuery validates every query param handleCursors accepts,
// writing the Problem+JSON response itself and returning ok=false when
// one is malformed.
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

	// include_abandoned is the explicit opt-in to the dead set. It is
	// opt-in rather than opt-out because the abandoned rows outnumber
	// the live ones by ~40:1 and describe work that ended months ago.
	q.includeAbandoned = r.URL.Query().Get("include_abandoned") == "true"

	// status: "active" / "stale" / "abandoned" / "" — semantic
	// convenience layer over max_age. Active = lag <= 10 min
	// (caps maxAge); stale = the complement, up to the abandoned
	// boundary; abandoned = the dead set alone, which implies the
	// opt-in so `?status=abandoned` needs no second parameter.
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

// handleCursors serves GET /v1/diagnostics/cursors; parameters are
// documented in the OpenAPI spec.
//
// The response defaults to non-abandoned rows: `ingestion_cursors` keeps one
// permanent row per one-shot job shard, which otherwise dominates it
// (measured on r1: 4,703 of 4,815 rows). Live namespaces are never
// `abandoned` at any age, so stuck ingest cannot drop out of the default
// view. Unknown `source` values return an empty array rather than 400, so a
// typo and a brand-new source look the same.
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

// pageCursors slices the filtered listing to one page and returns the
// `pagination.next` offset token, empty when the page is the last one.
// Always returns a non-nil slice so `data` marshals as [] rather than
// null on an over-the-end offset.
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

// writeCursorsListError maps a ListCursors error to the appropriate
// Problem+JSON response. Under cascade the
// /v1/diagnostics/cursors endpoint is exactly the operator's
// must-have view, and a generic 500 would not
// distinguish "postgres briefly stalled" (retry now) from "endpoint
// permanently broken" (escalate). Mapping transient + timeout
// shapes to 503 lets operators read the response without ambiguity.
//
// Extracted from handleCursors to keep that function under the
// gocognit ceiling — the seven-branch error map pushed it past 20.
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
