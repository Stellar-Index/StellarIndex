package pipeline

import (
	"context"

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
