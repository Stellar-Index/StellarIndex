package v1_test

import (
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

func assertProblem(t *testing.T, resp *http.Response, wantType string) {
	t.Helper()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/"+wantType {
		t.Errorf("Type = %q, want %s", p.Type, wantType)
	}
}

// TestSourceFilter_Classes pins which sources a single-source selector
// accepts on every route: on-chain sources and CEX venues answer; data
// vendors (aggregator, FX, oracle vendors) and unknown names are 400.
func TestSourceFilter_Classes(t *testing.T) {
	oracle := httpTestServer(t, v1.New(v1.Options{Oracle: &stubOracleReader{}}))
	markets := httpTestServer(t, v1.New(v1.Options{Markets: &stubMarketsReader{}}))
	obs := startHTTPTest(t, v1.New(v1.Options{History: &stubHistoryReader{}}).Handler())
	stream := startObservationsStreamServer(t, &stubHistoryReader{})

	cases := []struct {
		name, src string
		wantType  string // "" = accepted
	}{
		{"on-chain dex", "soroswap", ""},
		{"on-chain oracle", "band", ""},
		{"cex", "binance", ""},
		{"aggregator", "coingecko", "off-chain-source-filter"},
		{"fx provider", "massive", "off-chain-source-filter"},
		{"sovereign anchor", "ecb", "off-chain-source-filter"},
		{"oracle vendor", "chainlink", "off-chain-source-filter"},
		{"fund-nav oracle vendor", "tiingo", "off-chain-source-filter"},
		{"unknown", "no-such-source", "unknown-source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			urls := []string{
				oracle.URL + "/v1/oracle/latest?asset=native&source=" + tc.src,
				markets.URL + "/v1/markets?source=" + tc.src,
				obs.URL + "/v1/observations?asset=native&quote=fiat:USD&source=" + tc.src,
			}
			for _, u := range urls {
				resp := mustGet(t, u)
				if tc.wantType == "" {
					if resp.StatusCode != http.StatusOK {
						t.Errorf("%s: status = %d, want 200", u, resp.StatusCode)
					}
					continue
				}
				assertProblem(t, resp, tc.wantType)
			}
			// The stream holds a 200 open, so only the refusal is asserted.
			if tc.wantType != "" {
				assertProblem(t, mustGet(t, stream+"/v1/observations/stream?asset=native&quote=fiat:USD&source="+tc.src), tc.wantType)
			}
		})
	}
}

// TestSourceFilter_RegistryPartition walks the registry so a newly
// registered source must fall on one side: only on-chain sources and
// Class=Exchange/Subclass=CEX venues are selectable.
func TestSourceFilter_RegistryPartition(t *testing.T) {
	markets := httpTestServer(t, v1.New(v1.Options{Markets: &stubMarketsReader{}}))
	cex := map[string]bool{}
	for _, n := range v1.CexSourceNames() {
		cex[n] = true
	}
	for _, must := range []string{"binance", "kraken", "bitstamp", "coinbase"} {
		if !cex[must] {
			t.Fatalf("%s is not classed as a CEX venue", must)
		}
	}
	for name := range external.Registry {
		want := external.IsOnChain(name) || cex[name]
		resp := mustGet(t, markets.URL+"/v1/markets?source="+name)
		if got := resp.StatusCode == http.StatusOK; got != want {
			t.Errorf("markets source=%s: ok=%v, want %v", name, got, want)
		}
	}
}

// TestSourceFilter_SelectableAgreesWithRefusal pins that the explorer's
// gate (`selectable` on /v1/sources) and the 400 are one predicate.
func TestSourceFilter_SelectableAgreesWithRefusal(t *testing.T) {
	srv := v1.New(v1.Options{Markets: &stubMarketsReader{}})
	ts := httpTestServer(t, srv)
	var body struct {
		Data []v1.Source `json:"data"`
	}
	mustDecode(t, mustGet(t, ts.URL+"/v1/sources"), &body)
	if len(body.Data) == 0 {
		t.Fatal("no sources listed")
	}
	for _, s := range body.Data {
		resp := mustGet(t, ts.URL+"/v1/markets?source="+s.Name)
		if got := resp.StatusCode == http.StatusOK; got != s.Selectable {
			t.Errorf("%s: selectable=%v but /v1/markets status=%d", s.Name, s.Selectable, resp.StatusCode)
		}
	}
}
