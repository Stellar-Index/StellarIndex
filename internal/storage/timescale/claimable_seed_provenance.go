package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ClaimableSeedProvenance is one complete `supply seed-claimable-balances`
// pass's audit record for a single classic asset (migration 0183).
type ClaimableSeedProvenance struct {
	AssetKey            string
	ClaimablesSeeded    int
	ClaimablesRetracted int
	MinLedgerSeen       *uint32 // nil when ClaimablesSeeded == 0
	MaxLedgerSeen       *uint32 // nil when ClaimablesSeeded == 0
	LakeVerifiedThrough uint32
	SeededAt            time.Time
}

// UpsertClaimableSeedProvenance records (or overwrites) the most recent
// complete seed pass for an asset. A row without lake verification is refused:
// it would claim a history seed the walk never proved.
func (s *Store) UpsertClaimableSeedProvenance(ctx context.Context, p ClaimableSeedProvenance) error {
	if p.AssetKey == "" {
		return errors.New("timescale: UpsertClaimableSeedProvenance: empty AssetKey")
	}
	if p.ClaimablesSeeded < 0 || p.ClaimablesRetracted < 0 {
		return fmt.Errorf("timescale: UpsertClaimableSeedProvenance %s: negative count (seeded %d, retracted %d)", p.AssetKey, p.ClaimablesSeeded, p.ClaimablesRetracted)
	}
	if p.LakeVerifiedThrough == 0 {
		return fmt.Errorf("timescale: UpsertClaimableSeedProvenance %s: no LakeVerifiedThrough — the pass did not prove the lake intact", p.AssetKey)
	}
	const q = `
        INSERT INTO claimable_seed_provenance (
            asset_key, claimables_seeded, claimables_retracted,
            min_ledger_seen, max_ledger_seen, lake_verified_through, seeded_at
        ) VALUES ($1, $2, $3, $4, $5, $6, now())
        ON CONFLICT (asset_key) DO UPDATE SET
            claimables_seeded     = EXCLUDED.claimables_seeded,
            claimables_retracted  = EXCLUDED.claimables_retracted,
            min_ledger_seen       = EXCLUDED.min_ledger_seen,
            max_ledger_seen       = EXCLUDED.max_ledger_seen,
            lake_verified_through = EXCLUDED.lake_verified_through,
            seeded_at             = now()
    `
	var minLedger, maxLedger sql.Null[int64]
	if p.MinLedgerSeen != nil {
		minLedger = sql.Null[int64]{V: int64(*p.MinLedgerSeen), Valid: true}
	}
	if p.MaxLedgerSeen != nil {
		maxLedger = sql.Null[int64]{V: int64(*p.MaxLedgerSeen), Valid: true}
	}
	if _, err := s.db.ExecContext(ctx, q, p.AssetKey, p.ClaimablesSeeded, p.ClaimablesRetracted,
		minLedger, maxLedger, int64(p.LakeVerifiedThrough)); err != nil {
		return fmt.Errorf("timescale: UpsertClaimableSeedProvenance %s: %w", p.AssetKey, err)
	}
	return nil
}

// ClaimableSeedProvenanceFor returns the asset's provenance row; ok=false when
// no complete pass has stamped it.
func (s *Store) ClaimableSeedProvenanceFor(ctx context.Context, assetKey string) (ClaimableSeedProvenance, bool, error) {
	const q = `
        SELECT asset_key, claimables_seeded, claimables_retracted,
               min_ledger_seen, max_ledger_seen, lake_verified_through, seeded_at
          FROM claimable_seed_provenance
         WHERE asset_key = $1
    `
	var (
		p                    ClaimableSeedProvenance
		minLedger, maxLedger sql.Null[uint32]
	)
	err := s.db.QueryRowContext(ctx, q, assetKey).Scan(&p.AssetKey, &p.ClaimablesSeeded, &p.ClaimablesRetracted,
		&minLedger, &maxLedger, &p.LakeVerifiedThrough, &p.SeededAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ClaimableSeedProvenance{}, false, nil
	}
	if err != nil {
		return ClaimableSeedProvenance{}, false, fmt.Errorf("timescale: ClaimableSeedProvenanceFor %s: %w", assetKey, err)
	}
	if minLedger.Valid {
		p.MinLedgerSeen = &minLedger.V
	}
	if maxLedger.Valid {
		p.MaxLedgerSeen = &maxLedger.V
	}
	p.SeededAt = p.SeededAt.UTC()
	return p, true, nil
}
