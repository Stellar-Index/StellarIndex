package archivecompleteness

import (
	"encoding/json"
	"io"
	"time"
)

// Report is the JSON wire shape produced by `check` and consumed by `fix`, which fetches the
// missing files via the multi-source fallback chain.
//
// The Schema field locks the wire shape: new optional fields are fine, but removing or
// repurposing one bumps Schema and goes through an ADR amendment.
type Report struct {
	// Schema is the report's wire-format version, currently "1".
	Schema string `json:"schema"`

	// ScannedAt is when this report was produced, UTC.
	ScannedAt time.Time `json:"scanned_at"`

	// Range is the ledger-sequence range checked across all archives.
	Range RangeSpec `json:"range"`

	// CrossAnchor holds the cross-anchor scan results; nil when `-checks` skipped it.
	CrossAnchor *CrossAnchorReportSection `json:"cross_anchor,omitempty"`

	// Primary holds the primary (galexie-archive) scan results; nil because only the cross-anchor
	// archive is enforced today.
	Primary *PrimaryReportSection `json:"primary,omitempty"`
}

// RangeSpec is a `[from, to]` ledger range, both inclusive.
type RangeSpec struct {
	From uint32 `json:"from"`
	To   uint32 `json:"to"`
}

// CrossAnchorReportSection is the per-archive section for the cross-anchor scan.
type CrossAnchorReportSection struct {
	// ArchiveRoot is the absolute path scanned, so tooling knows which mount the report describes.
	ArchiveRoot string `json:"archive_root"`

	// Expected / Found / MissingCount mirror [CrossAnchorResult].
	Expected     int      `json:"expected"`
	Found        int      `json:"found"`
	MissingCount int      `json:"missing_count"`
	Missing      []uint32 `json:"missing,omitempty"`
	Truncated    bool     `json:"truncated,omitempty"`
}

// PrimaryReportSection is the per-archive section for the primary galexie-archive scan. Nothing
// populates it yet, so [Report.Primary] is always nil.
type PrimaryReportSection struct {
	BucketName   string `json:"bucket_name"`
	Expected     int    `json:"expected"`
	Found        int    `json:"found"`
	MissingCount int    `json:"missing_count"`
	// MissingRanges holds galexie detect-gaps (start, end) ranges, inclusive on both ends.
	MissingRanges []GapRange `json:"missing_ranges,omitempty"`
}

// GapRange is one contiguous missing-ledger range.
type GapRange struct {
	Start uint32 `json:"start"`
	End   uint32 `json:"end"`
}

// NewReport returns an empty Report for the given range; callers fill the sections per scan.
func NewReport(from, to uint32) *Report {
	return &Report{
		Schema:    "1",
		ScannedAt: time.Now().UTC(),
		Range:     RangeSpec{From: from, To: to},
	}
}

// SetCrossAnchor populates the cross-anchor section from a [CrossAnchorResult].
func (r *Report) SetCrossAnchor(archiveRoot string, res CrossAnchorResult) {
	r.CrossAnchor = &CrossAnchorReportSection{
		ArchiveRoot:  archiveRoot,
		Expected:     res.Expected,
		Found:        res.Found,
		MissingCount: res.Expected - res.Found,
		Missing:      res.Missing,
		Truncated:    res.Truncated,
	}
}

// WriteJSON encodes the report to w as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// AnyMissing reports whether either archive section shows missing files.
func (r *Report) AnyMissing() bool {
	if r.CrossAnchor != nil && r.CrossAnchor.MissingCount > 0 {
		return true
	}
	if r.Primary != nil && r.Primary.MissingCount > 0 {
		return true
	}
	return false
}

// Vacuous reports whether every POPULATED section scanned ZERO expected checkpoint positions. A
// range containing no checkpoint yields Expected=0, Found=0, MissingCount=0, which AnyMissing()
// reads as clean though nothing was verified. Callers must treat a vacuous report as a distinct
// failure and must NOT stamp a "last success" watermark on it.
//
// One section with Expected > 0 makes the report non-vacuous. A report with no populated section
// is also non-vacuous (no scan ran); Vacuous only catches "we scanned, but there was nothing to
// scan".
func (r *Report) Vacuous() bool {
	populated := false
	if r.CrossAnchor != nil {
		populated = true
		if r.CrossAnchor.Expected > 0 {
			return false
		}
	}
	if r.Primary != nil {
		populated = true
		if r.Primary.Expected > 0 {
			return false
		}
	}
	return populated
}
