package v1_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A count majority of dust prints must not become the single-bar OHLC
// by trimming a volume majority: 3 × 1,000,000 XLM at 0.100 and
// 4 × 30,000 XLM at 0.114 used to serve a 200 bar of the 4 wash prints
// alone. The window is contested, so the default filter withholds it
// as all-filtered rather than serving either side or claiming "no trades".
func TestOHLC_DustCountMajorityWindowIsWithheldNotServed(t *testing.T) {
	t0 := time.Unix(1_772_000_000, 0).UTC()
	trades := make([]canonical.Trade, 0, 7)
	for i := 0; i < 3; i++ {
		trades = append(trades, mkOHLCTrade(10_000_000_000_000, 1_000_000_000_000, t0.Add(time.Duration(i)*10*time.Second)))
	}
	for i := 0; i < 4; i++ {
		trades = append(trades, mkOHLCTrade(300_000_000_000, 34_200_000_000, t0.Add(time.Duration(i)*10*time.Second+5*time.Second)))
	}
	ts := httpTestServer(t, v1.New(v1.Options{History: &stubHistoryReader{trades: trades}}))

	resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote=fiat:USD")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (contested window withheld); body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "all-filtered") {
		t.Errorf("body should cite all-filtered: %s", body)
	}
}
