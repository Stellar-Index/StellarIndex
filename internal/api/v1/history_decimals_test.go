package v1_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestHistory_PerSideDecimals: the additive base_decimals/quote_decimals
// fields resolve the Soroban token's declared decimals() for the base
// side (via the TokenDecimals reader) and default 7 for a native/classic
// quote — resolved ONCE per request and stamped on every row.
func TestHistory_PerSideDecimals(t *testing.T) {
	reader := &stubHistoryReader{trades: []canonical.Trade{mkHistTrade(100), mkHistTrade(101)}}
	stub := &decStub{d: 6, found: true}
	srv := v1.New(v1.Options{History: reader, TokenDecimals: stub})
	ts := httpTestServer(t, srv)

	// base = Soroban token (6 decimals per stub), quote = native (7).
	resp := mustGet(t, ts.URL+"/v1/history?base="+decTestContract+"&quote=native&limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	var env struct {
		Data []v1.TradeRow `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 2 {
		t.Fatalf("got %d rows, want 2", len(env.Data))
	}
	for i, row := range env.Data {
		if row.BaseDecimals != 6 {
			t.Errorf("row %d base_decimals = %d, want 6 (Soroban resolved)", i, row.BaseDecimals)
		}
		if row.QuoteDecimals != 7 {
			t.Errorf("row %d quote_decimals = %d, want 7 (native default)", i, row.QuoteDecimals)
		}
	}
	// Resolved once per side (not per row) — the reader is consulted for
	// the Soroban base contract.
	if stub.gotContract != decTestContract {
		t.Errorf("consulted contract = %q, want %q", stub.gotContract, decTestContract)
	}
	// Both decimals keys present on /v1/history rows (2 rows × 2 keys).
	if n := strings.Count(string(body), `"base_decimals"`); n != 2 {
		t.Errorf("base_decimals key count = %d, want 2; body=%s", n, body)
	}
}

// TestHistory_ClassicNeverConsultsDecimals: a classic/native pair resolves
// to 7 on both sides WITHOUT consulting the token-decimals reader (a wrong
// overlay would mis-scale every amount on the markets page).
func TestHistory_ClassicDecimalsNoReaderConsult(t *testing.T) {
	reader := &stubHistoryReader{trades: []canonical.Trade{mkHistTrade(100)}}
	stub := &decStub{d: 18, found: true} // would lie if consulted
	srv := v1.New(v1.Options{History: reader, TokenDecimals: stub})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&limit=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.TradeRow `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 {
		t.Fatalf("got %d rows, want 1", len(env.Data))
	}
	if env.Data[0].BaseDecimals != 7 || env.Data[0].QuoteDecimals != 7 {
		t.Errorf("native/fiat decimals = (%d,%d), want (7,7)",
			env.Data[0].BaseDecimals, env.Data[0].QuoteDecimals)
	}
	if stub.gotContract != "" {
		t.Errorf("classic/native pair consulted the decimals reader (contract %q)", stub.gotContract)
	}
}
