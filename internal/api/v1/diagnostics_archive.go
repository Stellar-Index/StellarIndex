package v1

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"time"
)

// ArchiveReportView is the wire shape of GET /v1/diagnostics/archive —
// the latest ADR-0017 archive-completeness report. It mirrors
// internal/archivecompleteness.Report field-for-field but is declared
// locally so the public wire contract stays decoupled from the
// daemon's internal struct (same rationale as Source vs
// external.Metadata in sources.go): the JSON tags below ARE the
// contract, and an internal-only field added to the daemon's Report
// won't leak onto the wire.
//
// The report is produced by `stellarindex-ops archive-completeness
// verify` (daily systemd timer) which writes its JSON report to the
// path the API serves here (Options.ArchiveReportPath). The API is a
// pure read-through: no re-scan, no freshness synthesis — `scanned_at`
// is the daemon's own timestamp, so a stale value is itself the signal
// that the timer stopped firing.
type ArchiveReportView struct {
	// Schema is the daemon's report wire-format version ("1").
	Schema string `json:"schema"`
	// ScannedAt is when the daemon produced this report (RFC 3339 UTC).
	ScannedAt time.Time `json:"scanned_at"`
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

// handleDiagnosticsArchive serves GET /v1/diagnostics/archive — the
// latest archive-completeness report (ADR-0017), read from the JSON
// file the daily `archive-completeness verify` timer writes.
//
// Degradation ladder:
//   - Options.ArchiveReportPath empty → 503 (deployment opted out).
//   - File doesn't exist → 404 (daemon hasn't produced a report yet —
//     a legitimate state on a fresh host, not a server bug).
//   - File unreadable / unparseable → 500 with a log line (the file
//     exists but something is wrong: permissions, torn write, schema
//     drift).
//
// Caching: covered by the `/v1/diagnostics/` prefix rule in
// middleware/cachecontrol.go (`private, no-cache, must-revalidate`).
func (s *Server) handleDiagnosticsArchive(w http.ResponseWriter, r *http.Request) {
	if s.archiveReportPath == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/archive-report-unavailable",
			"Archive report not available", http.StatusServiceUnavailable,
			"this deployment has no api.archive_report_path configured — the archive-completeness daemon's report is not served here")
		return
	}
	raw, err := os.ReadFile(s.archiveReportPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/archive-report-missing",
				"Archive report not found", http.StatusNotFound,
				"the archive-completeness daemon hasn't written a report yet — it runs on a daily timer, so a fresh deployment can legitimately be in this state")
			return
		}
		s.logger.Error("archive report read failed", "path", s.archiveReportPath, "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	var view ArchiveReportView
	if err := json.Unmarshal(raw, &view); err != nil {
		s.logger.Error("archive report parse failed", "path", s.archiveReportPath, "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	writeJSON(w, view, Flags{})
}
