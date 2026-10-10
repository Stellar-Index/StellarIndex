package v1_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func envelopeStale(t *testing.T, url string) bool {
	t.Helper()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", url, resp.StatusCode)
	}
	var env struct {
		Data  json.RawMessage `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	// Non-vacuity: the completeness summary must actually be served, so
	// flags.stale is qualifying a republished verdict.
	if !strings.Contains(string(env.Data), `"completeness":{"complete":true`) {
		t.Fatalf("GET %s served no completeness summary; flags.stale would be meaningless", url)
	}
	return env.Flags.Stale
}

// TestProtocols_StaleWhenRepublishedVerdictIsOld pins that /v1/protocols
// and /v1/protocols/{name} republish the completeness_snapshots rows
// /v1/coverage gates, so a verdict 30h old must raise flags.stale on all
// three — not only on /v1/coverage.
func TestProtocols_StaleWhenRepublishedVerdictIsOld(t *testing.T) {
	snaps := []timescale.CompletenessSnapshot{{
		Source: "blend", Genesis: 51_499_546, Tip: 63_000_000, Watermark: 63_000_000,
		CoveragePct: 1, Complete: true, LakeComplete: true,
		SubstrateOK: true, RecognitionOK: true, ProjectionOK: true,
		ComputedAt:            time.Now().UTC().Add(-30 * time.Hour),
		ProjectionEvidencedAt: time.Now().UTC().Add(-30 * time.Hour),
	}}
	old := httpTestServer(t, v1.New(v1.Options{CompletenessReader: &stubCompletenessReader{snaps: snaps}}))
	for _, path := range []string{"/v1/protocols", "/v1/protocols/blend"} {
		if !envelopeStale(t, old.URL+path) {
			t.Errorf("GET %s: flags.stale = false for a 30h-old verdict, want true "+
				"(/v1/coverage flags the same row stale)", path)
		}
	}
	if !coverageStaleFlag(t, old.URL) {
		t.Fatal("/v1/coverage flags.stale = false for the same row — the gate itself regressed")
	}

	snaps[0].ComputedAt = time.Now().UTC().Add(-5 * time.Minute)
	fresh := httpTestServer(t, v1.New(v1.Options{CompletenessReader: &stubCompletenessReader{snaps: snaps}}))
	for _, path := range []string{"/v1/protocols", "/v1/protocols/blend"} {
		if envelopeStale(t, fresh.URL+path) {
			t.Errorf("GET %s: flags.stale = true for a 5-minute-old verdict", path)
		}
	}
}
