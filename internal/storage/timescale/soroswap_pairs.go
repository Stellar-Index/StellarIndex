package timescale

import (
	"context"
	"errors"
	"fmt"
)

// SoroswapPair is the read-side projection of one soroswap_pairs row.
type SoroswapPair struct {
	PairStrkey   string
	Token0Strkey string
	Token1Strkey string
}

// UpsertSoroswapPair inserts or refreshes a (pair, token0, token1) mapping. observed_at is bumped to now() on every call.
func (s *Store) UpsertSoroswapPair(ctx context.Context, pairStrkey, token0Strkey, token1Strkey string) error {
	if pairStrkey == "" {
		return errors.New("timescale: UpsertSoroswapPair: empty pair_strkey")
	}
	if token0Strkey == "" || token1Strkey == "" {
		return fmt.Errorf("timescale: UpsertSoroswapPair %s: token0 or token1 empty", pairStrkey)
	}
	const q = `
		INSERT INTO soroswap_pairs (pair_strkey, token0_strkey, token1_strkey, observed_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (pair_strkey) DO UPDATE SET
		    token0_strkey = EXCLUDED.token0_strkey,
		    token1_strkey = EXCLUDED.token1_strkey,
		    observed_at   = EXCLUDED.observed_at
	`
	if _, err := s.db.ExecContext(ctx, q, pairStrkey, token0Strkey, token1Strkey); err != nil {
		return fmt.Errorf("timescale: UpsertSoroswapPair %s: %w", pairStrkey, err)
	}
	return nil
}

// InsertSoroswapPairIfAbsent writes a (pair, token0, token1) mapping only when pair_strkey has no row, reporting whether it inserted; it never overwrites on-chain new_pair rows (UpsertSoroswapPair) with the off-chain bootstrap.
func (s *Store) InsertSoroswapPairIfAbsent(ctx context.Context, pairStrkey, token0Strkey, token1Strkey string) (bool, error) {
	if pairStrkey == "" {
		return false, errors.New("timescale: InsertSoroswapPairIfAbsent: empty pair_strkey")
	}
	if token0Strkey == "" || token1Strkey == "" {
		return false, fmt.Errorf("timescale: InsertSoroswapPairIfAbsent %s: token0 or token1 empty", pairStrkey)
	}
	const q = `
		INSERT INTO soroswap_pairs (pair_strkey, token0_strkey, token1_strkey, observed_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (pair_strkey) DO NOTHING
	`
	res, err := s.db.ExecContext(ctx, q, pairStrkey, token0Strkey, token1Strkey)
	if err != nil {
		return false, fmt.Errorf("timescale: InsertSoroswapPairIfAbsent %s: %w", pairStrkey, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("timescale: InsertSoroswapPairIfAbsent %s rows affected: %w", pairStrkey, err)
	}
	return n == 1, nil
}

// LoadSoroswapPairRegistry returns every soroswap_pairs row as a flat slice to seed the decoder's in-memory pair registry at startup; an empty slice (not an error) for a fresh deployment.
func (s *Store) LoadSoroswapPairRegistry(ctx context.Context) ([]SoroswapPair, error) {
	const q = `
		SELECT pair_strkey, token0_strkey, token1_strkey
		  FROM soroswap_pairs
	`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: LoadSoroswapPairRegistry: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]SoroswapPair, 0, 256)
	for rows.Next() {
		var p SoroswapPair
		if err := rows.Scan(&p.PairStrkey, &p.Token0Strkey, &p.Token1Strkey); err != nil {
			return nil, fmt.Errorf("timescale: LoadSoroswapPairRegistry scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LoadSoroswapPairRegistry rows: %w", err)
	}
	return out, nil
}
