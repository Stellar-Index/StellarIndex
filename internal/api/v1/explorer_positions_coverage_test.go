package v1_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestExplorer_AccountPositions_PartialReadFailureIsDisclosed is the
// C3-045 regression.
//
// Each of the six per-protocol folds logs and returns an empty slice on
// a read error, so before the fix a response where three of six reads
// failed was byte-for-byte identical on the wire to "this account holds
// no positions" — the caller could not tell a missing lending position
// from a repaid one. The static `note` is about valuation semantics and
// says nothing about coverage, so it could never carry this.
func TestExplorer_AccountPositions_PartialReadFailureIsDisclosed(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	boom := errors.New("clickhouse: connection refused")

	// Three of six folds fail; the three that succeed still return
	// real positions, so the response is genuinely partial rather than
	// wholly empty.
	reader := &stubPositionsReader{
		blendErr:    boom,
		phoenixErr:  boom,
		creditErr:   boom,
		backstop:    []timescale.BlendBackstopFold{{Pool: "CPOOL1", SharesNet: "250000", LastActivity: now, LastLedger: 101}},
		defindex:    []timescale.DefindexVaultFold{{ContractID: "CVAULT1", SharesNet: "400000", LastActivity: now, LastLedger: 102}},
		aquarius:    []timescale.AquariusGaugeFold{{ContractID: "CGAUGEPOOL1", NetDelta: "-500", LastActivity: now, LastLedger: 103}},
		blend:       nil,
		phoenix:     nil,
		credit:      nil,
		aquariusErr: nil,
	}

	srv := v1.New(v1.Options{Positions: reader})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts/"+testG+"/positions")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degrade, don't fail)", resp.StatusCode)
	}
	// Decoded as raw JSON rather than into AccountPositionsView so the
	// assertion is about the WIRE, and so this case still compiles —
	// and fails — against the pre-fix struct that had no such field.
	var body struct {
		Data map[string]any `json:"data"`
	}
	mustDecode(t, resp, &body)

	positions, _ := body.Data["positions"].([]any)
	if len(positions) != 3 {
		t.Fatalf("positions = %d, want the 3 folds that succeeded: %+v",
			len(positions), body.Data["positions"])
	}

	note, _ := body.Data["coverage_note"].(string)
	if note == "" {
		t.Fatal("coverage_note absent: 3 of 6 protocol reads failed and the response " +
			"is indistinguishable from an account with no blend/phoenix/sorocredit positions")
	}
	// Assert the corrected VALUE, not merely that something is set:
	// the note must name how many folds were lost, out of how many,
	// and which ones.
	if !strings.Contains(note, "3 of 6 protocol reads failed") {
		t.Errorf("coverage_note = %q, want it to state 3 of 6 protocol reads failed", note)
	}
	for _, want := range []string{"blend", "phoenix_stake", "sorocredit"} {
		if !strings.Contains(note, want) {
			t.Errorf("coverage_note = %q, want it to name the failed fold %q", note, want)
		}
	}
	// The protocols that DID answer must not be smeared as degraded.
	for _, notWant := range []string{"defindex", "aquarius_rewards", "blend_backstop"} {
		if strings.Contains(note, notWant) {
			t.Errorf("coverage_note = %q names %q, which read successfully", note, notWant)
		}
	}
	// The static valuation note is orthogonal and must survive.
	if staticNote, _ := body.Data["note"].(string); staticNote == "" {
		t.Error("the static valuation/accrual note was dropped")
	}
}

// TestExplorer_AccountPositions_AllReadsOKOmitsCoverageNote — the
// field must stay absent on a healthy response, so its presence is a
// real signal rather than decoration every consumer learns to ignore.
func TestExplorer_AccountPositions_AllReadsOKOmitsCoverageNote(t *testing.T) {
	srv := v1.New(v1.Options{Positions: &stubPositionsReader{}})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts/"+testG+"/positions")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	mustDecode(t, resp, &body)
	if note, _ := body.Data["coverage_note"].(string); note != "" {
		t.Errorf("coverage_note = %q on a fully-successful read, want absent", note)
	}
}

// TestExplorer_AccountPositions_TotalReadFailureIsNotAnEmptyAccount —
// the worst case: every fold fails. The response must not read as
// "this address holds nothing".
func TestExplorer_AccountPositions_TotalReadFailureIsNotAnEmptyAccount(t *testing.T) {
	boom := errors.New("postgres: server closed the connection unexpectedly")
	reader := &stubPositionsReader{
		blendErr: boom, backstopErr: boom, phoenixErr: boom,
		defindexErr: boom, creditErr: boom, aquariusErr: boom,
	}
	srv := v1.New(v1.Options{Positions: reader})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts/"+testG+"/positions")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	mustDecode(t, resp, &body)

	if positions, _ := body.Data["positions"].([]any); len(positions) != 0 {
		t.Fatalf("positions = %+v, want empty", positions)
	}
	note, _ := body.Data["coverage_note"].(string)
	if !strings.Contains(note, "6 of 6 protocol reads failed") {
		t.Fatalf("coverage_note = %q, want it to state all 6 folds failed", note)
	}
}
