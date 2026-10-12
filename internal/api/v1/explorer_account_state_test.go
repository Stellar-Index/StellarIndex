package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
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

// TestExplorer_AccountStateAndHolders_Watermark pins ADR-0041 Decision 4 on lake-backed explorer reads: `as_of_ledger`
// carries the cached lake watermark and `flags.stale` fires when its close time trails now beyond the threshold.
func TestExplorer_AccountStateAndHolders_Watermark(t *testing.T) {
	// cachedLedger is the account state's OWN vintage (stamped at cache fill), deliberately
	// different from serveTimeLedger to prove the two are decoupled; otherwise as_of_ledger
	// would come from a fresh watermark read regardless of how old the cached state is.
	const cachedLedger, serveTimeLedger = 63_888_888, 63_999_999
	reader := &stubExplorerReader{
		accountState: clickhouse.AccountState{Exists: true, Balance: 1, AsOfLedger: cachedLedger},
		holders:      []clickhouse.AssetHolder{{AccountID: testG, Balance: 5}},
		holderCount:  1,
	}
	srv := v1.New(v1.Options{Explorer: reader, LakeWatermark: &wmStub{ledger: serveTimeLedger, closedAt: time.Now().Add(-10 * time.Minute)}})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts/"+testG)
	var acct struct {
		Data  v1.AccountStateView `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &acct)
	if acct.Data.AsOfLedger != cachedLedger {
		t.Errorf("account as_of_ledger = %d, want the cached state's own vintage %d (not the serve-time watermark %d)",
			acct.Data.AsOfLedger, cachedLedger, serveTimeLedger)
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

// TestExplorer_AccountState_RejectsChecksumInvalidStrkey: a strkey with valid shape but a corrupted CRC must get 400 before
// reaching the lake reader, as sibling endpoints do via canonical.IsAccountID; a shape-only check let it through.
func TestExplorer_AccountState_RejectsChecksumInvalidStrkey(t *testing.T) {
	reader := &stubExplorerReader{accountState: clickhouse.AccountState{Exists: true, Balance: 1}}
	base := explorerTestServer(t, reader)

	// Same shape as testG (56 chars, 'G'-prefixed, valid base32 alphabet)
	// but with the last character mutated — breaks the CRC16 checksum
	// while keeping looksLikeStellarAccount's shape-only check happy.
	badChecksum := testG[:len(testG)-1] + "A"
	if badChecksum == testG {
		t.Fatal("test setup: mutated strkey must differ from testG")
	}

	resp := mustGet(t, base+"/v1/accounts/"+badChecksum)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (checksum-invalid G-strkey must be rejected before reaching the lake reader)", resp.StatusCode)
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

// TestExplorer_AccountsList_Watermark pins ADR-0041 Decision 4 on the /v1/accounts wealth ranking: `as_of_ledger` is the
// ranking snapshot's own ledger, not the serve-time watermark, and `flags.stale` fires when the watermark trails now.
func TestExplorer_AccountsList_Watermark(t *testing.T) {
	reader := &stubExplorerReader{
		wealth:       []clickhouse.AccountWealth{{AccountID: testG, Value: big.NewRat(12345, 100)}},
		wealthLedger: 63_888_888,
	}
	srv := v1.New(v1.Options{
		Explorer:      reader,
		Prices:        &stubPriceReader{},
		LakeWatermark: &wmStub{ledger: 63_888_999, closedAt: time.Now().Add(-10 * time.Minute)},
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data  v1.AccountsListView `json:"data"`
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if env.Data.AsOfLedger != 63_888_888 {
		t.Errorf("as_of_ledger = %d, want 63888888", env.Data.AsOfLedger)
	}
	if !env.Flags.Stale {
		t.Error("flags.stale should fire for a 10-minute-old watermark")
	}
	if len(env.Data.Accounts) != 1 || env.Data.Accounts[0].AccountID != testG {
		t.Errorf("accounts = %+v, want one row for testG", env.Data.Accounts)
	}
}

func getAccountStateMap(t *testing.T, st clickhouse.AccountState) map[string]any {
	t.Helper()
	base := explorerTestServer(t, &stubExplorerReader{accountState: st})
	resp := mustGet(t, base+"/v1/accounts/"+testG)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	mustDecode(t, resp, &body)
	return body.Data
}

func servedInt(t *testing.T, data map[string]any, key string) *big.Int {
	t.Helper()
	var n big.Int
	switch v := data[key].(type) {
	case float64:
		n.SetInt64(int64(v))
	case string:
		if _, ok := n.SetString(v, 10); !ok {
			t.Fatalf("%s = %q, not an integer string", key, v)
		}
	default:
		t.Fatalf("%s missing from the served account (got %#v)", key, data[key])
	}
	return &n
}

// The served fields must make minimum balance and spendable XLM derivable. Example: 3 sponsored subentries, 5 XLM, 1 XLM
// in selling liabilities gives reserve (2+3+0-3) x 0.5 = 1 XLM, plus 1 locked = 2, so 3 XLM spendable.
// Without sponsorship counters and liabilities only (2+3) x 0.5 = 2.5 is derivable.
func TestExplorer_AccountState_ServesReserveInputs(t *testing.T) {
	data := getAccountStateMap(t, clickhouse.AccountState{
		Exists: true, Balance: 50_000_000, NumSubEntries: 3,
		NumSponsored: 3, NumSponsoring: 0,
		BuyingLiabilities: 0, SellingLiabilities: 10_000_000,
	})
	const baseReserve = 5_000_000
	slots := new(big.Int).Add(big.NewInt(2), servedInt(t, data, "num_subentries"))
	slots.Add(slots, servedInt(t, data, "num_sponsoring"))
	slots.Sub(slots, servedInt(t, data, "num_sponsored"))
	locked := new(big.Int).Mul(slots, big.NewInt(baseReserve))
	locked.Add(locked, servedInt(t, data, "selling_liabilities"))
	avail := new(big.Int).Sub(servedInt(t, data, "balance"), locked)
	if locked.Int64() != 20_000_000 || avail.Int64() != 30_000_000 {
		t.Errorf("derived locked=%s spendable=%s stroops, want 20000000/30000000", locked, avail)
	}
	if servedInt(t, data, "buying_liabilities").Sign() != 0 {
		t.Errorf("buying_liabilities = %v, want \"0\"", data["buying_liabilities"])
	}
}

// A live account's zero sub-entry count and zero flags are facts, not absent.
func TestExplorer_AccountState_ServesZeroCountsAndFlags(t *testing.T) {
	data := getAccountStateMap(t, clickhouse.AccountState{Exists: true, Balance: 1})
	for _, k := range []string{"num_subentries", "flags", "num_sponsoring", "num_sponsored"} {
		if servedInt(t, data, k).Sign() != 0 {
			t.Errorf("%s = %v, want 0", k, data[k])
		}
	}
	gone := getAccountStateMap(t, clickhouse.AccountState{Exists: false})
	for _, k := range []string{"num_subentries", "flags", "balance", "coverage_note"} {
		if _, present := gone[k]; present {
			t.Errorf("exists:false body carries %s = %v", k, gone[k])
		}
	}
}

// Pool shares are labelled, trustline liabilities served, and the view
// declares the holding domains it does not cover.
func TestExplorer_AccountState_TrustlineKindsAndHoldingScope(t *testing.T) {
	pool := "pool:7b2c0000000000000000000000000000000000000000000000000000000000ff"
	data := getAccountStateMap(t, clickhouse.AccountState{
		Exists: true, Balance: 1,
		Trustlines: []clickhouse.TrustlineState{
			{Asset: "USDC-" + testG, Balance: 500, Limit: 1000, BuyingLiabilities: 40, SellingLiabilities: 250},
			{Asset: pool, Balance: 183_920_000, Limit: 9_000_000_000, PoolShare: true},
		},
	})
	tls, _ := data["trustlines"].([]any)
	if len(tls) != 2 {
		t.Fatalf("trustlines = %#v, want 2", data["trustlines"])
	}
	usdc, _ := tls[0].(map[string]any)
	share, _ := tls[1].(map[string]any)
	if usdc["kind"] != "asset" || usdc["selling_liabilities"] != "250" || usdc["buying_liabilities"] != "40" {
		t.Errorf("asset trustline = %#v, want kind asset + liabilities 40/250", usdc)
	}
	if share["kind"] != "pool_share" || share["asset"] != pool {
		t.Errorf("pool-share trustline = %#v, want kind pool_share", share)
	}
	note, _ := data["coverage_note"].(string)
	for _, want := range []string{"Claimable balances", "SAC", "pool_share"} {
		if !strings.Contains(note, want) {
			t.Errorf("coverage_note %q does not name %q", note, want)
		}
	}
}

// The wealth ranking counts classic holdings only, so it is a lower bound.
func TestExplorer_AccountsList_DeclaresClassicOnlyLowerBound(t *testing.T) {
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{wealth: []clickhouse.AccountWealth{{AccountID: testG, Value: big.NewRat(1, 1)}}},
		Prices:   &stubPriceReader{},
	})
	resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/accounts")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data["lower_bound"] != true {
		t.Errorf("lower_bound = %v, want true", env.Data["lower_bound"])
	}
	note, _ := env.Data["coverage_note"].(string)
	if !strings.Contains(note, "Claimable balances") || !strings.Contains(note, "SAC") {
		t.Errorf("coverage_note %q must name the excluded holding domains", note)
	}
}
