package v1

import (
	"context"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
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
	// ProjectionVerifiedFrom is the PROJECTION axis's floor: the lowest
	// ledger the served tier holds any row at for this source. It is the
	// bottom of the range ProjectionOK — and therefore Complete — is a
	// claim about; below it the served tier holds nothing at all.
	//
	// It is NOT GenesisLedger, which is the lake axis's floor and is
	// routinely ten years lower: on pubnet, sdex and the oracle sources
	// publish genesis_ledger 2 with a served tier that begins ~61.6M.
	// Reading complete/coverage_pct/genesis_ledger without this field
	// overstates the served claim by that whole span. The audit's own
	// `detail` string has always named the range in prose; this is the
	// same number, typed.
	//
	// Omitted when the audit recorded no floor — a verdict written
	// before the field existed, or a run whose projection axis was not
	// evaluated because an earlier claim already failed at genesis
	// (`projection_ok` is false in that case). Absent means UNKNOWN, not
	// "from ledger 0".
	ProjectionVerifiedFrom uint32 `json:"projection_verified_from,omitempty"`
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

// NotApplicableSourceView is one source that does not exist on the
// serving network (#483).
type NotApplicableSourceView struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// RecognitionAxisView is the ADR-0033 RECOGNITION audit axis: distinct
// on-chain event shapes in the certified lake that sit on contracts NO
// indexed source owns.
//
// It is deliberately NOT a [CoverageVerdictView]. It used to be one —
// compute-completeness writes it to the same completeness_snapshots
// table under the reserved source name "recognition", so it arrived in
// `sources[]` and the public headline counted it as a 21st source that
// had failed. Every per-source field on it was a fiction of that shape:
// substrate_ok/projection_ok were hardcoded true, complete/lake_complete
// were false PERMANENTLY BY CONSTRUCTION (they can only be true if no
// un-indexed Soroban contract exists anywhere on the network), and
// coverage_pct measured ledgers-until-the-first-foreign-contract, a
// number that only ever decreases. A permanently-red row in a
// completeness board teaches every reader to ignore the board.
//
// So it gets its own vocabulary here, at the top level, with the
// numbers the audit actually produces. The deployed metrics already
// draw this line — see the two `WHERE source <> 'recognition'` clauses
// in configs/ansible/roles/archival-node/files/data-freshness.sh
// (PR #465) — this is the public API agreeing with them.
//
// What it does NOT mean: it is not missing data and not a gap in any
// source we publish. A source silently dropping its OWN events is a
// different bucket entirely — that surfaces as `recognition_ok: false`
// on THAT source's row in Sources, and fails the headline.
type RecognitionAxisView struct {
	// AllShapesRecognized is true when every event shape in the audited
	// lake belongs to a contract some indexed source owns — i.e. the
	// census is empty. False on any public network with protocols we
	// have not integrated, which is the expected steady state.
	AllShapesRecognized bool `json:"all_shapes_recognized"`
	// UnrecognizedShapes / UnrecognizedContracts are the census: how
	// many distinct (contract, topic) event shapes are unattributed,
	// across how many distinct contracts.
	//
	// Omitted — NOT zeroed — when they cannot be read from the audit's
	// stored detail (a snapshot written before this format existed).
	// "0 unrecognized shapes" is a claim of cleanliness; inventing it
	// from a parse failure would be exactly the laundering this axis
	// exists to prevent. Detail is still served verbatim.
	UnrecognizedShapes    *int `json:"unrecognized_shapes,omitempty"`
	UnrecognizedContracts *int `json:"unrecognized_contracts,omitempty"`
	// EarliestLedger is the lowest ledger an unattributed shape was
	// first seen at (0/omitted when the census is empty);
	// ScannedFromLedger is the audit's floor — the start of the Soroban
	// era, since before it no contract events existed at all.
	EarliestLedger    uint32 `json:"earliest_ledger,omitempty"`
	ScannedFromLedger uint32 `json:"scanned_from_ledger"`
	// TipLedger is the network tip the audit ran against.
	TipLedger uint32 `json:"tip_ledger"`
	// Meaning is a plain-language statement of what this axis is and is
	// not, served on the wire rather than only in the OpenAPI spec: a
	// consumer building a dashboard from this JSON never reads the
	// spec, and this number is easy to mistake for a data gap.
	Meaning string `json:"meaning"`
	// Detail is the audit's own description, verbatim.
	Detail string `json:"detail,omitempty"`
	// ComputedAt is when the audit run produced this census.
	ComputedAt time.Time `json:"computed_at"`
}

// recognitionAxisMeaning is [RecognitionAxisView.Meaning]. Constant
// prose, deliberately: it describes the axis, not the reading.
const recognitionAxisMeaning = "Event shapes on Soroban contracts that no indexed source owns — " +
	"protocols Stellar Index has not integrated. This is a DISCOVERY BACKLOG (which decoder to build next), " +
	"not missing data: no source we publish is dropping events because of it, and it can never reach zero on a " +
	"public network, so it is reported as its own audit axis and is excluded from complete_sources / total_sources. " +
	"A source silently dropping its OWN events is the opposite case and appears as recognition_ok=false on that " +
	"source's row in `sources`, where it does fail the headline."

// CoverageVerdictsView is the envelope data field of GET /v1/coverage.
type CoverageVerdictsView struct {
	// Sources lists every audited SOURCE's verdict, source-sorted.
	// System audit axes (completeness.IsAuditAxis) are not sources and
	// are reported in their own fields — see Recognition.
	Sources []CoverageVerdictView `json:"sources"`
	// Recognition is the ADR-0033 recognition audit axis, or null when
	// the audit has not produced one on this deployment. The key is
	// always present so its ABSENCE is visible rather than silent.
	Recognition *RecognitionAxisView `json:"recognition"`
	// CompleteSources / TotalSources summarize the headline ("20/20") for
	// the served/combined axis (Complete). SOURCES ONLY — a system audit
	// axis is not a source and is neither numerator nor denominator.
	// TotalSources always equals len(Sources).
	CompleteSources int `json:"complete_sources"`
	TotalSources    int `json:"total_sources"`
	// LakeCompleteSources tallies the lake (archive) axis (LakeComplete)
	// — how many sources' certified ClickHouse archive is proven
	// genesis-complete, independent of the served tier's retention
	// window. See CoverageVerdictView.LakeComplete.
	LakeCompleteSources int `json:"lake_complete_sources"`
	// Network is the Stellar network this deployment serves (pubnet /
	// testnet / futurenet). Protocol sources are anchored to pubnet
	// contract identities (ADR-0035), so on a test net they do not
	// exist — they are listed in NotApplicableSources with a reason and
	// EXCLUDED from Sources and every total, instead of being counted
	// incomplete by construction (#483).
	Network string `json:"network"`
	// NotApplicableSources names the sources that do not exist on this
	// network. Always empty on pubnet.
	NotApplicableSources []NotApplicableSourceView `json:"not_applicable_sources"`
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
// CALIBRATED TO THE DEPLOYED CADENCE (2026-07-26): the original 2160
// (~3h) assumed an hourly compute-completeness timer; r1's timer is
// DAILY (07:32), so a 3h bound would read stale ~90% of every day —
// noisy-honest at best. 34560 ≈ 2 audit periods at the daily cadence:
// one whole missed run plus most of a second before the flag trips,
// which is the "the audit stopped" signal this gate exists for rather
// than "the audit hasn't run yet today".
const coverageVerdictStaleLedgers uint32 = 34560

// coverageVerdictStaleAge is the wall-clock twin of
// [coverageVerdictStaleLedgers] — the same three-audit-period horizon
// expressed in time, so the gate still has an opinion on a deployment
// with no CursorsReader wired (or before the ledgerstream cursor
// exists) and on the pathological case where the live tip itself is
// frozen alongside a stalled audit.
// 26h = the daily cadence plus a two-hour grace: catches a missed run
// on the first morning it fails, without flagging the ordinary gap
// between yesterday's run and today's. Same calibration note as above.
const coverageVerdictStaleAge = 26 * time.Hour

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

	network := s.network
	if network == "" {
		network = sourcenet.Pubnet
	}
	view := CoverageVerdictsView{
		Sources:              make([]CoverageVerdictView, 0, len(snaps)),
		Network:              network,
		NotApplicableSources: make([]NotApplicableSourceView, 0),
	}
	for _, na := range sourcenet.NotApplicableOn(network) {
		view.NotApplicableSources = append(view.NotApplicableSources, NotApplicableSourceView{Source: na.Source, Reason: na.Reason})
	}
	for _, sn := range snaps {
		// A stale pubnet-only row (written before the catalogue was
		// network-scoped) must not count against a test net.
		if ok, _ := sourcenet.Applicable(sn.Source, network); !ok {
			continue
		}
		// A system audit axis is not a source: it has no substrate,
		// projection or retention window, so neither `complete` axis is
		// defined for it and it belongs in neither total. It is
		// published in full below, not dropped.
		if completeness.IsAuditAxis(sn.Source) {
			view.Recognition = recognitionAxisView(sn)
			continue
		}
		view.Sources = append(view.Sources, CoverageVerdictView{
			Source:                 sn.Source,
			Complete:               sn.Complete,
			LakeComplete:           sn.LakeComplete,
			SubstrateOK:            sn.SubstrateOK,
			RecognitionOK:          sn.RecognitionOK,
			ProjectionOK:           sn.ProjectionOK,
			GenesisLedger:          sn.Genesis,
			WatermarkLedger:        sn.Watermark,
			TipLedger:              sn.Tip,
			ProjectionVerifiedFrom: sn.ProjectionVerifiedFrom,
			CoveragePct:            sn.CoveragePct,
			FirstProblemLedger:     sn.FirstProblem,
			Detail:                 sn.Detail,
			ComputedAt:             sn.ComputedAt,
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

// recognitionAxisView projects the system recognition snapshot onto its
// own wire shape, publishing only the fields that are real for a
// census. The per-source booleans the snapshot carries
// (substrate_ok/projection_ok, hardcoded true by the computor;
// complete/lake_complete, false by construction) and coverage_pct are
// deliberately NOT re-exported: they were never claims about this axis.
func recognitionAxisView(sn timescale.CompletenessSnapshot) *RecognitionAxisView {
	v := &RecognitionAxisView{
		AllShapesRecognized: sn.RecognitionOK,
		EarliestLedger:      sn.FirstProblem,
		ScannedFromLedger:   sn.Genesis,
		TipLedger:           sn.Tip,
		Meaning:             recognitionAxisMeaning,
		Detail:              sn.Detail,
		ComputedAt:          sn.ComputedAt,
	}
	// Typed counts only when the audit's own format parses; otherwise
	// omitted, never zeroed (see RecognitionAxisView.UnrecognizedShapes).
	if c, ok := completeness.ParseRecognitionDetail(sn.Detail); ok {
		shapes, contracts := c.Shapes, c.Contracts
		v.UnrecognizedShapes, v.UnrecognizedContracts = &shapes, &contracts
	}
	return v
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
