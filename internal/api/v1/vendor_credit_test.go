package v1_test

import (
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestOracleLatest_CreditsLicensedVendor: a per-source row from a licensed aggregator names its vendor; an on-chain oracle
// row carries no credit key.
func TestOracleLatest_CreditsLicensedVendor(t *testing.T) {
	reader := &stubOracleReader{updates: []canonical.OracleUpdate{
		mkReflectorUpdate("coingecko", "12000000000000", 14),
		mkReflectorUpdate("reflector-dex", "12500000000000", 14),
	}}
	ts := httpTestServer(t, v1.New(v1.Options{Oracle: reader}))
	resp := mustGet(t, ts.URL+"/v1/oracle/latest?asset=native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var page struct {
		Data []v1.OracleReading `json:"data"`
	}
	mustDecode(t, resp, &page)
	credits := map[string]string{}
	for _, r := range page.Data {
		credits[r.Source] = r.Credit
	}
	if credits["coingecko"] != "CoinGecko" || credits["reflector-dex"] != "" {
		t.Errorf("credits = %v, want coingecko credited and reflector-dex bare", credits)
	}
}

// TestSourcesAndMethodology_CreditLicensedVendors pins `credit` on the two source registries the explorer renders.
func TestSourcesAndMethodology_CreditLicensedVendors(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{}))

	resp := mustGet(t, ts.URL+"/v1/sources")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/sources status = %d", resp.StatusCode)
	}
	var sources struct {
		Data []v1.Source `json:"data"`
	}
	mustDecode(t, resp, &sources)
	bySource := map[string]v1.Source{}
	for _, s := range sources.Data {
		bySource[s.Name] = s
	}
	if bySource["coingecko"].Credit != "CoinGecko" || !strings.Contains(bySource["ecb"].Credit, "European Central Bank") || bySource["sdex"].Credit != "" {
		t.Errorf("/v1/sources credits: coingecko=%q ecb=%q sdex=%q", bySource["coingecko"].Credit, bySource["ecb"].Credit, bySource["sdex"].Credit)
	}

	resp = mustGet(t, ts.URL+"/v1/methodology")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/methodology status = %d", resp.StatusCode)
	}
	var method struct {
		Data v1.Methodology `json:"data"`
	}
	mustDecode(t, resp, &method)
	byMethod := map[string]v1.MethodologySource{}
	for _, s := range method.Data.Sources {
		byMethod[s.Name] = s
	}
	if byMethod["coingecko"].Credit != "CoinGecko" || byMethod["sdex"].Credit != "" {
		t.Errorf("/v1/methodology credits: coingecko=%q sdex=%q", byMethod["coingecko"].Credit, byMethod["sdex"].Credit)
	}
}
