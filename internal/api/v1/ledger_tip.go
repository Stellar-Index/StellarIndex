package v1

import (
	"context"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// LedgerTipView is the /v1/ledger/tip shape and /v1/ledger/stream event data: the highest committed ledger from the `ledgerstream` cursor, fresher than diagnostics' prices_1m-derived latest_ledger.
type LedgerTipView struct {
	LatestLedger uint32   `json:"latest_ledger"`
	IngestedAt   WireTime `json:"ingested_at"`
	LagSeconds   int64    `json:"lag_seconds"`
}

// handleLedgerTip serves GET /v1/ledger/tip, a light poll target avoiding the expensive /v1/diagnostics/ingestion snapshot.
// Cache-Control max-age=2 (cursor advances ~5s) absorbs a refreshing status page without hiding a stall; push clients use
// /v1/ledger/stream.
func (s *Server) handleLedgerTip(w http.ResponseWriter, r *http.Request) {
	if s.Cursors == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/ledger-tip-unavailable",
			"Ledger tip not available", http.StatusServiceUnavailable,
			"this deployment has no CursorsReader wired — check binary configuration")
		return
	}

	view, ok, err := s.ledgerTip(r.Context())
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("ledgerTip failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/ledger-tip-unavailable",
			"Ledger tip not available", http.StatusServiceUnavailable,
			"the live-ingest cursor has not been established yet — the indexer "+
				"has not committed its first ledger on this deployment")
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=2")
	writeJSON(w, view, Flags{})
}

// ledgerTip is the shared core of [Server.handleLedgerTip] and the /v1/ledger/stream producer. ok=false with no error means the
// ledgerstream cursor row does not exist yet (cold start); a non-nil error means the cursors read failed.
func (s *Server) ledgerTip(ctx context.Context) (LedgerTipView, bool, error) {
	rows, err := s.Cursors.ListCursors(ctx)
	if err != nil {
		return LedgerTipView{}, false, err
	}
	c, ok := findLedgerstreamCursor(rows)
	if !ok {
		return LedgerTipView{}, false, nil
	}
	lag := int64(time.Since(c.UpdatedAt).Seconds())
	if lag < 0 {
		// Clock skew between the API host and the DB server: never
		// report a negative lag.
		lag = 0
	}
	return LedgerTipView{
		LatestLedger: c.LastLedger,
		IngestedAt:   WireTime(c.UpdatedAt.UTC()),
		LagSeconds:   lag,
	}, true, nil
}

// findLedgerstreamCursor picks the live-ingest cursor out of a ListCursors result: (source = "ledgerstream", sub_source = "");
// backfill cursors share the table with a non-empty sub_source. Mirrors ledgerStreamLagSeconds.
func findLedgerstreamCursor(rows []timescale.Cursor) (timescale.Cursor, bool) {
	for _, c := range rows {
		if c.Source == cursorSourceLedgerstream && c.Sub == "" {
			return c, true
		}
	}
	return timescale.Cursor{}, false
}

// cursorSourceLedgerstream is the ingestion_cursors.source value the
// live indexer writes (cmd/stellarindex-indexer's cursorSource const).
const cursorSourceLedgerstream = "ledgerstream"
