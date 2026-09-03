package v1_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// r1's actual system recognition snapshot, 2026-09-02 — the row that
// made the public headline read "20 of 21 complete" with the 1 being an
// audit axis rather than a source.
func liveRecognitionSnapshot(now time.Time) timescale.CompletenessSnapshot {
	return timescale.CompletenessSnapshot{
		Source: completeness.SystemRecognitionSource,
		// The per-source fields the computor is forced to write: two
		// hardcoded trues, complete/lake_complete false BY CONSTRUCTION,
		// and a coverage_pct that only decreases.
		Genesis: 50_457_424, Tip: 64_234_754, Watermark: 50_560_485,
		CoveragePct: 0.0074805490265131905, Complete: false, LakeComplete: false,
		FirstProblem: 50_560_486,
		SubstrateOK:  true, RecognitionOK: false, ProjectionOK: true,
		Detail: completeness.FormatRecognitionDetail(completeness.RecognitionCensus{
			Shapes: 23945, Contracts: 4172, EarliestLedger: 50_560_486,
		}),
		ComputedAt: now,
	}
}

func getCoverage(t *testing.T, snaps []timescale.CompletenessSnapshot) v1.CoverageVerdictsView {
	t.Helper()
	srv := v1.New(v1.Options{CompletenessReader: &stubCompletenessReader{snaps: snaps}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/coverage")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.CoverageVerdictsView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}

// The category error: the system recognition census is not a source, so
// it must not sit in sources[] and must not be counted against the
// public "N of M complete" headline. Two genuinely-complete sources
// alongside it read 2 of 2, not 2 of 3.
func TestHandleCoverageVerdicts_recognitionIsNotASource(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	d := getCoverage(t, []timescale.CompletenessSnapshot{
		{
			Source: "blend", Genesis: 51_499_546, Tip: 64_234_754, Watermark: 64_234_754,
			CoveragePct: 1, Complete: true, LakeComplete: true,
			SubstrateOK: true, RecognitionOK: true, ProjectionOK: true, ComputedAt: now,
		},
		liveRecognitionSnapshot(now),
		{
			Source: "soroswap", Genesis: 61_500_000, Tip: 64_234_754, Watermark: 64_234_754,
			CoveragePct: 1, Complete: true, LakeComplete: true,
			SubstrateOK: true, RecognitionOK: true, ProjectionOK: true, ComputedAt: now,
		},
	})

	if d.TotalSources != 2 || d.CompleteSources != 2 || d.LakeCompleteSources != 2 {
		t.Fatalf("headline = %d/%d complete, %d lake — want 2/2 and 2; the recognition audit axis must not be counted as a source",
			d.CompleteSources, d.TotalSources, d.LakeCompleteSources)
	}
	if len(d.Sources) != d.TotalSources {
		t.Fatalf("len(sources) = %d but total_sources = %d — the two must agree", len(d.Sources), d.TotalSources)
	}
	for _, s := range d.Sources {
		if s.Source == completeness.SystemRecognitionSource {
			t.Fatalf("recognition is still in sources[]: %+v", s)
		}
	}
}

// Removing it from sources[] must make it MORE visible, not less: the
// axis is published at the top level with its real numbers, its
// meaning, and its detail. This is the honesty half of the change — if
// this test is deleted the headline change becomes laundering.
func TestHandleCoverageVerdicts_recognitionAxisPublishesItsNumbers(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	d := getCoverage(t, []timescale.CompletenessSnapshot{liveRecognitionSnapshot(now)})

	rec := d.Recognition
	if rec == nil {
		t.Fatal("data.recognition is nil — the axis was dropped rather than relocated")
	}
	if rec.AllShapesRecognized {
		t.Error("all_shapes_recognized = true, want false: 23945 shapes are unattributed")
	}
	if rec.UnrecognizedShapes == nil || *rec.UnrecognizedShapes != 23945 {
		t.Errorf("unrecognized_shapes = %v, want 23945", rec.UnrecognizedShapes)
	}
	if rec.UnrecognizedContracts == nil || *rec.UnrecognizedContracts != 4172 {
		t.Errorf("unrecognized_contracts = %v, want 4172", rec.UnrecognizedContracts)
	}
	if rec.EarliestLedger != 50_560_486 {
		t.Errorf("earliest_ledger = %d, want 50560486", rec.EarliestLedger)
	}
	if rec.ScannedFromLedger != 50_457_424 {
		t.Errorf("scanned_from_ledger = %d, want 50457424", rec.ScannedFromLedger)
	}
	if rec.TipLedger != 64_234_754 {
		t.Errorf("tip_ledger = %d, want 64234754", rec.TipLedger)
	}
	// The plain-language statement must say what it is AND what it is
	// not, on the wire — a consumer charting this JSON never reads the
	// OpenAPI spec.
	for _, want := range []string{"DISCOVERY BACKLOG", "not missing data", "recognition_ok=false"} {
		if !strings.Contains(rec.Meaning, want) {
			t.Errorf("meaning does not contain %q: %q", want, rec.Meaning)
		}
	}
	if !strings.Contains(rec.Detail, "23945 unrecognized shape(s)") {
		t.Errorf("detail lost the audit's own wording: %q", rec.Detail)
	}
	if !rec.ComputedAt.Equal(now) {
		t.Errorf("computed_at = %v, want %v", rec.ComputedAt, now)
	}
}

// The anti-laundering guard. Excluding the audit axis must not blunt
// the headline: a REAL source that fails still drags complete_sources
// below total_sources, with its claim breakdown intact.
func TestHandleCoverageVerdicts_genuineSourceFailureStillFailsHeadline(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	d := getCoverage(t, []timescale.CompletenessSnapshot{
		{
			Source: "blend", Genesis: 51_499_546, Tip: 64_234_754, Watermark: 64_234_754,
			CoveragePct: 1, Complete: true, LakeComplete: true,
			SubstrateOK: true, RecognitionOK: true, ProjectionOK: true, ComputedAt: now,
		},
		liveRecognitionSnapshot(now),
		{
			// A source silently dropping its OWN events: the bucket that
			// IS a defect. recognition_ok=false on the source's own row.
			Source: "phoenix", Genesis: 51_572_016, Tip: 64_234_754, Watermark: 60_000_000,
			CoveragePct: 0.8, Complete: false, LakeComplete: false, FirstProblem: 60_000_001,
			SubstrateOK: true, RecognitionOK: false, ProjectionOK: true,
			Detail: "recognition: 2 unrecognized shape(s) on owned contracts", ComputedAt: now,
		},
	})

	if d.CompleteSources != 1 || d.TotalSources != 2 {
		t.Fatalf("headline = %d/%d, want 1/2 — a genuine source failure must still fail the headline",
			d.CompleteSources, d.TotalSources)
	}
	if d.LakeCompleteSources != 1 {
		t.Fatalf("lake_complete_sources = %d, want 1", d.LakeCompleteSources)
	}
	var px *v1.CoverageVerdictView
	for i := range d.Sources {
		if d.Sources[i].Source == "phoenix" {
			px = &d.Sources[i]
		}
	}
	if px == nil {
		t.Fatal("phoenix row missing from sources[]")
	}
	if px.RecognitionOK || px.Complete {
		t.Errorf("phoenix must stay red: recognition_ok=%v complete=%v", px.RecognitionOK, px.Complete)
	}
}

// A deployment whose audit has never written the census gets an
// explicit null, not a silently absent key: absence of the axis must be
// visible, and must never read as "clean".
func TestHandleCoverageVerdicts_recognitionAbsentIsNull(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	srv := v1.New(v1.Options{CompletenessReader: &stubCompletenessReader{snaps: []timescale.CompletenessSnapshot{{
		Source: "blend", Genesis: 51_499_546, Tip: 64_234_754, Watermark: 64_234_754,
		CoveragePct: 1, Complete: true, LakeComplete: true,
		SubstrateOK: true, RecognitionOK: true, ProjectionOK: true, ComputedAt: now,
	}}}})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/coverage")
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, ok := env.Data["recognition"]
	if !ok {
		t.Fatal(`the "recognition" key must always be present so its absence is visible`)
	}
	if string(raw) != "null" {
		t.Errorf("recognition = %s, want null when the audit has produced no census", raw)
	}
}

// Fail-closed on an unparseable detail: publish no counts rather than
// zeroes. "0 unrecognized shapes" is a claim of cleanliness, and
// inventing it from a parse failure is the laundering direction.
func TestHandleCoverageVerdicts_recognitionCountsOmittedWhenUnparseable(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	sn := liveRecognitionSnapshot(now)
	sn.Detail = "legacy row written before the census format existed"
	d := getCoverage(t, []timescale.CompletenessSnapshot{sn})

	rec := d.Recognition
	if rec == nil {
		t.Fatal("data.recognition is nil")
	}
	if rec.UnrecognizedShapes != nil || rec.UnrecognizedContracts != nil {
		t.Errorf("counts must be omitted, not zeroed, when the detail does not parse: shapes=%v contracts=%v",
			rec.UnrecognizedShapes, rec.UnrecognizedContracts)
	}
	if rec.Detail != sn.Detail {
		t.Errorf("detail must still be served verbatim, got %q", rec.Detail)
	}
	if rec.AllShapesRecognized {
		t.Error("all_shapes_recognized must still reflect the stored verdict (false)")
	}
}
