package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SACBalanceSeedSource discriminates which ClickHouse reader produced a
// [SACBalanceSeedProvenance] row (migration 0102). See
// docs/architecture/supply-pipeline.md "Dormant contract-held SAC
// balances" for the full incident 2026-07-06 background.
type SACBalanceSeedSource string

const (
	// SACBalanceSeedSourceCurrentState reads stellar.ledger_entries_current
	// (clickhouse.StreamSACBalanceSeeds) — fast, but has a coverage floor
	// (~ledger 62,000,000): the current-state materialized view only
	// reflects ledger_entry_changes rows inserted after the MV was
	// created, so a Balance(Address) entry dormant since before that
	// floor is invisible to it.
	SACBalanceSeedSourceCurrentState SACBalanceSeedSource = "current_state"

	// SACBalanceSeedSourceFullHistory reads stellar.ledger_entry_changes
	// directly (clickhouse.StreamSACBalanceSeedsFullHistory) — the
	// certified append-log, complete to genesis (ADR-0034). Closes the
	// current-state floor gap; heavier, run-heavy-job.sh only.
	SACBalanceSeedSourceFullHistory SACBalanceSeedSource = "full_history"
)

// SACBalanceSeedProvenance is one `supply seed-sac-balances` pass's
// audit record for a single watched SAC-wrapper contract.
type SACBalanceSeedProvenance struct {
	ContractID    string
	AssetKey      string
	Source        SACBalanceSeedSource
	HoldersSeeded int
	MinLedgerSeen *uint32 // nil when HoldersSeeded == 0
	MaxLedgerSeen *uint32 // nil when HoldersSeeded == 0
	// LakeVerifiedThrough is the ledger through which the lake was proven
	// intact before a full_history pass emitted anything (migration 0182).
	// Required for full_history: the source label alone is not evidence.
	LakeVerifiedThrough *uint32
	// HoldersRetracted counts the removed / TTL-archived holders written as
	// tombstones; nil only on rows read back from before migration 0182.
	HoldersRetracted *int
	SeededAt         time.Time
}

// UpsertSACBalanceSeedProvenance records (or overwrites) the most recent
// seed pass for a contract. Idempotent — a re-run of `supply
// seed-sac-balances` for the same contract simply replaces the prior
// row's stats + timestamp, matching sep41_supply_rollup's genesis-seed
// upsert pattern (migration 0088). Pure audit trail: never read by the
// supply computation (ClassicSupplyAt / SumSACBalancesAtOrBefore), only
// by operator/ops reporting.
func (s *Store) UpsertSACBalanceSeedProvenance(ctx context.Context, p SACBalanceSeedProvenance) error {
	if p.ContractID == "" {
		return errors.New("timescale: UpsertSACBalanceSeedProvenance: empty ContractID")
	}
	if p.AssetKey == "" {
		return fmt.Errorf("timescale: UpsertSACBalanceSeedProvenance %s: empty AssetKey", p.ContractID)
	}
	if p.Source != SACBalanceSeedSourceCurrentState && p.Source != SACBalanceSeedSourceFullHistory {
		return fmt.Errorf("timescale: UpsertSACBalanceSeedProvenance %s: invalid source %q", p.ContractID, p.Source)
	}
	if p.HoldersSeeded < 0 {
		return fmt.Errorf("timescale: UpsertSACBalanceSeedProvenance %s: negative HoldersSeeded %d", p.ContractID, p.HoldersSeeded)
	}
	if p.Source == SACBalanceSeedSourceFullHistory && p.LakeVerifiedThrough == nil {
		return fmt.Errorf("timescale: UpsertSACBalanceSeedProvenance %s: full_history without LakeVerifiedThrough — the pass did not prove the lake intact", p.ContractID)
	}
	if p.HoldersRetracted != nil && *p.HoldersRetracted < 0 {
		return fmt.Errorf("timescale: UpsertSACBalanceSeedProvenance %s: negative HoldersRetracted %d", p.ContractID, *p.HoldersRetracted)
	}

	const q = `
        INSERT INTO sac_balance_seed_provenance (
            contract_id, asset_key, source, holders_seeded,
            min_ledger_seen, max_ledger_seen,
            lake_verified_through, holders_retracted, seeded_at
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, now()
        )
        ON CONFLICT (contract_id) DO UPDATE SET
            asset_key       = EXCLUDED.asset_key,
            source          = EXCLUDED.source,
            holders_seeded  = EXCLUDED.holders_seeded,
            min_ledger_seen = EXCLUDED.min_ledger_seen,
            max_ledger_seen = EXCLUDED.max_ledger_seen,
            lake_verified_through = EXCLUDED.lake_verified_through,
            holders_retracted     = EXCLUDED.holders_retracted,
            seeded_at       = now()
    `
	var minLedger, maxLedger sql.NullInt64
	if p.MinLedgerSeen != nil {
		minLedger = sql.NullInt64{Int64: int64(*p.MinLedgerSeen), Valid: true}
	}
	if p.MaxLedgerSeen != nil {
		maxLedger = sql.NullInt64{Int64: int64(*p.MaxLedgerSeen), Valid: true}
	}
	var verified, retracted sql.NullInt64
	if p.LakeVerifiedThrough != nil {
		verified = sql.NullInt64{Int64: int64(*p.LakeVerifiedThrough), Valid: true}
	}
	if p.HoldersRetracted != nil {
		retracted = sql.NullInt64{Int64: int64(*p.HoldersRetracted), Valid: true}
	}
	if _, err := s.db.ExecContext(ctx, q,
		p.ContractID, p.AssetKey, string(p.Source), p.HoldersSeeded, minLedger, maxLedger,
		verified, retracted,
	); err != nil {
		return fmt.Errorf("timescale: UpsertSACBalanceSeedProvenance %s: %w", p.ContractID, err)
	}
	return nil
}

// SACBalanceSeedProvenanceFor reads the most recent seed-pass record for
// one contract. Returns (zero value, false, nil) when the contract has
// never been seeded — distinct from an error, since "never seeded" is
// the expected state for a freshly-added [supply.sac_wrappers] entry.
func (s *Store) SACBalanceSeedProvenanceFor(ctx context.Context, contractID string) (SACBalanceSeedProvenance, bool, error) {
	if contractID == "" {
		return SACBalanceSeedProvenance{}, false, errors.New("timescale: SACBalanceSeedProvenanceFor: empty contractID")
	}
	const q = `
        SELECT contract_id, asset_key, source, holders_seeded,
               min_ledger_seen, max_ledger_seen,
               lake_verified_through, holders_retracted, seeded_at
          FROM sac_balance_seed_provenance
         WHERE contract_id = $1
    `
	var (
		p                    SACBalanceSeedProvenance
		source               string
		minLedger, maxLedger sql.NullInt64
		verified             sql.Null[uint32]
		retracted            sql.Null[int]
	)
	err := s.db.QueryRowContext(ctx, q, contractID).Scan(
		&p.ContractID, &p.AssetKey, &source, &p.HoldersSeeded,
		&minLedger, &maxLedger, &verified, &retracted, &p.SeededAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SACBalanceSeedProvenance{}, false, nil
	}
	if err != nil {
		return SACBalanceSeedProvenance{}, false, fmt.Errorf("timescale: SACBalanceSeedProvenanceFor %s: %w", contractID, err)
	}
	p.Source = SACBalanceSeedSource(source)
	if minLedger.Valid {
		v := uint32(minLedger.Int64) //nolint:gosec // ledger seq fits uint32
		p.MinLedgerSeen = &v
	}
	if maxLedger.Valid {
		v := uint32(maxLedger.Int64) //nolint:gosec // ledger seq fits uint32
		p.MaxLedgerSeen = &v
	}
	if verified.Valid {
		p.LakeVerifiedThrough = &verified.V
	}
	if retracted.Valid {
		p.HoldersRetracted = &retracted.V
	}
	p.SeededAt = p.SeededAt.UTC()
	return p, true, nil
}
