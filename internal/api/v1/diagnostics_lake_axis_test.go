package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDiagnosticsIngestion_ServesBothCompletenessAxes pins C6-046
// (audit-2026-07-23). ADR-0033/ADR-0034 verdicts are TWO-AXIS:
// `lake_complete` (archive proven genesis-to-tip) and `complete` (that
// plus the retention-scoped projection reconcile). A source is routinely
// lake_complete=true with complete=false.
//
// /v1/diagnostics/ingestion — the snapshot the public status page reads —
// projected only the SERVED axis, so the page could not distinguish "the
// data does not exist" from "the data exists and the projection is
// catching up". The two-axis verdict reached /v1/coverage and
// /diagnostics alone.
func TestDiagnosticsIngestion_ServesBothCompletenessAxes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	srv := v1.New(v1.Options{
		CompletenessReader: &stubCompletenessReader{snaps: []timescale.CompletenessSnapshot{
			{
				Source: "sdex", Genesis: 2, Tip: 63_000_000, Watermark: 62_000_000,
				CoveragePct: 0.98,
				// The exact asymmetric state the status page could not render.
				Complete: false, LakeComplete: true,
				SubstrateOK: true, RecognitionOK: true, ProjectionOK: false,
				ComputedAt: now,
			},
		}},
	})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/diagnostics/ingestion", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var body struct {
		Data struct {
			BackfillCoverage []struct {
				Source                   string `json:"source"`
				CompletenessComplete     bool   `json:"completeness_complete"`
				CompletenessLakeComplete bool   `json:"completeness_lake_complete"`
			} `json:"backfill_coverage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}

	var found bool
	for _, r := range body.Data.BackfillCoverage {
		if r.Source != "sdex" {
			continue
		}
		found = true
		if r.CompletenessComplete {
			t.Errorf("completeness_complete = true, want false (ProjectionOK is false)")
		}
		if !r.CompletenessLakeComplete {
			t.Error("completeness_lake_complete is absent/false while the snapshot says the archive " +
				"IS genesis-complete — the status page cannot tell a projection lag from missing history")
		}
	}
	if !found {
		t.Fatalf("no backfill_coverage row for sdex in %s", rec.Body.String())
	}
	_ = context.Background
}
