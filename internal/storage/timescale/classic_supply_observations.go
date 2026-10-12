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

// SeedIntraLedgerSeq is the intra_ledger_seq an ops seed writer stamps on every row. It is
// math.MaxUint32 because a seed row is the authoritative FINAL state for its ledger (from the
// ClickHouse lake, ADR-0034), so it must sit at the END of the intra-ledger order: a live change
// can never overwrite it, while a re-seed (equal sentinel) stays corrective under the `<=` guard.
// The live dispatcher counter cannot reach this value, so there is no live/seed ambiguity.
const SeedIntraLedgerSeq = uint32(math.MaxUint32)

// observationWalkVersion is stamped as walk_version on every observation row
// (migration 0199). It is the numbering intra_ledger_seq was drawn from, so
// the upsert guard compares positions only within one walk and lets a
// re-derive under a newer walk replace an older one's row.
const observationWalkVersion = int64(dispatcher.EntryWalkVersion)

// TrustlineObservation is the wire shape for a single
// trustline-delta row. Mirrors trustline_observations columns.
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

// InsertTrustlineObservation appends one [TrustlineObservation], last-writer-wins on conflict,
// like [Store.InsertAccountObservation]: the post-state is monotonic within a ledger, so the
// latest write is the final state. Rejects empty AccountID / AssetKey / nil Balance before the DB.
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

// SumTrustlineBalancesAtOrBefore returns the sum of trustline_balance for `assetKey` across every
// observed account at-or-before `asOfLedger`. The row picked per (account, asset) is the
// most-recent; is_removal=true rows contribute 0. The result is a non-nil *big.Int (zero when the
// asset has no observations).
//
// It reads trustline_balance_current (migration 0215), seeding the asset on first use; the full
// history scan serves only an asset a concurrent delete un-seeded.
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

// seedTrustlineCurrent folds the asset's history into trustline_balance_current and marks it
// seeded, in one transaction under the asset's advisory lock (the one 0215's invalidation takes).
// Strict > so a stale snapshot row never overwrites an equal-key row the trigger rewrote since.
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

// claimableObservationUpsert is the INSERT … ON CONFLICT tail shared by the
// single-row and batch writers, so a seeded row and a live-observed row land
// through byte-identical SQL. Keep them in lockstep: the seed's whole premise
// is that its rows are indistinguishable from the observer's.
const claimableObservationUpsert = ` ON CONFLICT (claimable_id, ledger, observed_at) DO UPDATE SET
            balance_stroops  = EXCLUDED.balance_stroops,
            is_removal       = EXCLUDED.is_removal,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq,
            walk_version     = EXCLUDED.walk_version
        WHERE (claimable_observations.walk_version, claimable_observations.intra_ledger_seq)
           <= (EXCLUDED.walk_version, EXCLUDED.intra_ledger_seq)`

// InsertClaimableObservationBatch persists rows via one multi-row INSERT with the same
// intra_ledger_seq-guarded upsert as [Store.InsertClaimableObservation], so it is idempotent and an
// earlier intra-ledger change can never overwrite a later one.
//
// It exists for `supply seed-claimable-balances`, which can emit one row per live claimable balance
// on the network; at one round-trip per row that seed is round-trip-bound for hours.
//
// Postgres rejects an ON CONFLICT DO UPDATE statement that presents the same conflict key twice
// ("cannot affect row a second time"), so intra-batch duplicates are collapsed last-wins first, as
// InsertSEP41TransferBatch does.
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

// claimableObservationKey is the claimable_observations ON CONFLICT identity
// (claimable_id, ledger, observed_at). observed_at is normalised to a UTC
// UnixNano so two instants differing only in location / monotonic reading —
// which land in the same timestamptz — collapse to one key.
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

// LiveClaimable is a claimable balance's latest served observation when that
// observation is live.
type LiveClaimable struct {
	AssetKey string
	Ledger   uint32
}

// LiveClaimableObservations returns, by claimable_id, every claimable balance whose latest served
// observation is live, picked as SumClaimableBalancesAtOrBefore picks at the tip. `supply
// seed-claimable-balances` retracts each one the lake shows removed.
// unbounded-latest-ok: a balance created before any floor and never claimed is still live.
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

// SumClaimableBalancesAtOrBefore — same shape as
// SumTrustlineBalancesAtOrBefore, keyed on claimable_id.
// unbounded-latest-ok: a balance created before any floor and never claimed is still live.
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
// One change to a pool produces TWO row writes (one per asset
// side); the LP-reserve observer (liquidity_pools) emits both.
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

// SumLPReservesAtOrBefore — most-recent (pool_id, asset_key)
// observation, summed across pools.
// unbounded-latest-ok: a pool whose last row predates any floor still holds those reserves.
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

// SumSACBalancesAtOrBefore sums the most-recent (contract_id, holder) observation across holders,
// keyed by asset_key so the reader gets the SAC component of a watched classic asset directly.
// unbounded-latest-ok: a holder whose last row predates any floor still holds that balance at asOfLedger.
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

// SACBalanceObservationsExist reports whether any sac_balance_observations row exists for the
// asset at-or-before the ledger, regardless of is_removal. SumSACBalancesAtOrBefore's
// COALESCE(sum(...), 0) cannot tell "summed to zero from real rows" from "no rows", and the
// escrow-bound gate in CrossCheckSubsetBound needs that distinction.
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

// TrustlineBalanceForAccountAtOrBefore returns the most-recent trustline balance for the
// (account, asset) pair at-or-before the ledger, or non-nil zero when there is no observation in
// scope or the latest is a removal. The classic-supply reader uses it for IssuerBalance and
// LockedAccountBalances.
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

// SACBalanceForContractAtOrBefore returns the most-recent SAC balance for the (holder, asset) pair
// at-or-before the ledger, zero on no observation or removal, like
// TrustlineBalanceForAccountAtOrBefore. The classic-supply reader uses it for
// LockedContractBalances.
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

// MinClassicComponentLedger returns how far the slowest of the four classic-supply component
// OBSERVERS has progressed at-or-before `asOfLedger`; the supply Refresher uses it to detect
// snapshots whose components lag the snapshot ledger.
//
// Each component contributes its observer WATERMARK, MAX(ledger) across ALL assets in its table,
// and the result is the MIN of the four. The watermark is deliberately global, NOT per-asset:
// observers write a row only when a balance CHANGES, so a per-asset MAX(ledger) measures activity,
// not freshness (a quiet asset is not stale). A dead observer stops advancing every asset's
// watermark, which is still caught.
//
// An entirely empty component contributes no signal (NULLIF, excluded from the MIN). An asset with
// NO observations in ANY component is uninstrumented and returns 0, so the caller skips the
// freshness gate (zero means no signal on Supply.MinComponentLedger); otherwise global watermarks
// would give it a healthy-looking anchor and let a zero-valued supply publish.
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

// classicWatermarkTTL bounds how long [Store.classicObserverWatermark] reuses a cached watermark.
// It is two orders of magnitude tighter than the ~85-minute staleness threshold it feeds, so
// caching cannot mask a stalled observer; it only collapses redundant per-asset reads in a tick.
const classicWatermarkTTL = 30 * time.Second

type classicWatermarkCache struct {
	mu        sync.Mutex
	asOf      uint32
	ledger    uint32
	fetchedAt time.Time
}

// classicObserverWatermark returns the lowest per-observer watermark across the four
// classic-supply component tables, across all assets. Memoized per [classicWatermarkTTL] (see
// [classicWatermarkCache]) and keyed on asOfLedger so a backfill replaying historical ledgers is
// never served a live-tip watermark.
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
