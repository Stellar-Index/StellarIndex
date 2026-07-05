package v1_test

import (
	"net/http"
	"testing"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
)

// TestSourceHealth_KnownSource — the endpoint serves the registry
// projection even on a bare server (no stats readers wired): the
// worst case is zeroed counters, never an error.
func TestSourceHealth_KnownSource(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/sources/binance/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.SourceHealthRow `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Name != "binance" {
		t.Errorf("name = %q, want binance", env.Data.Name)
	}
	if env.Data.Class != "exchange" {
		t.Errorf("class = %q, want exchange", env.Data.Class)
	}
	if env.Data.Subclass != "cex" {
		t.Errorf("subclass = %q, want cex", env.Data.Subclass)
	}
	if !env.Data.IncludeInVWAP {
		t.Error("include_in_vwap = false, want true for binance")
	}
	// Stats readers aren't wired in this test server, so the live
	// counters must degrade to zero rather than erroring.
	if env.Data.TradeCount24h != 0 || env.Data.Entries24h != 0 {
		t.Errorf("expected zeroed stats on a reader-less server, got trades=%d entries=%d",
			env.Data.TradeCount24h, env.Data.Entries24h)
	}
}

// TestSourceHealth_OnChainSource — a projected on-chain venue resolves
// too (the registry covers both sides of the on-chain/off-chain split).
func TestSourceHealth_OnChainSource(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/sources/soroswap/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.SourceHealthRow `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Name != "soroswap" || env.Data.Subclass != "dex" {
		t.Errorf("got name=%q subclass=%q, want soroswap/dex", env.Data.Name, env.Data.Subclass)
	}
}

// TestSourceHealth_UnknownSourceIs404 — the registry is the 404
// boundary; typos get a problem+json, not an empty row.
func TestSourceHealth_UnknownSourceIs404(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/sources/not-a-venue/health")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unregistered source", resp.StatusCode)
	}
	assertProblemNoStore(t, resp)
}
