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
//
// Two axes (ADR-0033/ADR-0034 two-axis verdict, decision brief
// notes/DECISION-genesis-complete-verdict-2026-07-16.md Option B):
//   - LakeComplete: the certified ClickHouse ARCHIVE is contiguous +
//     hash-chained + recognition-complete from genesis to tip
//     (substrate ∧ recognition only).
//   - Complete: the SERVED tier additionally reconciles against that
//     archive within its retention window (substrate ∧ recognition ∧
//     projection). Postgres is the served/working-set tier, not the
//     archive (ADR-0034), so Complete can be false for a source whose
//     LakeComplete is true.
type CoverageVerdictView struct {
	// Source is the logical source name (soroswap, blend, sdex, …) —
	// the same identifiers /v1/sources uses.
	Source string `json:"source"`
	// Complete is the SERVED/combined verdict: substrate ∧ recognition ∧
	// projection, all holding from genesis to the watermark. Projection
	// reconcile is retention-scoped by design (ADR-0034: Postgres is the
	// served tier, not the archive), so Complete for a trade-emitting
	// source reflects only what's PROJECTED into the served tier — it
	// can be false even when the certified ClickHouse archive is
	// genesis-complete. See LakeComplete for that claim.
	Complete bool `json:"complete"`
	// LakeComplete is the LAKE (archive) axis: substrate ∧ recognition
	// only, genesis-to-tip, decoupled from the retention-scoped
	// projection reconcile. This is "the certified ClickHouse archive is
	// contiguous + hash-chained + recognition-complete from genesis to
	// tip for this source's domain" — the two-axis verdict from
	// notes/DECISION-genesis-complete-verdict-2026-07-16.md (Option B).
	// A source can be lake_complete=true, complete=false: the archive is
	// genesis-proven even though the served tier only holds a retention
	// window of it.
	LakeComplete bool `json:"lake_complete"`
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
	// CompleteSources / TotalSources summarize the headline ("15/15") for
	// the served/combined axis (Complete).
	CompleteSources int `json:"complete_sources"`
	TotalSources    int `json:"total_sources"`
	// LakeCompleteSources tallies the lake (archive) axis (LakeComplete)
	// — how many sources' certified ClickHouse archive is proven
	// genesis-complete, independent of the served tier's retention
	// window. See CoverageVerdictView.LakeComplete.
	LakeCompleteSources int `json:"lake_complete_sources"`
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
			LakeComplete:       sn.LakeComplete,
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
		if sn.LakeComplete {
			view.LakeCompleteSources++
		}
	}
	view.TotalSources = len(view.Sources)

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, view, Flags{})
}
