package v1_test

import (
	"net/http"
	"sort"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// offChainSources is every registered source that a single-source
// filter must refuse: CEX + FX venues, aggregators, sovereign anchors,
// and the off-chain oracles (Chainlink, Tiingo).
func offChainSources(t *testing.T) []string {
	t.Helper()
	var out []string
	for name := range external.Registry {
		if !external.IsOnChain(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	for _, must := range []string{"binance", "kraken", "coingecko", "coinmarketcap", "massive", "exchangeratesapi", "ecb", "chainlink", "tiingo"} {
		if i := sort.SearchStrings(out, must); i == len(out) || out[i] != must {
			t.Fatalf("%s is not classed off-chain; the filter would let it be selected alone", must)
		}
	}
	return out
}

func assertOffChainRefused(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var p v1.Problem
	mustDecode(t, resp, &p)
	if p.Type != "https://api.stellarindex.io/errors/off-chain-source-filter" {
		t.Errorf("Type = %q, want off-chain-source-filter", p.Type)
	}
}

// TestSourceFilter_OffChainRefused pins that no route selects one
// off-chain provider's data on its own.
func TestSourceFilter_OffChainRefused(t *testing.T) {
	oracle := httpTestServer(t, v1.New(v1.Options{Oracle: &stubOracleReader{}}))
	markets := httpTestServer(t, v1.New(v1.Options{Markets: &stubMarketsReader{}}))
	obs := startHTTPTest(t, v1.New(v1.Options{History: &stubHistoryReader{}}).Handler())
	stream := startObservationsStreamServer(t, &stubHistoryReader{})

	for _, src := range offChainSources(t) {
		t.Run(src, func(t *testing.T) {
			assertOffChainRefused(t, mustGet(t, oracle.URL+"/v1/oracle/latest?asset=native&source="+src))
			assertOffChainRefused(t, mustGet(t, markets.URL+"/v1/markets?source="+src))
			assertOffChainRefused(t, mustGet(t, obs.URL+"/v1/observations?asset=native&quote=fiat:USD&source="+src))
			assertOffChainRefused(t, mustGet(t, stream+"/v1/observations/stream?asset=native&quote=fiat:USD&source="+src))
		})
	}
}

// TestSourceFilter_OnChainAccepted — on-chain sources (DEX venues and
// the Soroban oracles) stay selectable.
func TestSourceFilter_OnChainAccepted(t *testing.T) {
	reader := &stubOracleReader{}
	oracle := httpTestServer(t, v1.New(v1.Options{Oracle: reader}))
	for _, src := range []string{"reflector-dex", "band", "redstone"} {
		if resp := mustGet(t, oracle.URL+"/v1/oracle/latest?asset=native&source="+src); resp.StatusCode != http.StatusOK {
			t.Errorf("oracle/latest source=%s: status = %d, want 200", src, resp.StatusCode)
		}
		if reader.lastSource != src {
			t.Errorf("oracle/latest source=%s reached the reader as %q", src, reader.lastSource)
		}
	}
	markets := httpTestServer(t, v1.New(v1.Options{Markets: &stubMarketsReader{}}))
	if resp := mustGet(t, markets.URL+"/v1/markets?source=soroswap"); resp.StatusCode != http.StatusOK {
		t.Errorf("markets source=soroswap: status = %d, want 200", resp.StatusCode)
	}
	obs := startHTTPTest(t, v1.New(v1.Options{History: &stubHistoryReader{}}).Handler())
	if resp := mustGet(t, obs.URL+"/v1/observations?asset=native&quote=fiat:USD&source=sdex"); resp.StatusCode != http.StatusOK {
		t.Errorf("observations source=sdex: status = %d, want 200", resp.StatusCode)
	}
}
