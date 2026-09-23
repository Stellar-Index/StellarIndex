package v1_test

import (
	"math/big"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

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

// The served fields must make minimum balance and spendable XLM derivable.
// An account with 3 sponsored subentries, 5 XLM and 1 XLM locked by offers:
// reserve = (2 + 3 + 0 - 3) × 0.5 XLM = 1 XLM, plus 1 XLM selling
// liabilities locked = 2 XLM, so 3 XLM spendable. Without the sponsorship
// counters and liabilities the only derivable figure is (2 + 3) × 0.5 = 2.5.
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
		Explorer: &stubExplorerReader{wealth: []clickhouse.AccountWealth{{AccountID: testG, USD: 1}}},
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
