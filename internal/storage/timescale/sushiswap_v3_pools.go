package timescale

import (
	"context"
	"errors"
	"fmt"
)

// SushiswapV3Pool is one sushiswap_v3_pools row. Token fields are C-strkeys;
// canonical.Asset reconstruction stays with the caller.
type SushiswapV3Pool struct {
	PoolID         string
	FactoryID      string
	Token0         string
	Token1         string
	FeePips        int32
	TickSpacing    int32
	CreationLedger uint32
}

// UpsertSushiswapV3Pool records (or refreshes) a pool created by the factory.
// Idempotent on pool_id: the decoder calls it on every observed pool_created.
func (s *Store) UpsertSushiswapV3Pool(ctx context.Context, p SushiswapV3Pool) error {
	if p.PoolID == "" || p.FactoryID == "" || p.Token0 == "" || p.Token1 == "" {
		return errors.New("timescale: UpsertSushiswapV3Pool: empty pool, factory or token")
	}
	const q = `
		INSERT INTO sushiswap_v3_pools
		    (pool_id, factory_id, token0, token1, fee_pips, tick_spacing, creation_ledger, observed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (pool_id) DO UPDATE SET
		    factory_id      = EXCLUDED.factory_id,
		    token0          = EXCLUDED.token0,
		    token1          = EXCLUDED.token1,
		    fee_pips        = EXCLUDED.fee_pips,
		    tick_spacing    = EXCLUDED.tick_spacing,
		    creation_ledger = EXCLUDED.creation_ledger,
		    observed_at     = EXCLUDED.observed_at
	`
	if _, err := s.db.ExecContext(ctx, q, p.PoolID, p.FactoryID, p.Token0, p.Token1,
		p.FeePips, p.TickSpacing, int64(p.CreationLedger)); err != nil {
		return fmt.Errorf("timescale: UpsertSushiswapV3Pool %s: %w", p.PoolID, err)
	}
	return nil
}

// LoadSushiswapV3Pools returns every persisted pool, used at boot to seed the
// decoder's token map.
func (s *Store) LoadSushiswapV3Pools(ctx context.Context) ([]SushiswapV3Pool, error) {
	const q = `
		SELECT pool_id, factory_id, token0, token1, fee_pips, tick_spacing, creation_ledger
		  FROM sushiswap_v3_pools
	`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: LoadSushiswapV3Pools: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]SushiswapV3Pool, 0, 64)
	for rows.Next() {
		var p SushiswapV3Pool
		var ledger int64
		if err := rows.Scan(&p.PoolID, &p.FactoryID, &p.Token0, &p.Token1, &p.FeePips, &p.TickSpacing, &ledger); err != nil {
			return nil, fmt.Errorf("timescale: LoadSushiswapV3Pools scan: %w", err)
		}
		p.CreationLedger = uint32(ledger)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LoadSushiswapV3Pools rows: %w", err)
	}
	return out, nil
}
