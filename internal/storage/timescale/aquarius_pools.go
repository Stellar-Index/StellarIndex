package timescale

import (
	"context"
	"errors"
	"fmt"
)

// AquariusPool is one aquarius_pools row: the pool type its router add_pool announced, verbatim.
type AquariusPool struct {
	ContractID  string
	PoolType    string
	FirstLedger uint32
}

// ErrAquariusPoolTypeConflict is returned when a pool is re-recorded with a type other than the one stored.
var ErrAquariusPoolTypeConflict = errors.New("timescale: aquarius pool re-announced with a different pool type")

// UpsertAquariusPool records a pool's announced type; re-recording the same type is a no-op and a different type is
// refused with [ErrAquariusPoolTypeConflict], leaving the row as first written.
func (s *Store) UpsertAquariusPool(ctx context.Context, p AquariusPool) error {
	if p.ContractID == "" {
		return errors.New("timescale: UpsertAquariusPool: empty contract id")
	}
	const q = `
		INSERT INTO aquarius_pools (contract_id, pool_type, first_ledger, observed_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (contract_id) DO UPDATE SET pool_type = aquarius_pools.pool_type
		WHERE aquarius_pools.pool_type = EXCLUDED.pool_type
	`
	res, err := s.db.ExecContext(ctx, q, p.ContractID, p.PoolType, int64(p.FirstLedger))
	if err != nil {
		return fmt.Errorf("timescale: UpsertAquariusPool %s: %w", p.ContractID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("timescale: UpsertAquariusPool %s: %w", p.ContractID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s as %q", ErrAquariusPoolTypeConflict, p.ContractID, p.PoolType)
	}
	return nil
}

// LoadAquariusPools returns every recorded pool, used at boot to restore the decoder's liquidity layouts.
func (s *Store) LoadAquariusPools(ctx context.Context) ([]AquariusPool, error) {
	const q = `SELECT contract_id, pool_type, first_ledger FROM aquarius_pools`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: LoadAquariusPools: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]AquariusPool, 0, 64)
	for rows.Next() {
		var p AquariusPool
		var ledger int64
		if err := rows.Scan(&p.ContractID, &p.PoolType, &ledger); err != nil {
			return nil, fmt.Errorf("timescale: LoadAquariusPools scan: %w", err)
		}
		p.FirstLedger = uint32(ledger)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LoadAquariusPools rows: %w", err)
	}
	return out, nil
}
