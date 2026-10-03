package v1_test

import (
	"math/big"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A held (frozen) price or a list missing rows is replaced by the origin's
// next answer once the fault clears, so it must not ride the route's band.

func cacheControlOf(t *testing.T, srv *v1.Server, path string) string {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", path, resp.StatusCode)
	}
	return resp.Header.Get("Cache-Control")
}

func TestFrozenPriceServesAreNoStore(t *testing.T) {
	for _, tc := range []struct {
		path   string
		frozen frozenPairs
		held   lkgPairs
		reader func() *stubPriceReader
	}{
		{lastPriceURL, frozenPairs{xlmUSD: true}, lkgPairs{xlmUSD + "/300": heldUSD}, movedUSDBucketReader},
		{xLastURL, frozenPairs{xlmUSD: true}, lkgPairs{xlmUSD + "/300": heldUSD}, movedUSDBucketReader},
		{"/v1/price?asset=crypto:XLM&quote=fiat:GBP", frozenPairs{xlmGBP: true}, lkgPairs{xlmGBP + "/300": heldLKG}, movedBucketReader},
		{"/v1/price/batch?asset_ids=crypto:XLM&quote=fiat:GBP", frozenPairs{xlmGBP: true}, lkgPairs{xlmGBP + "/300": heldLKG}, movedBucketReader},
	} {
		t.Run(tc.path, func(t *testing.T) {
			unfrozen := v1.New(v1.Options{Prices: tc.reader(), Freeze: frozenPairs{}, Triangulated: tc.held})
			if got := cacheControlOf(t, unfrozen, tc.path); got == "no-store" {
				t.Errorf("unfrozen: Cache-Control = %q, want the route band", got)
			}
			frozen := v1.New(v1.Options{Prices: tc.reader(), Freeze: tc.frozen, Triangulated: tc.held})
			if got := cacheControlOf(t, frozen, tc.path); got != "no-store" {
				t.Errorf("frozen: Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestProtocolsList_OmittedRowsAreNoStore(t *testing.T) {
	roster := map[string][]timescale.ProtocolContract{
		"blend": {{Source: "blend", ContractID: "CPOOL1", FactoryID: "CFACTORY1", FirstLedger: 51_500_000}},
	}
	healthy := v1.New(v1.Options{ProtocolContracts: &rosterCacheStubReader{bySource: roster}})
	if got := cacheControlOf(t, healthy, "/v1/protocols"); got != "public, max-age=60" {
		t.Errorf("full roster: Cache-Control = %q, want public, max-age=60", got)
	}
	failing := v1.New(v1.Options{ProtocolContracts: &rosterCacheStubReader{bySource: roster, errFor: map[string]bool{"blend": true}}})
	if got := cacheControlOf(t, failing, "/v1/protocols"); got != "no-store" {
		t.Errorf("roster read failed: Cache-Control = %q, want no-store", got)
	}
}

func TestPoolReserves_DisplayOutageIsNoStore(t *testing.T) {
	pairA := mkCStrkey(t, 1)
	tok0, tok1 := mkCStrkey(t, 10), mkCStrkey(t, 11)
	pairs := []timescale.SoroswapPair{{PairStrkey: pairA, Token0Strkey: tok0, Token1Strkey: tok1}}
	reader := func(displaysErr error) *stubExplorerReader {
		return &stubExplorerReader{
			pairStates: map[string]clickhouse.SoroswapPairState{
				pairA: {Pair: pairA, Token0: tok0, Token1: tok1, Reserve0: big.NewInt(1_000_000), Reserve1: big.NewInt(2_000_000), Ledger: 62_941_880},
			},
			tokenDisplays:    map[string]clickhouse.TokenDisplayMeta{tok0: {Decimals: 7, HasMeta: true}, tok1: {Decimals: 7, HasMeta: true}},
			tokenDisplaysErr: displaysErr,
		}
	}
	for _, tc := range []struct {
		name    string
		err     error
		noStore bool
	}{{"healthy", nil, false}, {"display outage", errTokenDisplaysDown, true}} {
		resp := mustGet(t, poolReservesTestServer(t, reader(tc.err), pairs)+"/v1/pools/reserves")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.name, resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); (got == "no-store") != tc.noStore {
			t.Errorf("%s: Cache-Control = %q, want no-store=%v", tc.name, got, tc.noStore)
		}
	}
}
