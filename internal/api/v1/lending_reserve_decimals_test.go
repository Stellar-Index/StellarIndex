package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"sync"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// perContractDecimals is a v1.TokenDecimalsReader answering per contract;
// a contract absent from the map reports found=false (uncaptured metadata).
// buildReserveView runs under forEachBounded, so consulted is mutex-guarded.
type perContractDecimals struct {
	byContract map[string]uint32
	mu         sync.Mutex
	consulted  map[string]bool
}

func newPerContractDecimals(byContract map[string]uint32) *perContractDecimals {
	return &perContractDecimals{byContract: byContract, consulted: map[string]bool{}}
}

func (d *perContractDecimals) TokenDecimals(_ context.Context, contractID string) (uint32, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.consulted[contractID] = true
	v, ok := d.byContract[contractID]
	return v, ok, nil
}

func (d *perContractDecimals) wasConsulted(contractID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.consulted[contractID]
}

func reserveState(pool, asset string, dec uint32, found bool, supplied, borrowed *big.Int) clickhouse.BlendReserveState {
	return clickhouse.BlendReserveState{
		Pool: pool, Asset: asset, Decimals: dec, DecimalsFound: found,
		Metrics: blend.ReserveMetrics{SuppliedUnderlying: supplied, BorrowedUnderlying: borrowed},
	}
}

func getReserves(t *testing.T, srv *v1.Server, pool string) v1.LendingPoolReservesView {
	t.Helper()
	resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/lending/pools/"+pool+"/reserves")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.LendingPoolReservesView `json:"data"`
	}
	mustDecode(t, resp, &env)
	return env.Data
}

func usdKey(t *testing.T, assetID string) string {
	t.Helper()
	a, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatalf("parse %q: %v", assetID, err)
	}
	return a.String() + "/fiat:USD"
}

func strOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
