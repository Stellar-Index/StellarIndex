package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
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

// coverageStaleFlag GETs /v1/coverage and returns the envelope's
// flags.stale — the live-tip gate's only observable.
func coverageStaleFlag(t *testing.T, url string) bool {
	t.Helper()
	resp := mustGet(t, url+"/v1/coverage")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data  v1.CoverageVerdictsView `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Guard against a vacuous pass: the verdicts must actually be
	// served, so `stale` is qualifying a real claim.
	if env.Data.TotalSources == 0 {
		t.Fatalf("no verdicts served — flags.stale would be meaningless")
	}
	return env.Flags.Stale
}

// TestHandleCoverageVerdicts_StaleWhenVerdictTrailsLiveTip pins the
// MNY-04 / A-H-4 live-tip gate.
//
// /v1/coverage is the product's trust surface ("every protocol,
// verified complete"). Every field on a row — tip_ledger, coverage_pct,
// the claim booleans — is frozen at the audit's compute time, so a
// verdict from a DEAD completeness audit keeps reading
// `complete: true, coverage_pct: 1` ("verified to tip") against a tip
// that the network left behind hours ago. Nothing in the response said
// so: flags.stale was hardcoded false.
//
// Here the verdict was computed against tip 63_000_000 only minutes ago
// (so the age signal is clean and cannot be what fires) while the live
// ledgerstream cursor — the SAME cursor compute-completeness resolves
// its tip from — sits 40_000 ledgers ahead (~2.3 days of chain, past
// the 34_560 two-audit-period bound at the daily cadence). The
// response must carry flags.stale = true.
//
// Proven red against the pre-fix handler: writeJSON(..., Flags{}) →
// stale = false.
func TestHandleCoverageVerdicts_StaleWhenVerdictTrailsLiveTip(t *testing.T) {
	srv := v1.New(v1.Options{
		CompletenessReader: &stubCompletenessReader{snaps: []timescale.CompletenessSnapshot{{
			Source: "blend", Genesis: 51_499_546, Tip: 63_000_000, Watermark: 63_000_000,
			CoveragePct: 1, Complete: true, LakeComplete: true,
			SubstrateOK: true, RecognitionOK: true, ProjectionOK: true,
			ComputedAt: time.Now().UTC().Add(-5 * time.Minute),
		}}},
		Cursors: &stubCursorsReader{rows: []timescale.Cursor{
			mkCursor("ledgerstream", "", 63_040_000, 4*time.Second),
		}},
	})
	ts := httpTestServer(t, srv)

	if !coverageStaleFlag(t, ts.URL) {
		t.Error("flags.stale = false, want true: the verdict's tip (63000000) trails the " +
			"live ledgerstream cursor (63040000) by 40000 ledgers (past the 34560 two-audit-period bound) — `complete: true` is no " +
			"longer a claim about the current chain (MNY-04)")
	}
}

// A verdict computed against a tip the live cursor has barely moved
// past is CURRENT — the flag must not fire. Guards the fix against
// over-flagging every response (which would make flags.stale useless
// on this surface and is the trivial way to pass the test above).
func TestHandleCoverageVerdicts_FreshVerdictNotStale(t *testing.T) {
	srv := v1.New(v1.Options{
		CompletenessReader: &stubCompletenessReader{snaps: []timescale.CompletenessSnapshot{{
			Source: "blend", Genesis: 51_499_546, Tip: 63_000_000, Watermark: 63_000_000,
			CoveragePct: 1, Complete: true, LakeComplete: true,
			SubstrateOK: true, RecognitionOK: true, ProjectionOK: true,
			ComputedAt: time.Now().UTC().Add(-5 * time.Minute),
		}}},
		Cursors: &stubCursorsReader{rows: []timescale.Cursor{
			// 500 ledgers ≈ 42 min of chain, well inside the ~3h horizon.
			mkCursor("ledgerstream", "", 63_000_500, 4*time.Second),
		}},
	})
	ts := httpTestServer(t, srv)

	if coverageStaleFlag(t, ts.URL) {
		t.Error("flags.stale = true, want false: the verdict is 500 ledgers behind the live " +
			"tip, inside the audit's own cadence — flagging it would make the signal noise")
	}
}

// With no CursorsReader wired the ledger-gap signal is unavailable, so
// the verdict's own age has to carry the gate: a verdict computed 30h
// ago is not a current claim. Proven red against the pre-fix handler
// (stale = false) and non-vacuous — the sibling test above shows a
// 5-minute-old verdict on the same wiring reads false.
func TestHandleCoverageVerdicts_StaleWhenVerdictIsOld(t *testing.T) {
	snaps := []timescale.CompletenessSnapshot{{
		Source: "blend", Genesis: 51_499_546, Tip: 63_000_000, Watermark: 63_000_000,
		CoveragePct: 1, Complete: true, LakeComplete: true,
		SubstrateOK: true, RecognitionOK: true, ProjectionOK: true,
		ComputedAt: time.Now().UTC().Add(-30 * time.Hour),
	}}
	srv := v1.New(v1.Options{CompletenessReader: &stubCompletenessReader{snaps: snaps}})
	ts := httpTestServer(t, srv)
	if !coverageStaleFlag(t, ts.URL) {
		t.Error("flags.stale = false, want true: the verdict is 30h old and no live tip is " +
			"wired to compare against — its age is the only freshness evidence there is")
	}

	// Same wiring, recent verdict → not stale (the age gate is the
	// thing being measured, not a constant true).
	snaps[0].ComputedAt = time.Now().UTC().Add(-5 * time.Minute)
	fresh := v1.New(v1.Options{CompletenessReader: &stubCompletenessReader{snaps: snaps}})
	if coverageStaleFlag(t, httpTestServer(t, fresh).URL) {
		t.Error("flags.stale = true for a 5-minute-old verdict with no live tip wired")
	}
}

// TestHandleCoverageVerdicts_NetworkScoping pins #483: on a test net the
// pubnet-anchored protocol sources are reported as NOT APPLICABLE and
// excluded from every total, instead of counting as incomplete because
// their pubnet genesis floor (soroswap 50,746,266) sits above the
// network's tip. Pubnet output is unchanged except for the two new
// fields.
func TestHandleCoverageVerdicts_NetworkScoping(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	snaps := []timescale.CompletenessSnapshot{
		{
			Source: "sdex", Genesis: 2, Tip: 4_467_014, Watermark: 4_467_014,
			CoveragePct: 100, Complete: true, LakeComplete: true,
			SubstrateOK: true, RecognitionOK: true, ProjectionOK: true, ComputedAt: now,
		},
		{
			// A stale row written before the catalogue was network-scoped.
			Source: "soroswap", Genesis: 50_746_266, Tip: 4_467_014, Watermark: 50_746_265,
			CoveragePct: 0, Complete: false, LakeComplete: false,
			SubstrateOK: true, RecognitionOK: true, ProjectionOK: false, ComputedAt: now,
		},
	}

	t.Run("testnet excludes pubnet-only sources", func(t *testing.T) {
		srv := v1.New(v1.Options{
			Network:            "testnet",
			CompletenessReader: &stubCompletenessReader{snaps: snaps},
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
		if d.Network != "testnet" {
			t.Errorf("network = %q, want testnet", d.Network)
		}
		if len(d.Sources) != 1 || d.Sources[0].Source != "sdex" {
			t.Fatalf("sources = %+v, want sdex only (the stale soroswap row must be dropped)", d.Sources)
		}
		if d.TotalSources != 1 || d.CompleteSources != 1 || d.LakeCompleteSources != 1 {
			t.Errorf("totals = %d/%d complete, %d lake, want 1/1 and 1",
				d.CompleteSources, d.TotalSources, d.LakeCompleteSources)
		}
		if len(d.NotApplicableSources) != len(sourcenet.PubnetOnlySources) {
			t.Fatalf("not_applicable_sources = %d, want %d", len(d.NotApplicableSources), len(sourcenet.PubnetOnlySources))
		}
		var sawSoroswap bool
		for _, na := range d.NotApplicableSources {
			if na.Source == "soroswap" {
				sawSoroswap = true
				if !strings.Contains(na.Reason, "testnet") {
					t.Errorf("reason must name the network: %q", na.Reason)
				}
			}
		}
		if !sawSoroswap {
			t.Error("soroswap must appear in not_applicable_sources")
		}
	})

	t.Run("pubnet is unchanged", func(t *testing.T) {
		srv := v1.New(v1.Options{
			Network:            "pubnet",
			CompletenessReader: &stubCompletenessReader{snaps: snaps},
		})
		ts := httpTestServer(t, srv)
		resp := mustGet(t, ts.URL+"/v1/coverage")
		var env struct {
			Data v1.CoverageVerdictsView `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		d := env.Data
		if d.Network != "pubnet" || len(d.NotApplicableSources) != 0 {
			t.Errorf("pubnet: network=%q not_applicable=%d, want pubnet and 0", d.Network, len(d.NotApplicableSources))
		}
		if d.TotalSources != 2 || d.CompleteSources != 1 {
			t.Errorf("pubnet totals = %d/%d, want 1/2 (both rows kept)", d.CompleteSources, d.TotalSources)
		}
	})

	t.Run("unset network defaults to pubnet", func(t *testing.T) {
		srv := v1.New(v1.Options{CompletenessReader: &stubCompletenessReader{snaps: snaps}})
		ts := httpTestServer(t, srv)
		resp := mustGet(t, ts.URL+"/v1/coverage")
		var env struct {
			Data v1.CoverageVerdictsView `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if env.Data.Network != "pubnet" || env.Data.TotalSources != 2 {
			t.Errorf("default: network=%q total=%d, want pubnet and 2", env.Data.Network, env.Data.TotalSources)
		}
	})
}
