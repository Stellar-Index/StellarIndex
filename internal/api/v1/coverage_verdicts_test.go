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
				CoveragePct: 99.99, Complete: true, LakeComplete: true,
				SubstrateOK: true, RecognitionOK: true, ProjectionOK: true, ComputedAt: now,
			},
			{
				Source: "phoenix", Genesis: 51_572_016, Tip: 63_000_000, Watermark: 60_000_000,
				CoveragePct: 80, Complete: false, LakeComplete: false, FirstProblem: 60_000_001,
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
	if d.LakeCompleteSources != 1 {
		t.Fatalf("lake_complete_sources = %d, want 1", d.LakeCompleteSources)
	}
	if d.Sources[0].Source != "blend" || !d.Sources[0].Complete || !d.Sources[0].LakeComplete {
		t.Errorf("blend row wrong: %+v", d.Sources[0])
	}
	px := d.Sources[1]
	if px.Complete || px.LakeComplete || px.ProjectionOK || !px.SubstrateOK || px.FirstProblemLedger != 60_000_001 || px.Detail == "" {
		t.Errorf("phoenix failing-claim breakdown wrong: %+v", px)
	}
	if px.WatermarkLedger != 60_000_000 || px.GenesisLedger != 51_572_016 {
		t.Errorf("phoenix ledger fields wrong: %+v", px)
	}
}

// TestHandleCoverageVerdicts_LakeCompleteDecouplesFromComplete pins
// the ADR-0033/0034 two-axis verdict wire mapping (decision brief
// notes/DECISION-genesis-complete-verdict-2026-07-16.md, Option B): a
// source whose certified ClickHouse archive is genesis-complete but
// whose served-tier projection reconcile fails (soroswap trades are
// retention-scoped per ADR-0034) must serve lake_complete=true,
// complete=false — and lake_complete_sources must tally independently
// of complete_sources.
func TestHandleCoverageVerdicts_LakeCompleteDecouplesFromComplete(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	srv := v1.New(v1.Options{
		CompletenessReader: &stubCompletenessReader{snaps: []timescale.CompletenessSnapshot{
			{
				Source: "soroswap", Genesis: 61_500_000, Tip: 63_305_532, Watermark: 63_305_532,
				CoveragePct: 1, Complete: false, LakeComplete: true,
				SubstrateOK: true, RecognitionOK: true, ProjectionOK: false,
				Detail:     "projection: mismatched ledger(s) outside the served retention window",
				ComputedAt: now,
			},
		}},
	})
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
	d := env.Data
	if d.CompleteSources != 0 {
		t.Errorf("complete_sources = %d, want 0 (projection failed)", d.CompleteSources)
	}
	if d.LakeCompleteSources != 1 {
		t.Errorf("lake_complete_sources = %d, want 1 (archive genesis-complete)", d.LakeCompleteSources)
	}
	sw := d.Sources[0]
	if sw.Complete {
		t.Error("soroswap Complete should be false: served-tier projection failed")
	}
	if !sw.LakeComplete {
		t.Error("soroswap LakeComplete should be true: substrate+recognition reached tip, decoupled from projection")
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
