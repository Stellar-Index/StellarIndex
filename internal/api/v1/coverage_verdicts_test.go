package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubCompletenessReader struct {
	snaps []timescale.CompletenessSnapshot
	err   error
}

func (s *stubCompletenessReader) ListCompletenessSnapshots(context.Context) ([]timescale.CompletenessSnapshot, error) {
	return s.snaps, s.err
}

// Happy path: verdicts are projected 1:1 with the summary counts; a
// failing source carries its claim breakdown + problem detail.
func TestHandleCoverageVerdicts_Happy(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	srv := v1.New(v1.Options{
		CompletenessReader: &stubCompletenessReader{snaps: []timescale.CompletenessSnapshot{
			{
				Source: "blend", Genesis: 51_499_546, Tip: 63_000_000, Watermark: 62_999_000,
				CoveragePct: 99.99, Complete: true,
				SubstrateOK: true, RecognitionOK: true, ProjectionOK: true, ComputedAt: now,
			},
			{
				Source: "phoenix", Genesis: 51_572_016, Tip: 63_000_000, Watermark: 60_000_000,
				CoveragePct: 80, Complete: false, FirstProblem: 60_000_001,
				SubstrateOK: true, RecognitionOK: true, ProjectionOK: false,
				Detail: "projection: 3 mismatched ledgers", ComputedAt: now,
			},
		}},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/coverage")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=60" {
		t.Errorf("Cache-Control = %q, want public, max-age=60", cc)
	}

	var env struct {
		Data v1.CoverageVerdictsView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := env.Data
	if d.TotalSources != 2 || d.CompleteSources != 1 {
		t.Fatalf("summary = %d/%d, want 1/2", d.CompleteSources, d.TotalSources)
	}
	if d.Sources[0].Source != "blend" || !d.Sources[0].Complete {
		t.Errorf("blend row wrong: %+v", d.Sources[0])
	}
	px := d.Sources[1]
	if px.Complete || px.ProjectionOK || !px.SubstrateOK || px.FirstProblemLedger != 60_000_001 || px.Detail == "" {
		t.Errorf("phoenix failing-claim breakdown wrong: %+v", px)
	}
	if px.WatermarkLedger != 60_000_000 || px.GenesisLedger != 51_572_016 {
		t.Errorf("phoenix ledger fields wrong: %+v", px)
	}
}

// No reader wired → 503 problem, mirroring every other optional-reader
// endpoint's contract.
func TestHandleCoverageVerdicts_NoReader(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/coverage")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
