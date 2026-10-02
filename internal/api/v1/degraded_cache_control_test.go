package v1_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A 200 whose body a failed read cut short must not carry the route's
// shared-cache band: a CDN would replay it to every caller for the whole
// s-maxage after the dependency recovered. Each case pairs the degraded
// exit with its healthy twin so the assertion cannot pass on a route that
// is uncacheable anyway.
func TestDegraded200DropsCacheBand(t *testing.T) {
	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	issuerSrv := func(assetsErr error) *v1.Server {
		return v1.New(v1.Options{Issuers: &stubIssuersReader{
			row:     timescale.IssuerRow{GStrkey: issuer, HomeDomain: "centre.io"},
			assetsE: assetsErr,
		}})
	}
	rosterSrv := func(fail bool) *v1.Server {
		return v1.New(v1.Options{ProtocolContracts: &rosterCacheStubReader{
			bySource: map[string][]timescale.ProtocolContract{
				"blend": {{Source: "blend", ContractID: "CPOOL1", FactoryID: "CFACTORY1", FirstLedger: 51_500_000}},
			},
			errFor: map[string]bool{"blend": fail},
		}})
	}
	assetSrv := func(dec *countingDecStub) *v1.Server {
		return v1.New(v1.Options{Prices: lockstepPrices(), Supply: lockstepSupply(), TokenDecimals: dec})
	}
	f2Srv := func(sup *stubSupplyLooker, vol *stubVolumeReader, chg *stubChange24hReader) *v1.Server {
		return v1.New(v1.Options{
			Prices: lockstepPrices(), Supply: sup, Volume: vol, Change24h: chg,
			TokenDecimals: &countingDecStub{d: 7, found: true},
		})
	}
	healthyChange := func() *stubChange24hReader {
		return &stubChange24hReader{prices: map[string]string{flaggedAsset: "1"}}
	}
	f2Healthy := f2Srv(lockstepSupply(), &stubVolumeReader{volume: "100"}, healthyChange())

	for _, tc := range []struct {
		name              string
		path              string
		healthy, degraded *v1.Server
	}{
		{
			name:     "issuer asset list read failed",
			path:     "/v1/issuers/" + issuer,
			healthy:  issuerSrv(nil),
			degraded: issuerSrv(errors.New("assets fetch broke")),
		},
		{
			name:     "protocol roster read failed",
			path:     "/v1/protocols",
			healthy:  rosterSrv(false),
			degraded: rosterSrv(true),
		},
		{
			// Stale-flagged and written through the pre-rendered body path,
			// not writeEnvelope.
			name:     "asset decimals read failed",
			path:     "/v1/assets/" + flaggedAsset,
			healthy:  assetSrv(&countingDecStub{d: 7, found: true}),
			degraded: assetSrv(&countingDecStub{err: errDecimalsRead}),
		},
		{
			name:     "asset supply read failed",
			path:     "/v1/assets/" + flaggedAsset,
			healthy:  f2Healthy,
			degraded: f2Srv(&stubSupplyLooker{err: errors.New("supply read broke")}, &stubVolumeReader{volume: "100"}, healthyChange()),
		},
		{
			name:     "asset volume read failed",
			path:     "/v1/assets/" + flaggedAsset,
			healthy:  f2Healthy,
			degraded: f2Srv(lockstepSupply(), &stubVolumeReader{err: errors.New("volume read broke")}, healthyChange()),
		},
		{
			name:     "asset change_24h read failed",
			path:     "/v1/assets/" + flaggedAsset,
			healthy:  f2Healthy,
			degraded: f2Srv(lockstepSupply(), &stubVolumeReader{volume: "100"}, &stubChange24hReader{err: errors.New("change read broke")}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if cc := cacheControlOf(t, tc.healthy, tc.path); !strings.HasPrefix(cc, "public") {
				t.Fatalf("healthy Cache-Control = %q, want the route's public band", cc)
			}
			if cc := cacheControlOf(t, tc.degraded, tc.path); cc != "no-store" {
				t.Errorf("degraded Cache-Control = %q, want no-store", cc)
			}
		})
	}
}

func cacheControlOf(t *testing.T, srv *v1.Server, path string) string {
	t.Helper()
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+path)
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	return resp.Header.Get("Cache-Control")
}
