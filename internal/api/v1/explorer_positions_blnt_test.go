package v1_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubBLNTPositionsReader struct {
	stubPositionsReader
	blnt    []timescale.BlendPositionFold
	blntErr error
}

func (s *stubBLNTPositionsReader) BLNTPositionsByUser(context.Context, string) ([]timescale.BlendPositionFold, error) {
	return s.blnt, s.blntErr
}

// TestExplorer_AccountPositions_BLNTLegs — BLNT folds serve as protocol "blnt" with Blend's leg semantics: an
// i128 net above 2^63 verbatim, an auction-moved leg superseded, and a seventh fold in the coverage denominator.
func TestExplorer_AccountPositions_BLNTLegs(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	const huge = "18446744073709551616"
	reader := &stubBLNTPositionsReader{blnt: []timescale.BlendPositionFold{{
		Pool: "CPOOL1", Asset: "CUSDC1",
		HasSupplyLeg: true, SupplyNet: huge, SupplyLastActivity: now, SupplyLastLedger: 10,
		HasBorrowLeg: true, BorrowNet: "40", BorrowSuperseded: true, BorrowTokens: "0", BorrowLastLedger: 11,
	}}}
	base := httpTestServer(t, v1.New(v1.Options{Positions: reader})).URL
	resp := mustGet(t, base+"/v1/accounts/"+testG+"/positions?include_closed=true")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data struct {
			Positions []map[string]any `json:"positions"`
		} `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data.Positions) != 2 {
		t.Fatalf("positions = %+v", body.Data.Positions)
	}
	sup, bor := body.Data.Positions[0], body.Data.Positions[1]
	if sup["protocol"] != "blnt" || sup["position_kind"] != "lending_supply" || sup["amount"] != huge ||
		sup["amount_semantics"] != "net_underlying_at_event_time" {
		t.Errorf("supply leg = %+v", sup)
	}
	if bor["protocol"] != "blnt" || bor["amount"] != "0" || bor["amount_semantics"] != "superseded_by_auction" {
		t.Errorf("borrow leg = %+v", bor)
	}

	boom := errors.New("down")
	reader = &stubBLNTPositionsReader{blntErr: boom, stubPositionsReader: stubPositionsReader{
		blendErr: boom, backstopErr: boom, phoenixErr: boom, defindexErr: boom, creditErr: boom, aquariusErr: boom,
	}}
	base = httpTestServer(t, v1.New(v1.Options{Positions: reader})).URL
	resp = mustGet(t, base+"/v1/accounts/"+testG+"/positions")
	var failed struct {
		Data map[string]any `json:"data"`
	}
	mustDecode(t, resp, &failed)
	if note, _ := failed.Data["coverage_note"].(string); !strings.Contains(note, "7 of 7 protocol reads failed") || !strings.Contains(note, "blnt") {
		t.Errorf("coverage_note = %q, want all 7 folds named failed", note)
	}
}

type stubBLNTBackstopReader struct {
	stubPositionsReader
	folds []timescale.BLNTBackstopFold
}

func (s *stubBLNTBackstopReader) BLNTBackstopByUser(context.Context, string) ([]timescale.BLNTBackstopFold, error) {
	return s.folds, nil
}

// TestExplorer_AccountPositions_BLNTBackstopUnknowns — an inconsistent share history serves amount "" under shares_unknown, never
// "shares"; a position whose queue composition is unknown serves the queued total and the marker, no per-entry expirations.
func TestExplorer_AccountPositions_BLNTBackstopUnknowns(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	reader := &stubBLNTBackstopReader{folds: []timescale.BLNTBackstopFold{
		{Pool: "CPOOL1", Tier: "FirstLoss", LastActivity: now, LastLedger: 10},
		{
			Pool: "CPOOL1", Tier: "SecondLoss", Shares: "18446744073709551616", ExpirationsUnknown: true, QueuedShares: "9223372036854775808",
			LastActivity: now, LastLedger: 11,
		},
	}}
	base := httpTestServer(t, v1.New(v1.Options{Positions: reader})).URL
	resp := mustGet(t, base+"/v1/accounts/"+testG+"/positions?include_closed=true")
	var body struct {
		Data struct {
			Positions []map[string]any `json:"positions"`
		} `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data.Positions) != 2 {
		t.Fatalf("positions = %+v", body.Data.Positions)
	}
	unknown, ambiguous := body.Data.Positions[0], body.Data.Positions[1]
	if unknown["amount"] != "" || unknown["amount_semantics"] != "shares_unknown" {
		t.Errorf("inconsistent position = %+v", unknown)
	}
	if ambiguous["amount_semantics"] != "shares" || ambiguous["queued_expirations_unknown"] != true ||
		ambiguous["queued_shares"] != "9223372036854775808" || ambiguous["queued_withdrawals"] != nil {
		t.Errorf("ambiguous queue position = %+v", ambiguous)
	}
}
