package v1_test

import (
	"encoding/csv"
	"math/big"
	"net/http"
	"reflect"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestHistoryCSV(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 100) // > 2^53 and > 2^64
	big1 := mkHistTrade(0)
	big1.BaseAmount = canonical.NewAmount(huge)
	big1.QuoteAmount = canonical.NewAmount(new(big.Int).Lsh(huge, 1))
	trades := []canonical.Trade{big1, mkHistTrade(101)}
	srv := v1.New(v1.Options{History: &stubHistoryReader{trades: trades}})
	ts := httpTestServer(t, srv)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/history?base=native&quote=fiat:USD&limit=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/csv")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("code=%d content-type=%q, want 200 text/csv", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Vary") == "" || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Errorf("Vary=%q Cache-Control=%q, want Accept and private, no-store", resp.Header.Get("Vary"), resp.Header.Get("Cache-Control"))
	}
	if link := resp.Header.Get("Link"); link == "" {
		t.Error("full page sent no Link rel=next")
	}
	records, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("body is not CSV: %v", err)
	}
	wantHeader := []string{
		"source", "ledger", "tx_hash", "op_index", "ts", "base_asset", "quote_asset",
		"base_amount", "quote_amount", "price", "base_decimals", "quote_decimals", "routed_via",
	}
	if len(records) != 3 || !reflect.DeepEqual(records[0], wantHeader) {
		t.Fatalf("records = %q, want header %q + 2 rows", records, wantHeader)
	}
	row := map[string]string{}
	for i, c := range records[0] {
		row[c] = records[1][i]
	}
	if row["base_amount"] != "1267650600228229401496703205376" || row["quote_amount"] != "2535301200456458802993406410752" {
		t.Errorf("amounts = %q / %q, want the exact 2^100 / 2^101 integer strings", row["base_amount"], row["quote_amount"])
	}
	if row["price"] != "2.0000000000" || row["source"] != "soroswap" || row["base_asset"] != "native" {
		t.Errorf("row = %q, want price 2.0000000000, source soroswap, base native", row)
	}
}
