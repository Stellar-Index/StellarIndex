package client_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Stellar-Index/StellarIndex/pkg/client"
)

// TestLendingPoolTopAccounts_QueryAndExactAmounts pins the path and query, and that amounts above 2^63 decode as exact strings.
func TestLendingPoolTopAccounts_QueryAndExactAmounts(t *testing.T) {
	const pool = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/lending/pools/"+pool+"/top-accounts" || r.URL.RawQuery != "asset=CAS3&limit=5&side=borrow" {
			t.Errorf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"pool":"` + pool + `","protocol":"blnt","side":"borrow","asset":"CAS3","ranking":"amount",
			"accounts":1,"lower_bound":true,"excluded":[{"kind":"borrow","pool":"` + pool + `","asset":null,"reason":"positions_truncated"}],
			"unattributed":[{"asset":"CAS3","side":"borrow","tokens":"36893488147419103232","amount":"40582836962161013554",
			"share_of_reserve":"0.500000"}],
			"rows":[{"rank":1,"account":"GA","asset":"CAS3","decimals":7,"tokens":"18446744073709551616",
			"amount":"20291418481080506777","value_usd":null,"usd_reason":"unpriced","share_of_reserve":"0.500000",
			"last_modified_ledger":10,"rank_lower_bound":true}],"as_of_ledger":11,"note":"n"},"as_of":"2026-11-16T12:00:00Z","flags":{}}`))
	})
	got, err := c.LendingPoolTopAccounts(context.Background(), pool, client.LendingTopAccountsQuery{Side: "borrow", Asset: "CAS3", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	d := got.Data
	if d.Protocol != "blnt" || d.Ranking != "amount" || !d.LowerBound || len(d.Excluded) != 1 || d.Excluded[0].Reason != "positions_truncated" {
		t.Fatalf("view = %+v", d)
	}
	if len(d.Unattributed) != 1 || d.Unattributed[0].Amount != "40582836962161013554" || d.Unattributed[0].ShareOfReserve != "0.500000" {
		t.Fatalf("unattributed = %+v", d.Unattributed)
	}
	if len(d.Rows) != 1 || d.Rows[0].Amount != "20291418481080506777" || d.Rows[0].ValueUSD != nil || *d.Rows[0].UsdReason != "unpriced" ||
		!d.Rows[0].RankLowerBound {
		t.Fatalf("rows = %+v", d.Rows)
	}
}
