package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"
)

// SeedIntraLedgerSeq is the intra_ledger_seq an ops seed writer
// (stellarindex-ops supply-seed / supply-seed-sac) stamps on every row it
// writes. It is math.MaxUint32 — the top of the per-ledger position space —
// because a seed row is the authoritative reconstructed FINAL state for its
// ledger (the latest entry from the ClickHouse lake, ADR-0034), so it must
// sit at the END of the intra-ledger order: a live per-ledger change (a much
// smaller position) can never overwrite it, while a re-seed (equal sentinel)
// stays corrective under the `<=` guard. The live dispatcher counter cannot
// reach this value (it would require 4.3e9 entry-changes in one ledger), so
// there is no live/seed ambiguity. See migration 0111 (audit-2026-07-16 C2-6).
const SeedIntraLedgerSeq = uint32(math.MaxUint32)

// TrustlineObservation is the wire shape for a single
// trustline-delta row. Mirrors trustline_observations columns.
type TrustlineObservation struct {
	AccountID  string
	AssetKey   string
	Ledger     uint32
	ObservedAt time.Time
	Balance    *big.Int
	IsRemoval  bool

	// IntraLedgerSeq is the within-ledger position of the change that
	// produced this row (audit-2026-07-16 C2-6). Guards the last-writer-wins
	// upsert so an out-of-order PersistEvents worker can't persist a stale
	// intra-ledger balance. Zero for the first change in a ledger; ops seeds
	// use SeedIntraLedgerSeq.
	IntraLedgerSeq uint32
}

// InsertTrustlineObservation appends one [TrustlineObservation]
// row, last-writer-wins on conflict. Same shape as
// [Store.InsertAccountObservation] from #299 — the AccountEntry
// post-state is monotonic within a ledger so the latest write is
// the authoritative final state.
//
// Defensive: rejects empty AccountID / AssetKey / nil Balance
// before touching the DB.
func (s *Store) InsertTrustlineObservation(ctx context.Context, o TrustlineObservation) error {
	if o.AccountID == "" {
		return errors.New("timescale: InsertTrustlineObservation: AccountID is empty")
	}
	if o.AssetKey == "" {
		return errors.New("timescale: InsertTrustlineObservation: AssetKey is empty")
	}
	if o.Balance == nil {
		return fmt.Errorf("timescale: InsertTrustlineObservation: Balance is nil (account=%s asset=%s)", o.AccountID, o.AssetKey)
	}
	// intra_ledger_seq-guarded upsert: a later intra-ledger change wins
	// regardless of parallel-worker commit order (audit-2026-07-16 C2-6).
	const q = `
        INSERT INTO trustline_observations (
            account_id, asset_key, ledger, observed_at,
            balance_stroops, is_removal, intra_ledger_seq
        ) VALUES (
            $1, $2, $3, $4,
            $5, $6, $7
        )
        ON CONFLICT (account_id, asset_key, ledger, observed_at) DO UPDATE SET
            balance_stroops  = EXCLUDED.balance_stroops,
            is_removal       = EXCLUDED.is_removal,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq
        WHERE trustline_observations.intra_ledger_seq <= EXCLUDED.intra_ledger_seq
    `
	_, err := s.db.ExecContext(ctx, q,
		o.AccountID, o.AssetKey, int(o.Ledger), o.ObservedAt.UTC(),
		o.Balance.String(), o.IsRemoval, int64(o.IntraLedgerSeq),
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertTrustlineObservation %s/%s@%d: %w", o.AccountID, o.AssetKey, o.Ledger, err)
	}
	return nil
}

// SumTrustlineBalancesAtOrBefore returns Σ trustline_balance for
// `assetKey` across every observed account at-or-before
// `asOfLedger`. Per-account row picked is the most-recent one
// for that (account, asset) pair; is_removal=true rows contribute
// 0.
//
// Returns a non-nil *big.Int (zero is a valid answer when the
// asset has no trustline observations yet) on success.
func (s *Store) SumTrustlineBalancesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT COALESCE(sum(balance_stroops), 0)::text
          FROM (
            SELECT DISTINCT ON (account_id)
                   balance_stroops, is_removal
              FROM trustline_observations
             WHERE asset_key = $1
               AND ledger    <= $2
             ORDER BY account_id, ledger DESC
          ) latest
         WHERE NOT is_removal
    `
	return scanSum(ctx, s.db, q, assetKey, int(asOfLedger))
}

// ClaimableObservation row.
type ClaimableObservation struct {
	ClaimableID string
	AssetKey    string
	Ledger      uint32
	ObservedAt  time.Time
	Balance     *big.Int
	IsRemoval   bool

	// IntraLedgerSeq — within-ledger change position; guards the upsert
	// (audit-2026-07-16 C2-6). See TrustlineObservation.IntraLedgerSeq.
	IntraLedgerSeq uint32
}

// InsertClaimableObservation — same shape as
// InsertTrustlineObservation, keyed on claimable_id.
func (s *Store) InsertClaimableObservation(ctx context.Context, o ClaimableObservation) error {
	if o.ClaimableID == "" {
		return errors.New("timescale: InsertClaimableObservation: ClaimableID is empty")
	}
	if o.AssetKey == "" {
		return errors.New("timescale: InsertClaimableObservation: AssetKey is empty")
	}
	if o.Balance == nil {
		return fmt.Errorf("timescale: InsertClaimableObservation: Balance is nil (cb=%s)", o.ClaimableID)
	}
	// intra_ledger_seq-guarded upsert (audit-2026-07-16 C2-6).
	const q = `
        INSERT INTO claimable_observations (
            claimable_id, asset_key, ledger, observed_at,
            balance_stroops, is_removal, intra_ledger_seq
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7
        )
        ON CONFLICT (claimable_id, ledger, observed_at) DO UPDATE SET
            balance_stroops  = EXCLUDED.balance_stroops,
            is_removal       = EXCLUDED.is_removal,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq
        WHERE claimable_observations.intra_ledger_seq <= EXCLUDED.intra_ledger_seq
    `
	_, err := s.db.ExecContext(ctx, q,
		o.ClaimableID, o.AssetKey, int(o.Ledger), o.ObservedAt.UTC(),
		o.Balance.String(), o.IsRemoval, int64(o.IntraLedgerSeq),
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertClaimableObservation %s@%d: %w", o.ClaimableID, o.Ledger, err)
	}
	return nil
}

// SumClaimableBalancesAtOrBefore — same shape as
// SumTrustlineBalancesAtOrBefore, keyed on claimable_id.
func (s *Store) SumClaimableBalancesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT COALESCE(sum(balance_stroops), 0)::text
          FROM (
            SELECT DISTINCT ON (claimable_id)
                   balance_stroops, is_removal
              FROM claimable_observations
             WHERE asset_key = $1
               AND ledger    <= $2
             ORDER BY claimable_id, ledger DESC
          ) latest
         WHERE NOT is_removal
    `
	return scanSum(ctx, s.db, q, assetKey, int(asOfLedger))
}

// LPReserveObservation row.
type LPReserveObservation struct {
	PoolID     string
	AssetKey   string
	Ledger     uint32
	ObservedAt time.Time
	Balance    *big.Int
	IsRemoval  bool

	// IntraLedgerSeq — within-ledger change position; guards the upsert
	// (audit-2026-07-16 C2-6). See TrustlineObservation.IntraLedgerSeq.
	IntraLedgerSeq uint32
}

// InsertLPReserveObservation — keyed on (pool_id, asset_key).
// One change to a pool produces TWO row writes (one per asset
// side); the observer in Task #65 emits both.
func (s *Store) InsertLPReserveObservation(ctx context.Context, o LPReserveObservation) error {
	if o.PoolID == "" {
		return errors.New("timescale: InsertLPReserveObservation: PoolID is empty")
	}
	if o.AssetKey == "" {
		return errors.New("timescale: InsertLPReserveObservation: AssetKey is empty")
	}
	if o.Balance == nil {
		return fmt.Errorf("timescale: InsertLPReserveObservation: Balance is nil (pool=%s asset=%s)", o.PoolID, o.AssetKey)
	}
	// intra_ledger_seq-guarded upsert (audit-2026-07-16 C2-6).
	const q = `
        INSERT INTO lp_reserve_observations (
            pool_id, asset_key, ledger, observed_at,
            balance_stroops, is_removal, intra_ledger_seq
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7
        )
        ON CONFLICT (pool_id, asset_key, ledger, observed_at) DO UPDATE SET
            balance_stroops  = EXCLUDED.balance_stroops,
            is_removal       = EXCLUDED.is_removal,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq
        WHERE lp_reserve_observations.intra_ledger_seq <= EXCLUDED.intra_ledger_seq
    `
	_, err := s.db.ExecContext(ctx, q,
		o.PoolID, o.AssetKey, int(o.Ledger), o.ObservedAt.UTC(),
		o.Balance.String(), o.IsRemoval, int64(o.IntraLedgerSeq),
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertLPReserveObservation %s/%s@%d: %w", o.PoolID, o.AssetKey, o.Ledger, err)
	}
	return nil
}

// SumLPReservesAtOrBefore — most-recent (pool_id, asset_key)
// observation, summed across pools.
func (s *Store) SumLPReservesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT COALESCE(sum(balance_stroops), 0)::text
          FROM (
            SELECT DISTINCT ON (pool_id)
                   balance_stroops, is_removal
              FROM lp_reserve_observations
             WHERE asset_key = $1
               AND ledger    <= $2
             ORDER BY pool_id, ledger DESC
          ) latest
         WHERE NOT is_removal
    `
	return scanSum(ctx, s.db, q, assetKey, int(asOfLedger))
}

// SACBalanceObservation row.
type SACBalanceObservation struct {
	ContractID string
	AssetKey   string
	Holder     string
	Ledger     uint32
	ObservedAt time.Time
	Balance    *big.Int
	IsRemoval  bool

	// IntraLedgerSeq — within-ledger change position; guards the upsert so
	// when a single (contract, holder) changes multiple times in one ledger
	// the FINAL change wins rather than whichever out-of-order worker
	// committed last (audit-2026-07-16 C2-6). See
	// TrustlineObservation.IntraLedgerSeq.
	IntraLedgerSeq uint32
}

// InsertSACBalanceObservation — keyed on (contract_id, holder).
// Asset_key is the operator-supplied SAC → asset mapping
// stamped at decode time.
func (s *Store) InsertSACBalanceObservation(ctx context.Context, o SACBalanceObservation) error {
	if o.ContractID == "" {
		return errors.New("timescale: InsertSACBalanceObservation: ContractID is empty")
	}
	if o.AssetKey == "" {
		return errors.New("timescale: InsertSACBalanceObservation: AssetKey is empty")
	}
	if o.Holder == "" {
		return errors.New("timescale: InsertSACBalanceObservation: Holder is empty")
	}
	if o.Balance == nil {
		return fmt.Errorf("timescale: InsertSACBalanceObservation: Balance is nil (contract=%s holder=%s)", o.ContractID, o.Holder)
	}
	// intra_ledger_seq-guarded upsert: the FINAL intra-ledger change to this
	// (contract, holder) wins regardless of parallel-worker commit order —
	// the wrong-supply-component fix (audit-2026-07-16 C2-6).
	const q = `
        INSERT INTO sac_balance_observations (
            contract_id, asset_key, holder, ledger, observed_at,
            balance_stroops, is_removal, intra_ledger_seq
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8
        )
        ON CONFLICT (contract_id, holder, ledger, observed_at) DO UPDATE SET
            asset_key        = EXCLUDED.asset_key,
            balance_stroops  = EXCLUDED.balance_stroops,
            is_removal       = EXCLUDED.is_removal,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq
        WHERE sac_balance_observations.intra_ledger_seq <= EXCLUDED.intra_ledger_seq
    `
	_, err := s.db.ExecContext(ctx, q,
		o.ContractID, o.AssetKey, o.Holder, int(o.Ledger), o.ObservedAt.UTC(),
		o.Balance.String(), o.IsRemoval, int64(o.IntraLedgerSeq),
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertSACBalanceObservation %s/%s@%d: %w", o.ContractID, o.Holder, o.Ledger, err)
	}
	return nil
}

// SumSACBalancesAtOrBefore — most-recent (contract_id, holder)
// observation, summed across holders. Keyed by asset_key so the
// reader can include the SAC component for the watched classic
// asset directly.
func (s *Store) SumSACBalancesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT COALESCE(sum(balance_stroops), 0)::text
          FROM (
            SELECT DISTINCT ON (contract_id, holder)
                   balance_stroops, is_removal
              FROM sac_balance_observations
             WHERE asset_key = $1
               AND ledger    <= $2
             ORDER BY contract_id, holder, ledger DESC
          ) latest
         WHERE NOT is_removal
    `
	return scanSum(ctx, s.db, q, assetKey, int(asOfLedger))
}

// TrustlineBalanceForAccountAtOrBefore returns the most-recent
// trustline balance for the (account, asset) pair at-or-before
// the supplied ledger. Returns zero (non-nil) when the account
// has no trustline observation in scope OR when the latest
// observation is a removal.
//
// Used by the classic-supply reader to compute IssuerBalance
// (asset issuer's holding of their own asset) and
// LockedAccountBalances (sum across operator-configured
// locked-set accounts).
func (s *Store) TrustlineBalanceForAccountAtOrBefore(ctx context.Context, accountID, assetKey string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT balance_stroops::text, is_removal
          FROM trustline_observations
         WHERE account_id = $1
           AND asset_key  = $2
           AND ledger    <= $3
         ORDER BY ledger DESC
         LIMIT 1
    `
	return scanLatestBalance(ctx, s.db, q, accountID, assetKey, int(asOfLedger))
}

// SACBalanceForContractAtOrBefore returns the most-recent SAC
// balance for the (holder, asset) pair at-or-before the supplied
// ledger. Returns zero on no-observation OR removal — same
// semantics as TrustlineBalanceForAccountAtOrBefore.
//
// Used by the classic-supply reader to compute
// LockedContractBalances (sum across operator-configured
// locked-set contracts holding the SAC).
func (s *Store) SACBalanceForContractAtOrBefore(ctx context.Context, contractHolder, assetKey string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT balance_stroops::text, is_removal
          FROM sac_balance_observations
         WHERE holder    = $1
           AND asset_key = $2
           AND ledger   <= $3
         ORDER BY ledger DESC
         LIMIT 1
    `
	return scanLatestBalance(ctx, s.db, q, contractHolder, assetKey, int(asOfLedger))
}

// scanLatestBalance is the shared helper for the per-(holder,
// asset) lookup methods. Returns zero on sql.ErrNoRows or
// is_removal=true; non-nil *big.Int otherwise.
func scanLatestBalance(ctx context.Context, db *sql.DB, q string, args ...any) (*big.Int, error) {
	var (
		raw       string
		isRemoval bool
	)
	err := db.QueryRowContext(ctx, q, args...).Scan(&raw, &isRemoval)
	if errors.Is(err, sql.ErrNoRows) {
		return big.NewInt(0), nil
	}
	if err != nil {
		return nil, fmt.Errorf("timescale: scanLatestBalance: %w", err)
	}
	if isRemoval {
		return big.NewInt(0), nil
	}
	v, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, fmt.Errorf("timescale: scanLatestBalance: parse %q", raw)
	}
	return v, nil
}

// scanSum runs `q` with the supplied positional args, expecting
// a single TEXT-cast NUMERIC result, and returns it as *big.Int.
// Shared helper for the four Sum* methods above.
func scanSum(ctx context.Context, db *sql.DB, q string, args ...any) (*big.Int, error) {
	var raw string
	if err := db.QueryRowContext(ctx, q, args...).Scan(&raw); err != nil {
		return nil, fmt.Errorf("timescale: scanSum: %w", err)
	}
	v, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, fmt.Errorf("timescale: scanSum: parse %q", raw)
	}
	return v, nil
}

// MinClassicComponentLedger returns the lowest "most-recent
// observation ledger" across the four classic-supply component
// tables for `assetKey`, scoped to ledgers at-or-before
// `asOfLedger`. Used by the supply Refresher to detect snapshots
// whose component observations lag the snapshot ledger by more
// than a threshold. F-1236 (codex audit-2026-05-12).
//
// Each component contributes MAX(ledger) of any row matching
// (asset_key, ledger <= asOfLedger). The function returns the
// MIN of those four maxima — the slowest observer.
//
// A component that has NO observations for the asset returns
// 0 from its MAX. Treated as "no signal" — excluded from the
// MIN. A zero result means no component has any observation
// (genuinely uninstrumented asset); caller treats that as
// "skip the freshness gate" via the documented zero-means-no-
// signal contract on Supply.MinComponentLedger.
func (s *Store) MinClassicComponentLedger(ctx context.Context, assetKey string, asOfLedger uint32) (uint32, error) {
	// Per-component MAX(ledger). Empty tables → 0. NULLIF +
	// MIN over a CTE filters out the "no observations yet"
	// components so a brand-new asset that's only been observed
	// in trustlines doesn't have its MinComponentLedger pinned
	// to 0 by the empty claimable/LP/SAC tables.
	const q = `
		WITH per_component AS (
		    SELECT NULLIF(COALESCE(MAX(ledger), 0), 0) AS l
		      FROM trustline_observations
		     WHERE asset_key = $1 AND ledger <= $2
		    UNION ALL
		    SELECT NULLIF(COALESCE(MAX(ledger), 0), 0) AS l
		      FROM claimable_observations
		     WHERE asset_key = $1 AND ledger <= $2
		    UNION ALL
		    SELECT NULLIF(COALESCE(MAX(ledger), 0), 0) AS l
		      FROM lp_reserve_observations
		     WHERE asset_key = $1 AND ledger <= $2
		    UNION ALL
		    SELECT NULLIF(COALESCE(MAX(ledger), 0), 0) AS l
		      FROM sac_balance_observations
		     WHERE asset_key = $1 AND ledger <= $2
		)
		SELECT COALESCE(MIN(l), 0) FROM per_component WHERE l IS NOT NULL
	`
	var minLedger uint32
	if err := s.db.QueryRowContext(ctx, q, assetKey, int(asOfLedger)).Scan(&minLedger); err != nil {
		return 0, fmt.Errorf("timescale: MinClassicComponentLedger: %w", err)
	}
	return minLedger, nil
}
