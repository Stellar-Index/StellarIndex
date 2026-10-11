package v1

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
)

// ArchiveReportView is the wire shape of GET /v1/diagnostics/archive, the latest archive
// completeness report. It mirrors internal/archivecompleteness.Report but is declared locally
// so the JSON tags stay the contract and an internal-only field cannot leak onto the wire.
//
// The API is a pure read-through of the file the daily verify timer writes (no re-scan, no
// freshness synthesis): `scanned_at` is the daemon's own timestamp, so a stale value is itself
// the signal that the timer stopped firing.
type ArchiveReportView struct {
	// Schema is the daemon's report wire-format version ("1").
	Schema string `json:"schema"`
	// ScannedAt is when the daemon produced this report (RFC 3339 UTC).
	ScannedAt WireTime `json:"scanned_at"`
	// Range is the inclusive ledger range the daemon checked.
	Range ArchiveRangeView `json:"range"`
	// CrossAnchor holds the cross-anchor (/srv/history-archive) scan
	// results. Nil when the daemon skipped that check.
	CrossAnchor *ArchiveCrossAnchorView `json:"cross_anchor,omitempty"`
	// Primary holds the primary (galexie-archive) scan results. Nil in
	// the current daemon — only the cross-anchor archive is enforced
	// today (see internal/archivecompleteness/report.go).
	Primary *ArchivePrimaryView `json:"primary,omitempty"`
}

// ArchiveRangeView is an inclusive `[from, to]` ledger range.
type ArchiveRangeView struct {
	From uint32 `json:"from"`
	To   uint32 `json:"to"`
}

// ArchiveCrossAnchorView is the cross-anchor archive scan section.
type ArchiveCrossAnchorView struct {
	ArchiveRoot  string   `json:"archive_root"`
	Expected     int      `json:"expected"`
	Found        int      `json:"found"`
	MissingCount int      `json:"missing_count"`
	Missing      []uint32 `json:"missing,omitempty"`
	// Truncated is true when the daemon capped the Missing list
	// (MaxMissingReported); MissingCount stays accurate regardless.
	Truncated bool `json:"truncated,omitempty"`
}

// ArchivePrimaryView is the primary (galexie-archive) scan section.
type ArchivePrimaryView struct {
	BucketName    string                `json:"bucket_name"`
	Expected      int                   `json:"expected"`
	Found         int                   `json:"found"`
	MissingCount  int                   `json:"missing_count"`
	MissingRanges []ArchiveGapRangeView `json:"missing_ranges,omitempty"`
}

// ArchiveGapRangeView is one contiguous missing-ledger range,
// inclusive on both ends.
type ArchiveGapRangeView struct {
	Start uint32 `json:"start"`
	End   uint32 `json:"end"`
}

// handleDiagnosticsArchive serves GET /v1/diagnostics/archive from the report file.
//
// Degradation ladder:
//   - Options.ArchiveReportPath empty → 503 (deployment opted out).
//   - File missing → 404 (no report yet; legitimate on a fresh host, not a server bug).
//   - File unreadable / unparseable → 500 with a log line (permissions, torn write, drift).
//
// Caching is covered by the `/v1/diagnostics/` prefix rule in middleware/cachecontrol.go.
func (s *Server) handleDiagnosticsArchive(w http.ResponseWriter, r *http.Request) {
	if s.ArchiveReportPath == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/archive-report-unavailable",
			"Archive report not available", http.StatusServiceUnavailable,
			"this deployment has no api.archive_report_path configured — the archive-completeness daemon's report is not served here")
		return
	}
	raw, err := os.ReadFile(s.ArchiveReportPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/archive-report-missing",
				"Archive report not found", http.StatusNotFound,
				"the archive-completeness daemon hasn't written a report yet — it runs on a daily timer, so a fresh deployment can legitimately be in this state")
			return
		}
		s.logger.Error("archive report read failed", "path", s.ArchiveReportPath, "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	var view ArchiveReportView
	if err := json.Unmarshal(raw, &view); err != nil {
		s.logger.Error("archive report parse failed", "path", s.ArchiveReportPath, "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	writeJSON(w, view, Flags{})
}
