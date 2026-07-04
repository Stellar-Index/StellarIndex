package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
)

const testG = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// wmStub is a canned v1.LakeWatermarkReader (ADR-0041 D4).
type wmStub struct {
	ledger   uint32
	closedAt time.Time
}

func (s *wmStub) LakeWatermark(context.Context) (uint32, time.Time, error) {
	return s.ledger, s.closedAt, nil
}

// TestExplorer_AccountStateAndHolders_Watermark pins ADR-0041 Decision 4 on
// the lake-backed explorer reads: `as_of_ledger` carries the (cached) lake
// watermark and `flags.stale` fires when its close time trails now beyond
// the threshold (10min here, comfortably past the 300s threshold).
func TestExplorer_AccountStateAndHolders_Watermark(t *testing.T) {
	reader := &stubExplorerReader{
		accountState: clickhouse.AccountState{Exists: true, Balance: 1},
		holders:      []clickhouse.AssetHolder{{AccountID: testG, Balance: 5}},
		holderCount:  1,
	}
	srv := v1.New(v1.Options{Explorer: reader, LakeWatermark: &wmStub{ledger: 63999999, closedAt: time.Now().Add(-10 * time.Minute)}})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts/"+testG)
	var acct struct {
		Data  v1.AccountStateView `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &acct)
	if acct.Data.AsOfLedger != 63999999 {
		t.Errorf("account as_of_ledger = %d, want 63999999", acct.Data.AsOfLedger)
	}
	if !acct.Flags.Stale {
		t.Error("account flags.stale should fire for a 10-minute-old watermark")
	}

	resp = mustGet(t, base+"/v1/assets/native/holders")
	var hold struct {
		Data  v1.AssetHoldersView `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &hold)
	if hold.Data.AsOfLedger != 63999999 {
		t.Errorf("holders as_of_ledger = %d, want 63999999", hold.Data.AsOfLedger)
	}
	if !hold.Flags.Stale {
		t.Error("holders flags.stale should fire for a 10-minute-old watermark")
	}
}

func TestExplorer_AccountState(t *testing.T) {
	reader := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:        true,
		Balance:       12345678901, // > 2^33, exercises the string encoding
		SeqNum:        9000000000000000000,
		NumSubEntries: 3,
		Flags:         1,
		HomeDomain:    "example.com",
		MasterWeight:  1, ThreshLow: 0, ThreshMed: 2, ThreshHigh: 3,
		Signers:    []clickhouse.AccountSigner{{Key: testG, Weight: 1}},
		Trustlines: []clickhouse.TrustlineState{{Asset: "USDC-" + testG, Balance: 500, Limit: 1000, Flags: 1}},
		Offers:     []clickhouse.OfferState{{OfferID: 7, Selling: "native", Buying: "USDC-" + testG, Amount: 250, PriceN: 1, PriceD: 2}},
	}}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/accounts/"+testG)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AccountStateView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if !body.Data.Exists {
		t.Fatal("exists should be true")
	}
	// Balances must be STRINGS (ADR-0003).
	if body.Data.Balance != "12345678901" || body.Data.SeqNum != "9000000000000000000" {
		t.Errorf("balance/seq = %q/%q, want exact strings", body.Data.Balance, body.Data.SeqNum)
	}
	if body.Data.Thresholds == nil || body.Data.Thresholds.Med != 2 {
		t.Errorf("thresholds = %+v, want med=2", body.Data.Thresholds)
	}
	if len(body.Data.Trustlines) != 1 || body.Data.Trustlines[0].Balance != "500" {
		t.Errorf("trustlines = %+v", body.Data.Trustlines)
	}
	if len(body.Data.Offers) != 1 || body.Data.Offers[0].Selling != "native" {
		t.Errorf("offers = %+v", body.Data.Offers)
	}
}

func TestExplorer_AccountState_NotFoundIs200Empty(t *testing.T) {
	reader := &stubExplorerReader{accountState: clickhouse.AccountState{Exists: false}}
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+"/v1/accounts/"+testG)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data v1.AccountStateView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if body.Data.Exists {
		t.Error("exists should be false for an unknown account")
	}
}

func TestExplorer_AccountState_RejectsBadStrkey(t *testing.T) {
	reader := &stubExplorerReader{}
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+"/v1/accounts/not-an-account")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestExplorer_AssetHolders(t *testing.T) {
	reader := &stubExplorerReader{
		holders: []clickhouse.AssetHolder{
			{AccountID: testG, Balance: 999999999999},
			{AccountID: testG, Balance: 1},
		},
		holderCount: 4321,
	}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/assets/USDC-"+testG+"/holders?limit=10")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AssetHoldersView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if body.Data.HolderCount != 4321 {
		t.Errorf("holder_count = %d, want 4321", body.Data.HolderCount)
	}
	if len(body.Data.Holders) != 2 || body.Data.Holders[0].Balance != "999999999999" {
		t.Errorf("holders = %+v (balance must be a string)", body.Data.Holders)
	}
}
