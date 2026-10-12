package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// SeedIntraLedgerSeq is the intra_ledger_seq a seed writer stamps (math.MaxUint32) so the seed row, the authoritative final state for its ledger, sorts last: a live change never overwrites it and a re-seed does.
const SeedIntraLedgerSeq = uint32(math.MaxUint32)

// observationWalkVersion is stamped as walk_version on each row (migration 0199): the upsert guard compares intra_ledger_seq only within one
// walk.
const observationWalkVersion = int64(dispatcher.EntryWalkVersion)

// TrustlineObservation is the wire shape for a single trustline-delta row.
type TrustlineObservation struct {
	AccountID  string
	AssetKey   string
	Ledger     uint32
	ObservedAt time.Time
	Balance    *big.Int
	IsRemoval  bool

	// IntraLedgerSeq is the within-ledger position of the change that produced this row. It guards
	// the last-writer-wins upsert so an out-of-order PersistEvents worker cannot persist a stale
	// balance. Zero for the first change in a ledger; ops seeds use SeedIntraLedgerSeq.
	IntraLedgerSeq uint32
}

// InsertTrustlineObservation appends one [TrustlineObservation], last-writer-wins on conflict as in [Store.InsertAccountObservation]; rejects empty AccountID/AssetKey or nil Balance.
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
	// regardless of parallel-worker commit order.
	const q = `
        INSERT INTO trustline_observations (
            account_id, asset_key, ledger, observed_at,
            balance_stroops, is_removal, intra_ledger_seq, walk_version
        ) VALUES (
            $1, $2, $3, $4,
            $5, $6, $7, $8
        )
        ON CONFLICT (account_id, asset_key, ledger, observed_at) DO UPDATE SET
            balance_stroops  = EXCLUDED.balance_stroops,
            is_removal       = EXCLUDED.is_removal,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq,
            walk_version     = EXCLUDED.walk_version
        WHERE (trustline_observations.walk_version, trustline_observations.intra_ledger_seq)
           <= (EXCLUDED.walk_version, EXCLUDED.intra_ledger_seq)
    `
	_, err := s.db.ExecContext(ctx, q,
		o.AccountID, o.AssetKey, int(o.Ledger), o.ObservedAt.UTC(),
		o.Balance.String(), o.IsRemoval, int64(o.IntraLedgerSeq), observationWalkVersion,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertTrustlineObservation %s/%s@%d: %w", o.AccountID, o.AssetKey, o.Ledger, err)
	}
	return nil
}

// SumTrustlineBalancesAtOrBefore returns the sum of trustline_balance for `assetKey` over the latest row per account at or before `asOfLedger` (removals contribute 0), as a non-nil *big.Int, read from trustline_balance_current (0215) and seeded on first use; unbounded-latest-ok: a holder whose last row predates any floor still holds that balance at asOfLedger.
func (s *Store) SumTrustlineBalancesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error) {
	if v, err := s.sumTrustlineCurrent(ctx, assetKey, asOfLedger); err != nil || v != nil {
		return v, err
	}
	if err := s.seedTrustlineCurrent(ctx, assetKey); err != nil {
		return nil, err
	}
	if v, err := s.sumTrustlineCurrent(ctx, assetKey, asOfLedger); err != nil || v != nil {
		return v, err
	}
	return s.sumTrustlineFullScan(ctx, assetKey, asOfLedger)
}

// trustlineLatestOrder is the per-holder pick order shared by the full scan, the seed and the
// 0215 trigger; they must agree or the current table drifts from the scan.
const trustlineLatestOrder = `ledger DESC, walk_version DESC, intra_ledger_seq DESC, observed_at DESC`

// sumTrustlineCurrent returns nil when the asset is not seeded. One statement, so the seeded check,
// the current rows and the rewind of holders newer than asOfLedger share a snapshot.
func (s *Store) sumTrustlineCurrent(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error) {
	q := `
        WITH cur AS MATERIALIZED (
            SELECT account_id, ledger, balance_stroops, is_removal
              FROM trustline_balance_current
             WHERE asset_key = $1
        )
        SELECT CASE WHEN EXISTS (SELECT 1 FROM trustline_balance_current_seeded WHERE asset_key = $1) THEN (
            COALESCE((SELECT sum(balance_stroops) FROM cur WHERE ledger <= $2 AND NOT is_removal), 0)
          + COALESCE((SELECT sum(p.balance_stroops)
                        FROM cur c
                        CROSS JOIN LATERAL (
                            SELECT t.balance_stroops, t.is_removal
                              FROM trustline_observations t
                             WHERE t.asset_key = $1 AND t.account_id = c.account_id AND t.ledger <= $2
                             ORDER BY ` + trustlineLatestOrder + `
                             LIMIT 1
                        ) p
                       WHERE c.ledger > $2 AND NOT p.is_removal), 0)
        )::text END
    `
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, q, assetKey, int(asOfLedger)).Scan(&raw); err != nil {
		return nil, fmt.Errorf("timescale: sumTrustlineCurrent %s: %w", assetKey, err)
	}
	if !raw.Valid {
		return nil, nil
	}
	v, ok := new(big.Int).SetString(raw.String, 10)
	if !ok {
		return nil, fmt.Errorf("timescale: sumTrustlineCurrent %s: parse %q", assetKey, raw.String)
	}
	return v, nil
}

// seedTrustlineCurrent folds the asset's history into trustline_balance_current and marks it seeded, in one transaction under the asset's
// advisory lock (the one 0215's invalidation takes). Strict > so a stale snapshot never overwrites an equal-key row the trigger rewrote.
// unbounded-latest-ok: every holder's latest row, however old, is part of the current balance.
func (s *Store) seedTrustlineCurrent(ctx context.Context, assetKey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: seedTrustlineCurrent %s: begin: %w", assetKey, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('trustline_balance_current:' || $1, 0))`, assetKey); err != nil {
		return fmt.Errorf("timescale: seedTrustlineCurrent %s: lock: %w", assetKey, err)
	}
	seed := `
        INSERT INTO trustline_balance_current AS c (
            asset_key, account_id, ledger, observed_at,
            walk_version, intra_ledger_seq, balance_stroops, is_removal
        )
        SELECT DISTINCT ON (account_id)
               asset_key, account_id, ledger, observed_at,
               walk_version, intra_ledger_seq, balance_stroops, is_removal
          FROM trustline_observations
         WHERE asset_key = $1
         ORDER BY account_id, ` + trustlineLatestOrder + `
        ON CONFLICT (asset_key, account_id) DO UPDATE SET
            ledger           = EXCLUDED.ledger,
            observed_at      = EXCLUDED.observed_at,
            walk_version     = EXCLUDED.walk_version,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq,
            balance_stroops  = EXCLUDED.balance_stroops,
            is_removal       = EXCLUDED.is_removal
        WHERE (c.ledger, c.walk_version, c.intra_ledger_seq, c.observed_at)
            < (EXCLUDED.ledger, EXCLUDED.walk_version, EXCLUDED.intra_ledger_seq, EXCLUDED.observed_at)
    `
	if _, err := tx.ExecContext(ctx, seed, assetKey); err != nil {
		return fmt.Errorf("timescale: seedTrustlineCurrent %s: fold: %w", assetKey, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO trustline_balance_current_seeded (asset_key) VALUES ($1) ON CONFLICT DO NOTHING`, assetKey); err != nil {
		return fmt.Errorf("timescale: seedTrustlineCurrent %s: mark: %w", assetKey, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: seedTrustlineCurrent %s: commit: %w", assetKey, err)
	}
	return nil
}

// sumTrustlineFullScan is the history scan the current table replaces.
// unbounded-latest-ok: a holder whose last row predates any floor still holds that balance at asOfLedger.
func (s *Store) sumTrustlineFullScan(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error) {
	q := `
        SELECT COALESCE(sum(balance_stroops), 0)::text
          FROM (
            SELECT DISTINCT ON (account_id)
                   balance_stroops, is_removal
              FROM trustline_observations
             WHERE asset_key = $1
               AND ledger    <= $2
             ORDER BY account_id, ` + trustlineLatestOrder + `
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
	// See TrustlineObservation.IntraLedgerSeq.
	IntraLedgerSeq uint32
}

// InsertClaimableObservation — same shape as InsertTrustlineObservation, keyed on claimable_id.
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
	// intra_ledger_seq-guarded upsert. The conflict
	// tail is shared with InsertClaimableObservationBatch so the ops seed and
	// the live observer write through identical semantics.
	const q = `
        INSERT INTO claimable_observations (
            claimable_id, asset_key, ledger, observed_at,
            balance_stroops, is_removal, intra_ledger_seq, walk_version
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8
        )` + claimableObservationUpsert
	_, err := s.db.ExecContext(ctx, q,
		o.ClaimableID, o.AssetKey, int(o.Ledger), o.ObservedAt.UTC(),
		o.Balance.String(), o.IsRemoval, int64(o.IntraLedgerSeq), observationWalkVersion,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertClaimableObservation %s@%d: %w", o.ClaimableID, o.Ledger, err)
	}
	return nil
}

// claimableObservationUpsert is the ON CONFLICT tail shared by the single-row and batch writers so seeded and live rows use byte-identical
// SQL.
const claimableObservationUpsert = ` ON CONFLICT (claimable_id, ledger, observed_at) DO UPDATE SET
            balance_stroops  = EXCLUDED.balance_stroops,
            is_removal       = EXCLUDED.is_removal,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq,
            walk_version     = EXCLUDED.walk_version
        WHERE (claimable_observations.walk_version, claimable_observations.intra_ledger_seq)
           <= (EXCLUDED.walk_version, EXCLUDED.intra_ledger_seq)`

// InsertClaimableObservationBatch persists rows in one multi-row INSERT with the same intra_ledger_seq-guarded upsert as [Store.InsertClaimableObservation], so an earlier intra-ledger change never overwrites a later one.
func (s *Store) InsertClaimableObservationBatch(ctx context.Context, rows []ClaimableObservation) error {
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		o := &rows[i]
		if o.ClaimableID == "" {
			return fmt.Errorf("timescale: InsertClaimableObservationBatch: row %d empty ClaimableID", i)
		}
		if o.AssetKey == "" {
			return fmt.Errorf("timescale: InsertClaimableObservationBatch: row %d (cb=%s) empty AssetKey", i, o.ClaimableID)
		}
		if o.Balance == nil {
			return fmt.Errorf("timescale: InsertClaimableObservationBatch: row %d (cb=%s) nil Balance", i, o.ClaimableID)
		}
	}
	insertRows := dedupeClaimableObservations(rows)

	const ncols = 8
	var sb strings.Builder
	sb.WriteString(`
        INSERT INTO claimable_observations (
            claimable_id, asset_key, ledger, observed_at,
            balance_stroops, is_removal, intra_ledger_seq, walk_version
        ) VALUES `)
	args := make([]any, 0, ncols*len(insertRows))
	for i := range insertRows {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * ncols
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8)
		o := &insertRows[i]
		args = append(args,
			o.ClaimableID, o.AssetKey, int(o.Ledger), o.ObservedAt.UTC(),
			o.Balance.String(), o.IsRemoval, int64(o.IntraLedgerSeq), observationWalkVersion,
		)
	}
	sb.WriteString(claimableObservationUpsert)

	if _, err := s.db.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("timescale: InsertClaimableObservationBatch (%d rows): %w", len(insertRows), err)
	}
	return nil
}

// claimableObservationKey is the ON CONFLICT identity; observed_at is a UTC UnixNano so instants differing only in location or monotonic
// reading collapse.
type claimableObservationKey struct {
	claimableID     string
	ledger          uint32
	observedAtNanos int64
}

// dedupeClaimableObservations collapses rows colliding on the conflict key,
// keeping the LAST copy of each in first-seen order.
func dedupeClaimableObservations(rows []ClaimableObservation) []ClaimableObservation {
	seen := make(map[claimableObservationKey]int, len(rows))
	out := make([]ClaimableObservation, 0, len(rows))
	for i := range rows {
		k := claimableObservationKey{
			claimableID:     rows[i].ClaimableID,
			ledger:          rows[i].Ledger,
			observedAtNanos: rows[i].ObservedAt.UTC().UnixNano(),
		}
		if at, dup := seen[k]; dup {
			out[at] = rows[i]
			continue
		}
		seen[k] = len(out)
		out = append(out, rows[i])
	}
	return out
}

// Read-path tie-break: every DISTINCT ON reader below orders by `ledger DESC, walk_version DESC,
// intra_ledger_seq DESC`, NOT ledger alone. Two rows can share a (key, ledger): an ops seed stamps
// [SeedIntraLedgerSeq] while the live observer writes the real ordinal, and ordering on ledger
// alone leaves that pick to the planner. The write path already guards this (upserts only
// overwrite when `(walk_version, intra_ledger_seq) <= EXCLUDED`); the read path must match. The
// seed sentinel wins at equal ledger because it is the ledger's final state.

// LiveClaimable is a claimable balance's latest served observation when that observation is live.
type LiveClaimable struct {
	AssetKey string
	Ledger   uint32
}

// LiveClaimableObservations returns by claimable_id every claimable balance whose latest served observation is live, picked as SumClaimableBalancesAtOrBefore picks at the tip; unbounded-latest-ok: a balance created before any floor and never claimed is still live.
func (s *Store) LiveClaimableObservations(ctx context.Context) (map[string]LiveClaimable, error) {
	const q = `
        SELECT claimable_id, asset_key, ledger
          FROM (
            SELECT DISTINCT ON (asset_key, claimable_id)
                   claimable_id, asset_key, ledger, is_removal
              FROM claimable_observations
             ORDER BY asset_key, claimable_id, ledger DESC, walk_version DESC, intra_ledger_seq DESC
          ) latest
         WHERE NOT is_removal
    `
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: LiveClaimableObservations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]LiveClaimable)
	for rows.Next() {
		var (
			id  string
			row LiveClaimable
		)
		if err := rows.Scan(&id, &row.AssetKey, &row.Ledger); err != nil {
			return nil, fmt.Errorf("timescale: LiveClaimableObservations scan: %w", err)
		}
		if prev, dup := out[id]; dup && prev.AssetKey != row.AssetKey {
			// A claimable balance's asset is immutable; two live assets for
			// one id means the served rows are wrong, not the lake.
			return nil, fmt.Errorf("timescale: LiveClaimableObservations: claimable %s is live under both %s and %s", id, prev.AssetKey, row.AssetKey)
		}
		out[id] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LiveClaimableObservations: %w", err)
	}
	return out, nil
}

// SumClaimableBalancesAtOrBefore is SumTrustlineBalancesAtOrBefore keyed on claimable_id; unbounded-latest-ok: a balance created before any floor and never claimed is still live.
func (s *Store) SumClaimableBalancesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT COALESCE(sum(balance_stroops), 0)::text
          FROM (
            SELECT DISTINCT ON (claimable_id)
                   balance_stroops, is_removal
              FROM claimable_observations
             WHERE asset_key = $1
               AND ledger    <= $2
             ORDER BY claimable_id, ledger DESC, walk_version DESC, intra_ledger_seq DESC
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
	// See TrustlineObservation.IntraLedgerSeq.
	IntraLedgerSeq uint32
}

// InsertLPReserveObservation — keyed on (pool_id, asset_key).
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
	// intra_ledger_seq-guarded upsert.
	const q = `
        INSERT INTO lp_reserve_observations (
            pool_id, asset_key, ledger, observed_at,
            balance_stroops, is_removal, intra_ledger_seq, walk_version
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8
        )
        ON CONFLICT (pool_id, asset_key, ledger, observed_at) DO UPDATE SET
            balance_stroops  = EXCLUDED.balance_stroops,
            is_removal       = EXCLUDED.is_removal,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq,
            walk_version     = EXCLUDED.walk_version
        WHERE (lp_reserve_observations.walk_version, lp_reserve_observations.intra_ledger_seq)
           <= (EXCLUDED.walk_version, EXCLUDED.intra_ledger_seq)
    `
	_, err := s.db.ExecContext(ctx, q,
		o.PoolID, o.AssetKey, int(o.Ledger), o.ObservedAt.UTC(),
		o.Balance.String(), o.IsRemoval, int64(o.IntraLedgerSeq), observationWalkVersion,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertLPReserveObservation %s/%s@%d: %w", o.PoolID, o.AssetKey, o.Ledger, err)
	}
	return nil
}

// SumLPReservesAtOrBefore sums the latest (pool_id, asset_key) observation across pools; unbounded-latest-ok: a pool whose last row predates any floor still holds those reserves.
func (s *Store) SumLPReservesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT COALESCE(sum(balance_stroops), 0)::text
          FROM (
            SELECT DISTINCT ON (pool_id)
                   balance_stroops, is_removal
              FROM lp_reserve_observations
             WHERE asset_key = $1
               AND ledger    <= $2
             ORDER BY pool_id, ledger DESC, walk_version DESC, intra_ledger_seq DESC
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

	// IntraLedgerSeq is the within-ledger change position; it makes the FINAL change win when a
	// (contract, holder) changes several times in one ledger. See TrustlineObservation.
	IntraLedgerSeq uint32
}

// InsertSACBalanceObservation — keyed on (contract_id, holder).
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
	// the wrong-supply-component fix.
	const q = `
        INSERT INTO sac_balance_observations (
            contract_id, asset_key, holder, ledger, observed_at,
            balance_stroops, is_removal, intra_ledger_seq, walk_version
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9
        )
        ON CONFLICT (contract_id, holder, ledger, observed_at) DO UPDATE SET
            asset_key        = EXCLUDED.asset_key,
            balance_stroops  = EXCLUDED.balance_stroops,
            is_removal       = EXCLUDED.is_removal,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq,
            walk_version     = EXCLUDED.walk_version
        WHERE (sac_balance_observations.walk_version, sac_balance_observations.intra_ledger_seq)
           <= (EXCLUDED.walk_version, EXCLUDED.intra_ledger_seq)
    `
	_, err := s.db.ExecContext(ctx, q,
		o.ContractID, o.AssetKey, o.Holder, int(o.Ledger), o.ObservedAt.UTC(),
		o.Balance.String(), o.IsRemoval, int64(o.IntraLedgerSeq), observationWalkVersion,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertSACBalanceObservation %s/%s@%d: %w", o.ContractID, o.Holder, o.Ledger, err)
	}
	return nil
}

// SumSACBalancesAtOrBefore sums the latest (contract_id, holder) observation across holders, keyed by asset_key so the reader gets a watched classic asset's SAC component; unbounded-latest-ok: a holder whose last row predates any floor still holds that balance at asOfLedger.
func (s *Store) SumSACBalancesAtOrBefore(ctx context.Context, assetKey string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT COALESCE(sum(balance_stroops), 0)::text
          FROM (
            SELECT DISTINCT ON (contract_id, holder)
                   balance_stroops, is_removal
              FROM sac_balance_observations
             WHERE asset_key = $1
               AND ledger    <= $2
             ORDER BY contract_id, holder, ledger DESC, walk_version DESC, intra_ledger_seq DESC
          ) latest
         WHERE NOT is_removal
    `
	return scanSum(ctx, s.db, q, assetKey, int(asOfLedger))
}

// SACBalanceObservationsExist reports whether any sac_balance_observations row exists for the asset at-or-before the ledger, regardless of is_removal.
func (s *Store) SACBalanceObservationsExist(ctx context.Context, assetKey string, asOfLedger uint32) (bool, error) {
	const q = `
        SELECT EXISTS (
            SELECT 1
              FROM sac_balance_observations
             WHERE asset_key = $1
               AND ledger    <= $2
        )
    `
	var exists bool
	if err := s.db.QueryRowContext(ctx, q, assetKey, int(asOfLedger)).Scan(&exists); err != nil {
		return false, fmt.Errorf("timescale: SACBalanceObservationsExist %s: %w", assetKey, err)
	}
	return exists, nil
}

// TrustlineBalanceForAccountAtOrBefore returns the latest trustline balance for (account, asset) at or before the ledger, or non-nil zero when none is in scope or the latest is a removal.
func (s *Store) TrustlineBalanceForAccountAtOrBefore(ctx context.Context, accountID, assetKey string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT balance_stroops::text, is_removal
          FROM trustline_observations
         WHERE account_id = $1
           AND asset_key  = $2
           AND ledger    <= $3
         ORDER BY ledger DESC, walk_version DESC, intra_ledger_seq DESC
         LIMIT 1
    `
	return scanLatestBalance(ctx, s.db, q, accountID, assetKey, int(asOfLedger))
}

// SACBalanceForContractAtOrBefore returns the latest SAC balance for (holder, asset) at or before the ledger, zero on no observation or removal, like TrustlineBalanceForAccountAtOrBefore.
func (s *Store) SACBalanceForContractAtOrBefore(ctx context.Context, contractHolder, assetKey string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT balance_stroops::text, is_removal
          FROM sac_balance_observations
         WHERE holder    = $1
           AND asset_key = $2
           AND ledger   <= $3
         ORDER BY ledger DESC, walk_version DESC, intra_ledger_seq DESC
         LIMIT 1
    `
	return scanLatestBalance(ctx, s.db, q, contractHolder, assetKey, int(asOfLedger))
}

// scanLatestBalance returns zero on sql.ErrNoRows or is_removal=true, else the non-nil balance.
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

// scanSum runs q expecting one TEXT-cast NUMERIC and returns it as *big.Int.
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

// MinClassicComponentLedger returns how far the slowest of the four classic-supply component observers has progressed at or before `asOfLedger` (each contributes its watermark), so the supply Refresher can detect lagging snapshots.
func (s *Store) MinClassicComponentLedger(ctx context.Context, assetKey string, asOfLedger uint32) (uint32, error) {
	// Cheap per-asset half: is this asset instrumented at all? Postgres
	// short-circuits the OR chain, so a normal asset stops at the first
	// EXISTS.
	const instrumentedQ = `
		SELECT EXISTS (SELECT 1 FROM trustline_observations
		                WHERE asset_key = $1 AND ledger <= $2)
		    OR EXISTS (SELECT 1 FROM claimable_observations
		                WHERE asset_key = $1 AND ledger <= $2)
		    OR EXISTS (SELECT 1 FROM lp_reserve_observations
		                WHERE asset_key = $1 AND ledger <= $2)
		    OR EXISTS (SELECT 1 FROM sac_balance_observations
		                WHERE asset_key = $1 AND ledger <= $2)
	`
	var instrumented bool
	if err := s.db.QueryRowContext(ctx, instrumentedQ, assetKey, int(asOfLedger)).Scan(&instrumented); err != nil {
		return 0, fmt.Errorf("timescale: MinClassicComponentLedger instrumented: %w", err)
	}
	if !instrumented {
		return 0, nil
	}
	return s.classicObserverWatermark(ctx, asOfLedger)
}

// classicWatermarkTTL bounds watermark caching; far tighter than the ~85-minute staleness threshold, so it cannot mask a stalled observer.
const classicWatermarkTTL = 30 * time.Second

type classicWatermarkCache struct {
	mu        sync.Mutex
	asOf      uint32
	ledger    uint32
	fetchedAt time.Time
}

// classicObserverWatermark returns the lowest watermark across the four classic-supply component tables, memoized per [classicWatermarkTTL]
// and keyed on asOfLedger so a backfill replaying history is never served a live-tip watermark.
func (s *Store) classicObserverWatermark(ctx context.Context, asOfLedger uint32) (uint32, error) {
	c := &s.classicWatermark
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.asOf == asOfLedger && !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < classicWatermarkTTL {
		return c.ledger, nil
	}

	const q = `
		WITH observer_watermark AS (
		    SELECT NULLIF(COALESCE(MAX(ledger), 0), 0) AS l
		      FROM trustline_observations WHERE ledger <= $1
		    UNION ALL
		    SELECT NULLIF(COALESCE(MAX(ledger), 0), 0) AS l
		      FROM claimable_observations WHERE ledger <= $1
		    UNION ALL
		    SELECT NULLIF(COALESCE(MAX(ledger), 0), 0) AS l
		      FROM lp_reserve_observations WHERE ledger <= $1
		    UNION ALL
		    SELECT NULLIF(COALESCE(MAX(ledger), 0), 0) AS l
		      FROM sac_balance_observations WHERE ledger <= $1
		)
		SELECT COALESCE(MIN(l), 0) FROM observer_watermark WHERE l IS NOT NULL
	`
	var watermark uint32
	if err := s.db.QueryRowContext(ctx, q, int(asOfLedger)).Scan(&watermark); err != nil {
		return 0, fmt.Errorf("timescale: classicObserverWatermark: %w", err)
	}
	c.asOf, c.ledger, c.fetchedAt = asOfLedger, watermark, time.Now()
	return watermark, nil
}
