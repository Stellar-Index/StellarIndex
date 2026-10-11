package v1

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// CoverageVerdictView is one source's row on GET /v1/coverage: the public projection of the
// ADR-0033 completeness verdict from completeness_snapshots.
//
// Two axes (ADR-0033/ADR-0034):
//   - LakeComplete: the certified ClickHouse ARCHIVE is contiguous + hash-chained +
//     recognition-complete from genesis to tip (substrate and recognition only).
//   - Complete: the SERVED tier additionally reconciles against that archive within its
//     retention window. Postgres is the working-set tier, not the archive, so Complete can be
//     false while LakeComplete is true.
type CoverageVerdictView struct {
	// Source is the logical source name, the same identifiers /v1/sources uses.
	Source string `json:"source"`
	// Complete is the SERVED verdict: substrate, recognition and projection all holding from
	// genesis to the watermark. Projection reconcile is retention-scoped (Postgres is the served
	// tier), so it can be false while the ClickHouse archive is genesis-complete.
	Complete bool `json:"complete"`
	// LakeComplete is the archive axis: substrate and recognition only, genesis-to-tip, decoupled
	// from the retention-scoped projection reconcile.
	LakeComplete bool `json:"lake_complete"`
	// SubstrateOK / RecognitionOK / ProjectionOK are the three ADR-0033 claims, reported separately
	// so a consumer can see WHICH one failed when Complete is false.
	SubstrateOK   bool `json:"substrate_ok"`
	RecognitionOK bool `json:"recognition_ok"`
	ProjectionOK  bool `json:"projection_ok"`
	// GenesisLedger is the first ledger this source could have data at; WatermarkLedger the highest
	// ledger the verdict covers; TipLedger the network tip at compute time.
	GenesisLedger   uint32 `json:"genesis_ledger"`
	WatermarkLedger uint32 `json:"watermark_ledger"`
	TipLedger       uint32 `json:"tip_ledger"`
	// ProjectionVerifiedFrom is the floor of the range ProjectionOK (and so Complete) claims: the
	// lowest served row when the served tier is a declared working-set window, else GenesisLedger.
	// Ignoring it overstates the served claim. Omitted means UNKNOWN, not "from ledger 0".
	ProjectionVerifiedFrom uint32 `json:"projection_verified_from,omitempty"`
	// ProjectionReconciledFrom is the lowest ledger the producing run reconciled itself; the span
	// from ProjectionVerifiedFrom below it was carried from the prior verdict.
	ProjectionReconciledFrom uint32 `json:"projection_reconciled_from,omitempty"`
	// ProjectionEvidencedAt is when one run last reconciled the whole served range cleanly. Unlike
	// ComputedAt, it does not advance on a run that only carried the claim. null = no evidence.
	ProjectionEvidencedAt *WireTime `json:"projection_evidenced_at"`
	// CoveragePct is watermark progress vs tip as a FRACTION in [0,1], not 100: completeness.Watermark's
	// (Ledger-Genesis+1)/(Tip-Genesis+1), clamped.
	CoveragePct float64 `json:"coverage_pct"`
	// FirstProblemLedger is the first ledger with a verified problem (0 when none).
	FirstProblemLedger uint32 `json:"first_problem_ledger,omitempty"`
	Detail             string `json:"detail,omitempty"`
	// ComputedAt is when the audit run produced this verdict.
	ComputedAt WireTime `json:"computed_at"`
}

// NotApplicableSourceView is one source that does not exist on the serving network.
type NotApplicableSourceView struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// RecognitionAxisView is the ADR-0033 RECOGNITION audit axis: distinct on-chain event shapes in the
// certified lake that sit on contracts NO indexed source owns.
//
// It is deliberately NOT a [CoverageVerdictView], although it is stored in completeness_snapshots
// under the reserved source name "recognition": as a `sources[]` row the headline would count it
// as a failed source and its per-source fields would be fiction. A source dropping its OWN events
// surfaces as `recognition_ok: false` on THAT source's row, and fails the headline.
type RecognitionAxisView struct {
	// AllShapesRecognized is true when the census is empty; false is the expected steady state on
	// any public network with protocols we have not integrated.
	AllShapesRecognized bool `json:"all_shapes_recognized"`
	// UnrecognizedShapes / UnrecognizedContracts are the census: distinct unattributed (contract,
	// topic) event shapes, across how many contracts. Omitted, NOT zeroed, when the audit's stored
	// detail does not parse: "0" is a claim of cleanliness a parse failure must not invent.
	UnrecognizedShapes    *int `json:"unrecognized_shapes,omitempty"`
	UnrecognizedContracts *int `json:"unrecognized_contracts,omitempty"`
	// EarliestLedger is the lowest ledger an unattributed shape was first seen at; ScannedFromLedger
	// is the audit floor, the start of the Soroban era.
	EarliestLedger    uint32 `json:"earliest_ledger,omitempty"`
	ScannedFromLedger uint32 `json:"scanned_from_ledger"`
	// TipLedger is the network tip the audit ran against.
	TipLedger uint32 `json:"tip_ledger"`
	// Meaning is served on the wire because a dashboard built from this JSON never reads the spec,
	// and this number is easy to mistake for a data gap.
	Meaning string `json:"meaning"`
	// Detail is the audit's own description, verbatim.
	Detail string `json:"detail,omitempty"`
	// ComputedAt is when the audit run produced this census.
	ComputedAt WireTime `json:"computed_at"`
}

// recognitionAxisMeaning is [RecognitionAxisView.Meaning]: constant prose describing the axis, not
// the reading.
const recognitionAxisMeaning = "Event shapes on Soroban contracts that no indexed source owns — " +
	"protocols Stellar Index has not integrated. This is a DISCOVERY BACKLOG (which decoder to build next), " +
	"not missing data: no source we publish is dropping events because of it, and it can never reach zero on a " +
	"public network, so it is reported as its own audit axis and is excluded from complete_sources / total_sources. " +
	"A source silently dropping its OWN events is the opposite case and appears as recognition_ok=false on that " +
	"source's row in `sources`, where it does fail the headline — including a watched SEP-41 source dropping its " +
	"own classic-token transfer/mint/burn/… kinds, via a watched-contract-scoped census that audits them without " +
	"re-scanning the whole classic-token firehose for every source."

// CoverageVerdictsView is the envelope data field of GET /v1/coverage.
type CoverageVerdictsView struct {
	// Sources lists every audited SOURCE's verdict, source-sorted. System audit axes
	// (completeness.IsAuditAxis) are not sources and have their own fields (see Recognition).
	Sources []CoverageVerdictView `json:"sources"`
	// Recognition is the ADR-0033 recognition axis, or null when none exists on this deployment.
	// The key is always present so its ABSENCE is visible rather than silent.
	Recognition *RecognitionAxisView `json:"recognition"`
	// UsdVolumePricing is the USD volume pricing axis (valuation coverage over a fixed 24h window),
	// or null until the first background refresh. Outside every source count: it can never change
	// Complete or LakeComplete.
	UsdVolumePricing *UsdVolumePricingAxisView `json:"usd_volume_pricing"`
	// CompleteSources / TotalSources are the headline ("20/20") for the served axis. SOURCES ONLY:
	// an audit axis is neither numerator nor denominator. TotalSources is
	// len(Sources) + len(UnverifiedSources).
	CompleteSources int `json:"complete_sources"`
	TotalSources    int `json:"total_sources"`
	// LakeCompleteSources tallies the lake axis, independent of the served tier's retention window.
	LakeCompleteSources int `json:"lake_complete_sources"`
	// Network is the Stellar network this deployment serves. Protocol sources are anchored to pubnet
	// contract identities (ADR-0035), so on a test net they are listed in NotApplicableSources and
	// EXCLUDED from Sources and every total, not counted incomplete by construction.
	Network string `json:"network"`
	// NotApplicableSources names the sources that do not exist on this network (empty on pubnet).
	NotApplicableSources []NotApplicableSourceView `json:"not_applicable_sources"`
	// UnverifiedSources names applicable audited sources with NO verdict row. They count in
	// TotalSources, so a failed first audit shrinks the numerator rather than vanishing from both.
	UnverifiedSources []UnverifiedSourceView `json:"unverified_sources"`
	// LaggingSources names the sources whose verdict tip is below the newest one: the latest run
	// wrote no verdict for them, so the headline sums more than one run.
	LaggingSources []LaggingSourceView `json:"lagging_sources"`
}

// LaggingSourceView is one source still carrying an earlier run's verdict.
type LaggingSourceView struct {
	Source    string `json:"source"`
	TipLedger uint32 `json:"tip_ledger"`
	Reason    string `json:"reason"`
}

// laggingSources lists every verdict whose tip is below the newest. A run stamps every source it
// evaluates with one tip, so a lower tip is a row the newest run did not rewrite.
func laggingSources(sources []CoverageVerdictView) []LaggingSourceView {
	var newest uint32
	for _, v := range sources {
		newest = max(newest, v.TipLedger)
	}
	out := make([]LaggingSourceView, 0)
	for _, v := range sources {
		if v.TipLedger < newest {
			out = append(out, LaggingSourceView{
				Source:    v.Source,
				TipLedger: v.TipLedger,
				Reason: fmt.Sprintf("verdict computed at tip %d, below the newest source verdict's tip %d: "+
					"the latest audit run did not refresh it, so it is counted at its earlier tip", v.TipLedger, newest),
			})
		}
	}
	return out
}

// UnverifiedSourceView is one audited source with no verdict row.
type UnverifiedSourceView struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// unverifiedSourceReason is [UnverifiedSourceView.Reason].
const unverifiedSourceReason = "no completeness verdict has ever been computed for this source on this deployment " +
	"(its audit has not completed, or the row was cleared) — counted in total_sources, not as complete"

// unverifiedSources lists every audited source applicable on network with no row in snaps.
func unverifiedSources(audited []string, snaps []timescale.CompletenessSnapshot, network string) []UnverifiedSourceView {
	have := make(map[string]struct{}, len(snaps))
	for _, sn := range snaps {
		have[sn.Source] = struct{}{}
	}
	out := make([]UnverifiedSourceView, 0)
	for _, name := range audited {
		if _, ok := have[name]; ok {
			continue
		}
		if ok, _ := sourcenet.Applicable(name, network); !ok {
			continue
		}
		out = append(out, UnverifiedSourceView{Source: name, Reason: unverifiedSourceReason})
	}
	return out
}

// coverageVerdictStaleLedgers bounds how far the network tip may run past a verdict's tip_ledger
// before the verdict stops being a claim about the CURRENT chain. The audit is daily; 34560 ledgers
// is ~2 audit periods at 5 s/ledger, a backstop that tolerates a slow run without flapping. The age
// gate is the primary "the audit stopped" signal and fires first.
const coverageVerdictStaleLedgers uint32 = 34560

// coverageVerdictStaleAge is the wall-clock gate, deliberately tighter than the ledger gate: one
// daily period plus a two-hour grace flags a missed run the morning it fails. It decides alone when
// no cursor is available.
const coverageVerdictStaleAge = 26 * time.Hour

// coverageVerdictEvidenceStaleAge bounds the age of the evidence behind a clean projection claim.
// The -pass audit re-proves a claim up to one audit period after [completeness.MaxProjectionCarryAge];
// two more periods absorb two missed re-proof nights. The SDEX census and sep41_transfers are
// re-proved weekly by their own timers, not by the pass.
const coverageVerdictEvidenceStaleAge = completeness.MaxProjectionCarryAge + 3*coverageVerdictStaleAge

// coverageIngestStallAge is the ledgerstream cursor age past which ingest counts as stalled. The
// cursor moves every ~5 s ledger, so it cannot fire healthy.
const coverageIngestStallAge = 10 * time.Minute

// handleCoverageVerdicts serves GET /v1/coverage, every source's latest ADR-0033 completeness
// verdict. `flags.stale` carries the live-tip gate ([Server.coverageVerdictsStale]): every row field
// is frozen at compute time, so without it a dead audit would keep serving "complete" forever.
func (s *Server) handleCoverageVerdicts(w http.ResponseWriter, r *http.Request) {
	if s.CompletenessReader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/coverage-unavailable",
			"Coverage verdicts not available", http.StatusServiceUnavailable,
			"this deployment has no CompletenessReader wired — check binary configuration")
		return
	}
	snaps, verdictsStale, err := s.completenessVerdicts(r.Context())
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

	network := s.Network
	if network == "" {
		network = sourcenet.Pubnet
	}
	view := CoverageVerdictsView{
		Sources:              make([]CoverageVerdictView, 0, len(snaps)),
		Network:              network,
		NotApplicableSources: make([]NotApplicableSourceView, 0),
	}
	view.UsdVolumePricing = s.UsdVolumePricing.view()
	for _, na := range sourcenet.NotApplicableOn(network) {
		view.NotApplicableSources = append(view.NotApplicableSources, NotApplicableSourceView{Source: na.Source, Reason: na.Reason})
	}
	for _, sn := range snaps {
		// A stale pubnet-only row must not count against a test net.
		if ok, _ := sourcenet.Applicable(sn.Source, network); !ok {
			continue
		}
		// An audit axis has no substrate, projection or retention window, so neither `complete`
		// axis is defined for it and it belongs in neither total; it is published separately.
		if completeness.IsAuditAxis(sn.Source) {
			view.Recognition = recognitionAxisView(sn)
			continue
		}
		view.Sources = append(view.Sources, CoverageVerdictView{
			Source:                   sn.Source,
			Complete:                 sn.Complete,
			LakeComplete:             sn.LakeComplete,
			SubstrateOK:              sn.SubstrateOK,
			RecognitionOK:            sn.RecognitionOK,
			ProjectionOK:             sn.ProjectionOK,
			GenesisLedger:            sn.Genesis,
			WatermarkLedger:          sn.Watermark,
			TipLedger:                sn.Tip,
			ProjectionVerifiedFrom:   sn.ProjectionVerifiedFrom,
			ProjectionReconciledFrom: sn.ProjectionReconciledFrom,
			ProjectionEvidencedAt:    wireTimeOrNil(sn.ProjectionEvidencedAt),
			CoveragePct:              sn.CoveragePct,
			FirstProblemLedger:       sn.FirstProblem,
			Detail:                   sn.Detail,
			ComputedAt:               WireTime(sn.ComputedAt),
		})
		if sn.Complete {
			view.CompleteSources++
		}
		if sn.LakeComplete {
			view.LakeCompleteSources++
		}
	}
	view.UnverifiedSources = unverifiedSources(s.AuditedSources, snaps, network)
	view.TotalSources = len(view.Sources) + len(view.UnverifiedSources)
	view.LaggingSources = laggingSources(view.Sources)

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, view, Flags{Stale: verdictsStale})
}

// completenessVerdicts is the one read of the verdict rows for every surface that republishes a
// claim, paired with the freshness verdict that qualifies them. Callers check the reader is non-nil.
func (s *Server) completenessVerdicts(ctx context.Context) ([]timescale.CompletenessSnapshot, bool, error) {
	snaps, err := s.CompletenessReader.ListCompletenessSnapshots(ctx)
	if err != nil {
		return nil, false, err
	}
	network := s.Network
	if network == "" {
		network = sourcenet.Pubnet
	}
	// The staleness gate must judge only what this deployment publishes: a pubnet-only row left on
	// a testnet deployment is never refreshed, and would flag flags.stale=true forever.
	applicable := make([]timescale.CompletenessSnapshot, 0, len(snaps))
	for _, sn := range snaps {
		if ok, _ := sourcenet.Applicable(sn.Source, network); !ok {
			continue
		}
		applicable = append(applicable, sn)
	}
	return snaps, s.coverageVerdictsStale(ctx, applicable), nil
}

// recognitionAxisView projects the recognition snapshot onto its own wire shape. The per-source
// booleans and coverage_pct are NOT re-exported: they were never claims about this axis.
func recognitionAxisView(sn timescale.CompletenessSnapshot) *RecognitionAxisView {
	v := &RecognitionAxisView{
		AllShapesRecognized: sn.RecognitionOK,
		EarliestLedger:      sn.FirstProblem,
		ScannedFromLedger:   sn.Genesis,
		TipLedger:           sn.Tip,
		Meaning:             recognitionAxisMeaning,
		Detail:              sn.Detail,
		ComputedAt:          WireTime(sn.ComputedAt),
	}
	// Counts only when the audit's format parses; otherwise omitted, never zeroed.
	if c, ok := completeness.ParseRecognitionDetail(sn.Detail); ok {
		shapes, contracts := c.Shapes, c.Contracts
		v.UnrecognizedShapes, v.UnrecognizedContracts = &shapes, &contracts
	}
	return v
}

// coverageVerdictsStale is the live-tip gate: is this `complete: true` a statement about the chain
// as it is NOW? The rows are stamped at compute time, so a dead audit still reads `coverage_pct: 1`.
//
// Four independent signals, OR'd (fail-closed; any one is `flags.stale`, ADR-0018):
//
//   - LEDGER GAP: the network tip has run more than [coverageVerdictStaleLedgers] past the
//     verdict's own tip. The tip is the cursor extrapolated by wall-clock time
//     ([completeness.NetworkTipLowerBound]), since the bare cursor would always agree with it.
//   - VERDICT AGE: computed_at older than [coverageVerdictStaleAge], or absent.
//   - EVIDENCE AGE: a source claiming projection_ok whose projection_evidenced_at is older than
//     [coverageVerdictEvidenceStaleAge], or unknown (computed_at is restamped by runs that only
//     carry the claim forward). Audit axes carry no projection claim and are exempt.
//   - INGEST STALL: the ledgerstream cursor has not been written for [coverageIngestStallAge]; a
//     frozen cursor plus a running audit keeps the gap at 0 and computed_at fresh.
//
// An EMPTY verdict list is not flagged; without a cursor read the age signals decide.
func (s *Server) coverageVerdictsStale(ctx context.Context, snaps []timescale.CompletenessSnapshot) bool {
	if len(snaps) == 0 {
		return false
	}
	live, haveTip := s.liveTip(ctx)
	now := time.Now()
	if haveTip && now.Sub(time.Time(live.IngestedAt)) > coverageIngestStallAge {
		return true
	}
	netTip := completeness.NetworkTipLowerBound(live.LatestLedger, time.Time(live.IngestedAt), now)
	for _, sn := range snaps {
		if haveTip && netTip > sn.Tip && netTip-sn.Tip > coverageVerdictStaleLedgers {
			return true
		}
		if sn.ComputedAt.IsZero() || now.Sub(sn.ComputedAt) > coverageVerdictStaleAge {
			return true
		}
		if sn.ProjectionOK && !completeness.IsAuditAxis(sn.Source) &&
			completeness.ProjectionEvidenceExpired(sn.ProjectionEvidencedAt, now, coverageVerdictEvidenceStaleAge) {
			return true
		}
	}
	return false
}

// liveTip returns the ledgerstream cursor's last ledger and write time. ok=false when no reader is
// wired, the row is missing or the read failed; callers degrade rather than fail. The read is
// bounded at 5s so a slow Postgres cannot hold a public GET open.
func (s *Server) liveTip(ctx context.Context) (LedgerTipView, bool) {
	if s.Cursors == nil {
		return LedgerTipView{}, false
	}
	tipCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	view, ok, err := s.ledgerTip(tipCtx)
	if err != nil {
		s.logger.Debug("live tip read failed — freshness gate falls back to verdict age", "err", err)
		return LedgerTipView{}, false
	}
	return view, ok
}
