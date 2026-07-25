package v1

import (
	"context"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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
	// CoveragePct is watermark progress vs tip, as a FRACTION in
	// [0,1] — 1.0 (not 100) means the verdict reaches the tip at
	// compute time. The name is a legacy misnomer kept for wire
	// compatibility; the value is what completeness.Watermark
	// computes, (Ledger-Genesis+1)/(Tip-Genesis+1), clamped.
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

// coverageVerdictStaleLedgers bounds how far the LIVE ingest frontier
// may run past a verdict's tip_ledger before the verdict stops being a
// claim about the CURRENT chain.
//
// The completeness audit runs hourly
// (deploy/systemd/stellarindex-completeness.timer: OnUnitActiveSec=1h
// plus up to 5 min of jitter plus a multi-minute run), and pubnet
// closes a ledger every ~5 s → ~720 ledgers/hour. 2160 ≈ 3 h of
// ledgers: three audit periods, so one skipped or slow run can't flap
// the flag, while an audit that has STOPPED surfaces within a few
// hours instead of never.
const coverageVerdictStaleLedgers uint32 = 2160

// coverageVerdictStaleAge is the wall-clock twin of
// [coverageVerdictStaleLedgers] — the same three-audit-period horizon
// expressed in time, so the gate still has an opinion on a deployment
// with no CursorsReader wired (or before the ledgerstream cursor
// exists) and on the pathological case where the live tip itself is
// frozen alongside a stalled audit.
const coverageVerdictStaleAge = 3 * time.Hour

// handleCoverageVerdicts serves GET /v1/coverage — every source's
// latest ADR-0033 completeness verdict. Verdicts change only when the
// audit runs (manually or on its timer), so a 60s public cache is
// generous to edges without hiding anything meaningful.
//
// `flags.stale` carries the live-tip gate (MNY-04): true when the
// published verdicts no longer describe the current chain. See
// [Server.coverageVerdictsStale] — without it this surface served
// "15/15 complete" forever after the audit died, since every field on
// the row (including tip_ledger and coverage_pct) is frozen at the
// verdict's own compute time and reads perfectly healthy in isolation.
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
	writeJSON(w, view, Flags{Stale: s.coverageVerdictsStale(r.Context(), snaps)})
}

// coverageVerdictsStale is the live-tip gate on the completeness
// verdicts (MNY-04 / audit A-H-4). It answers the only question a
// consumer of a trust surface actually has — "is this `complete: true`
// a statement about the chain as it is NOW?" — which the rows
// themselves cannot answer: `tip_ledger`, `coverage_pct` and every
// claim boolean are stamped at the audit's compute time, so a verdict
// from a dead audit still reads `coverage_pct: 1` ("verified to tip")
// against a tip that is hours behind the network.
//
// Two independent signals, OR'd (fail-closed — either alone is enough
// to say the response is below the surface's baseline contract, which
// is what `flags.stale` means per ADR-0018):
//
//   - LEDGER GAP: the live ingest frontier has advanced more than
//     [coverageVerdictStaleLedgers] past the verdict's own tip. This is
//     an apples-to-apples comparison: compute-completeness resolves its
//     `tip` from the SAME ledgerstream cursor
//     (internal/ops/chops/compute_completeness.go), so the difference
//     is exactly "how many ledgers have closed since this verdict was
//     computed".
//   - VERDICT AGE: computed_at older than [coverageVerdictStaleAge], or
//     absent entirely (an unknown-age verdict cannot be claimed fresh).
//
// A verdict list that is EMPTY is not flagged: there is no claim to
// qualify, and the summary counts (0/0) already say so.
//
// The gate degrades rather than fails: no CursorsReader, no
// ledgerstream cursor yet, or a slow/failing cursor read leaves the
// ledger-gap signal unavailable and the age signal alone decides.
func (s *Server) coverageVerdictsStale(ctx context.Context, snaps []timescale.CompletenessSnapshot) bool {
	if len(snaps) == 0 {
		return false
	}
	liveTip, haveTip := s.liveTipLedger(ctx)
	now := time.Now()
	for _, sn := range snaps {
		if haveTip && liveTip > sn.Tip && liveTip-sn.Tip > coverageVerdictStaleLedgers {
			return true
		}
		if sn.ComputedAt.IsZero() || now.Sub(sn.ComputedAt) > coverageVerdictStaleAge {
			return true
		}
	}
	return false
}

// liveTipLedger returns the live ingest frontier — the ledgerstream
// cursor's last ledger, the same value /v1/ledger/tip serves and the
// same one compute-completeness resolves its tip from. ok=false when no
// CursorsReader is wired, the cursor row doesn't exist yet, or the read
// failed; callers must degrade rather than fail, since this is a
// freshness annotation on someone else's response.
//
// The read is bounded at 5s — matching /v1/diagnostics/cursors' own
// ListCursors ceiling — so a slow Postgres can't hold a public GET open
// past the point where the annotation is worth waiting for.
func (s *Server) liveTipLedger(ctx context.Context) (uint32, bool) {
	if s.cursors == nil {
		return 0, false
	}
	tipCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	view, ok, err := s.ledgerTip(tipCtx)
	if err != nil {
		s.logger.Debug("live tip read failed — freshness gate falls back to verdict age", "err", err)
		return 0, false
	}
	if !ok {
		return 0, false
	}
	return view.LatestLedger, true
}
