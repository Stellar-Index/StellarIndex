package v1_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A live-shaped system recognition snapshot — the row that would
// make the public headline read "20 of 21 complete" with the 1 being an
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
