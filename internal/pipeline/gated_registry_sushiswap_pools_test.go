package pipeline

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeSushiStore layers an in-memory sushiswap_v3_pools double over the
// protocol_contracts one.
type fakeSushiStore struct {
	fakeProtocolContractStore
	pools   []timescale.SushiswapV3Pool
	written []timescale.SushiswapV3Pool
}

func (f *fakeSushiStore) LoadSushiswapV3Pools(context.Context) ([]timescale.SushiswapV3Pool, error) {
	return f.pools, nil
}

func (f *fakeSushiStore) UpsertSushiswapV3Pool(_ context.Context, p timescale.SushiswapV3Pool) error {
	f.written = append(f.written, p)
	return nil
}

func TestGatedRegistryOptions_SushiswapPoolRowsSeedAttrsAndLiveCreationIsPersisted(t *testing.T) {
	const pool = "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3"
	const newPool = "CBQHNAXSI55GX2GN6D67GK7BHVPSLJUGZQEU7WKQU3X7PTUSXHMYZJRR"
	// Two valid contract ids stand in for the token pair.
	tok0, tok1 := sushiswap_v3.MainnetFactory, "CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2"
	store := &fakeSushiStore{
		fakeProtocolContractStore: fakeProtocolContractStore{rows: map[string][]string{}},
		pools:                     []timescale.SushiswapV3Pool{{PoolID: pool, FactoryID: sushiswap_v3.MainnetFactory, Token0: tok0, Token1: tok1}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	got, err := gatedRegistryOptions(context.Background(), store, logger, context.Background(), true)
	if err != nil {
		t.Fatalf("gatedRegistryOptions: %v", err)
	}
	opts := got[sushiswap_v3.SourceName]

	d := sushiswap_v3.NewDecoder(opts...)
	found := false
	for _, id := range d.GatedContractSet() {
		found = found || id == pool
	}
	if !found {
		t.Fatal("persisted pool row did not reach the decoder gate")
	}
	if n := len(store.written); n != 0 {
		t.Fatalf("warm wrote %d pool rows, want 0", n)
	}

	reg := contractid.New(opts...)
	if a := reg.AllAttrs()[pool]; a[sushiswap_v3.AttrToken0] != tok0 || a[sushiswap_v3.AttrToken1] != tok1 {
		t.Fatalf("seeded attrs = %v, want tokens %s / %s", a, tok0, tok1)
	}

	reg.SeedWithAttrs(newPool, sushiswap_v3.MainnetFactory, 77, contractid.Attrs{
		sushiswap_v3.AttrToken0: tok0, sushiswap_v3.AttrToken1: tok1,
		sushiswap_v3.AttrFeePips: "3000", sushiswap_v3.AttrTickSpacing: "60",
	})
	want := timescale.SushiswapV3Pool{
		PoolID: newPool, FactoryID: sushiswap_v3.MainnetFactory, Token0: tok0, Token1: tok1,
		FeePips: 3000, TickSpacing: 60, CreationLedger: 77,
	}
	if len(store.written) != 1 || store.written[0] != want {
		t.Fatalf("written = %+v, want exactly [%+v]", store.written, want)
	}
}
