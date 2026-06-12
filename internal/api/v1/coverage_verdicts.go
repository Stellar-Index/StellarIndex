package v1

import (
	"net/http"
	"time"
)

// CoverageVerdictView is the wire shape of one source's row on
// GET /v1/coverage — the public projection of the ADR-0033
// completeness verdict (substrate continuity + recognition +
// projection reconciliation), straight from completeness_snapshots.
//
// This endpoint is the API half of the product's trust story: the
// explorer's Coverage center renders it, and API consumers can audit
// the same claim the demo makes ("every protocol, verified complete")
// rather than taking a marketing badge on faith.
type CoverageVerdictView struct {
	// Source is the logical source name (soroswap, blend, sdex, …) —
	// the same identifiers /v1/sources uses.
	Source string `json:"source"`
	// Complete is the headline verdict: all three claims hold from
	// genesis to the watermark.
	Complete bool `json:"complete"`
	// SubstrateOK / RecognitionOK / ProjectionOK are the three ADR-0033
	// claims, reported separately so a consumer can see WHICH claim
	// failed when Complete is false.
	SubstrateOK   bool `json:"substrate_ok"`
	RecognitionOK bool `json:"recognition_ok"`
	ProjectionOK  bool `json:"projection_ok"`
	// GenesisLedger is the first ledger this source could have data at
	// (WASM-audit sourced); WatermarkLedger is the highest ledger the
	// verdict covers. TipLedger is the network tip at compute time.
	GenesisLedger   uint32 `json:"genesis_ledger"`
	WatermarkLedger uint32 `json:"watermark_ledger"`
	TipLedger       uint32 `json:"tip_ledger"`
	// CoveragePct is watermark progress vs tip — 100 means the verdict
	// reaches the tip at compute time.
	CoveragePct float64 `json:"coverage_pct"`
	// FirstProblemLedger is the first ledger with a verified problem
	// (0 when none) and Detail the human-readable problem description.
	FirstProblemLedger uint32 `json:"first_problem_ledger,omitempty"`
	Detail             string `json:"detail,omitempty"`
	// ComputedAt is when the audit run produced this verdict.
	ComputedAt time.Time `json:"computed_at"`
}

// CoverageVerdictsView is the envelope data field of GET /v1/coverage.
type CoverageVerdictsView struct {
	// Sources lists every audited source's verdict, source-sorted.
	Sources []CoverageVerdictView `json:"sources"`
	// CompleteSources / TotalSources summarize the headline ("15/15").
	CompleteSources int `json:"complete_sources"`
	TotalSources    int `json:"total_sources"`
}

// handleCoverageVerdicts serves GET /v1/coverage — every source's
// latest ADR-0033 completeness verdict. Verdicts change only when the
// audit runs (manually or on its timer), so a 60s public cache is
// generous to edges without hiding anything meaningful.
func (s *Server) handleCoverageVerdicts(w http.ResponseWriter, r *http.Request) {
	if s.completenessReader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/coverage-unavailable",
			"Coverage verdicts not available", http.StatusServiceUnavailable,
			"this deployment has no CompletenessReader wired — check binary configuration")
		return
	}
	snaps, err := s.completenessReader.ListCompletenessSnapshots(r.Context())
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("coverage verdicts read failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	view := CoverageVerdictsView{Sources: make([]CoverageVerdictView, 0, len(snaps))}
	for _, sn := range snaps {
		view.Sources = append(view.Sources, CoverageVerdictView{
			Source:             sn.Source,
			Complete:           sn.Complete,
			SubstrateOK:        sn.SubstrateOK,
			RecognitionOK:      sn.RecognitionOK,
			ProjectionOK:       sn.ProjectionOK,
			GenesisLedger:      sn.Genesis,
			WatermarkLedger:    sn.Watermark,
			TipLedger:          sn.Tip,
			CoveragePct:        sn.CoveragePct,
			FirstProblemLedger: sn.FirstProblem,
			Detail:             sn.Detail,
			ComputedAt:         sn.ComputedAt,
		})
		if sn.Complete {
			view.CompleteSources++
		}
	}
	view.TotalSources = len(view.Sources)

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, view, Flags{})
}
