package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// InsertAccountObservation appends one [domain.AccountObservation] to `account_observations`.
// Storage takes the domain type so it need not import the accounts package.
//
// Last-writer-wins on conflict: an account touched several times in one ledger writes successive
// rows for the same (account_id, ledger), and the AccountEntry post-state is monotonic within a
// ledger, so the latest write is final. observed_at is the ledger close time and stays the same.
//
// A zero-value AccountID and nil Balance are rejected up front rather than as a NOT NULL violation.
func (s *Store) InsertAccountObservation(ctx context.Context, o domain.AccountObservation) error {
	if o.AccountID == "" {
		return errors.New("timescale: InsertAccountObservation: AccountID is empty")
	}
	if o.Balance == nil {
		return fmt.Errorf("timescale: InsertAccountObservation: AccountID=%s Balance is nil", o.AccountID)
	}
	// (walk_version, intra_ledger_seq) guards the upsert so a LATER intra-ledger change wins
	// whichever parallel PersistEvents worker commits last, and a newer walk replaces an older
	// one's row. `<=` (not `<`) lets a deterministic re-backfill, which re-assigns the SAME
	// position, correct the row rather than no-op.
	const q = `
        INSERT INTO account_observations (
            account_id, ledger, observed_at,
            balance_stroops, home_domain, flags, seq_num, is_removal,
            intra_ledger_seq, walk_version
        ) VALUES (
            $1, $2, $3,
            $4, $5, $6, $7, $8,
            $9, $10
        )
        ON CONFLICT (account_id, ledger, observed_at) DO UPDATE SET
            balance_stroops  = EXCLUDED.balance_stroops,
            home_domain      = EXCLUDED.home_domain,
            flags            = EXCLUDED.flags,
            seq_num          = EXCLUDED.seq_num,
            is_removal       = EXCLUDED.is_removal,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq,
            walk_version     = EXCLUDED.walk_version
        WHERE (account_observations.walk_version, account_observations.intra_ledger_seq)
           <= (EXCLUDED.walk_version, EXCLUDED.intra_ledger_seq)
    `
	// NULL = the AccountEntry carries no home_domain (the protocol has no
	// unset-vs-empty distinction). "Never observed" is the absence of a
	// row, which is what readers must key the static fallback on.
	var homeDomain sql.NullString
	if o.HomeDomain != "" {
		homeDomain = sql.NullString{String: o.HomeDomain, Valid: true}
	}
	balance := o.Balance.String() // numeric column accepts decimal text

	_, err := s.db.ExecContext(ctx, q,
		o.AccountID,
		int(o.Ledger),
		o.ObservedAt.UTC(),
		balance,
		homeDomain,
		int(o.Flags),
		o.SeqNum,
		o.IsRemoval,
		int64(o.IntraLedgerSeq),
		observationWalkVersion,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertAccountObservation %s@%d: %w", o.AccountID, o.Ledger, err)
	}
	return nil
}

// AccountObservation is the read-side row shape: the fields of [accounts.Observation] with
// sql-friendly types for NULL-able columns (HomeDomain is *string). It is separate from that
// dispatcher wire type so the two can evolve independently.
type AccountObservation struct {
	AccountID  string
	Ledger     uint32
	ObservedAt time.Time
	Balance    *big.Int
	HomeDomain *string
	Flags      uint32
	SeqNum     int64
	IsRemoval  bool
}

// LatestAccountObservationAtOrBefore returns the most-recent observation for accountID with
// ledger ≤ asOf, or [ErrNotFound] when the account has none in scope (the caller then falls back
// to operator-static config).
//
// `account_observations.ledger` is int4, so a uint32 above MaxInt32 fails with out-of-range
// (22003). Callers wanting "no upper bound" pass `math.MaxInt32`, not `^uint32(0)`; high values
// are capped here too so a caller bug cannot flood resolver errors.
func (s *Store) LatestAccountObservationAtOrBefore(ctx context.Context, accountID string, asOfLedger uint32) (AccountObservation, error) {
	const maxInt32 = uint32(math.MaxInt32)
	if asOfLedger > maxInt32 {
		asOfLedger = maxInt32
	}
	const q = `
        SELECT account_id, ledger, observed_at,
               balance_stroops::text, home_domain, flags, seq_num, is_removal
          FROM account_observations
         WHERE account_id = $1
           AND ledger <= $2
         ORDER BY ledger DESC
         LIMIT 1
    `
	row, err := scanAccountObservation(s.db.QueryRowContext(ctx, q, accountID, int(asOfLedger)))
	if errors.Is(err, sql.ErrNoRows) {
		return AccountObservation{}, ErrNotFound
	}
	if err != nil {
		return AccountObservation{}, fmt.Errorf("timescale: LatestAccountObservationAtOrBefore %s@%d: %w", accountID, asOfLedger, err)
	}
	return row, nil
}

// LatestAccountObservationsAtOrBefore is the batch form of
// [Store.LatestAccountObservationAtOrBefore]: one round trip for many
// accounts. An account with no observation in scope is absent from the map.
func (s *Store) LatestAccountObservationsAtOrBefore(ctx context.Context, accountIDs []string, asOfLedger uint32) (map[string]AccountObservation, error) {
	out := make(map[string]AccountObservation, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	asOfLedger = min(asOfLedger, uint32(math.MaxInt32))
	// Per-account LIMIT 1 keeps each probe the single read's index walk; a
	// ledger floor would drop an issuer whose last change is old.
	const q = `
        SELECT o.account_id, o.ledger, o.observed_at,
               o.balance_stroops::text, o.home_domain, o.flags, o.seq_num, o.is_removal
          FROM unnest($1::text[]) AS a(account_id)
         CROSS JOIN LATERAL (
               SELECT *
                 FROM account_observations
                WHERE account_id = a.account_id
                  AND ledger <= $2
                ORDER BY ledger DESC
                LIMIT 1
         ) o
    `
	rows, err := s.db.QueryContext(ctx, q, accountIDs, int(asOfLedger))
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestAccountObservationsAtOrBefore @%d: %w", asOfLedger, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		row, err := scanAccountObservation(rows)
		if err != nil {
			return nil, fmt.Errorf("timescale: LatestAccountObservationsAtOrBefore @%d: %w", asOfLedger, err)
		}
		out[row.AccountID] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestAccountObservationsAtOrBefore @%d: %w", asOfLedger, err)
	}
	return out, nil
}

// scanAccountObservation scans one row in the column order both latest-
// observation queries select. A Scan error is returned unwrapped so the
// caller can match sql.ErrNoRows.
func scanAccountObservation(sc interface{ Scan(dest ...any) error }) (AccountObservation, error) {
	var (
		row      AccountObservation
		balRaw   string
		homeStr  sql.NullString
		flagsInt int
		ledger   int
	)
	if err := sc.Scan(
		&row.AccountID,
		&ledger,
		&row.ObservedAt,
		&balRaw,
		&homeStr,
		&flagsInt,
		&row.SeqNum,
		&row.IsRemoval,
	); err != nil {
		return AccountObservation{}, err
	}
	bal, ok := new(big.Int).SetString(balRaw, 10)
	if !ok {
		return AccountObservation{}, fmt.Errorf("parse balance %q for %s", balRaw, row.AccountID)
	}
	row.Ledger = uint32(ledger)
	row.Balance = bal
	row.Flags = uint32(flagsInt)
	if homeStr.Valid {
		s := homeStr.String
		row.HomeDomain = &s
	}
	return row, nil
}

// UpsertAccountObserverWatermark advances the observer's TRUE processed-ledger watermark. The
// live indexer calls it every ledger, REGARDLESS of whether a watched balance changed, so the
// watermark tracks liveness rather than the last write.
//
// Singleton row (id=1). The WHERE guard makes a lower-or-equal value a silent no-op, so an
// out-of-order or replayed caller can never regress the anchor (mirrors [Store.UpsertCursor]).
// processed_ledger is bigint, so no int4 cap applies.
func (s *Store) UpsertAccountObserverWatermark(ctx context.Context, processedLedger uint32) error {
	const q = `
        INSERT INTO account_observer_watermark (id, processed_ledger, updated_at)
        VALUES (1, $1, now())
        ON CONFLICT (id) DO UPDATE SET
            processed_ledger = EXCLUDED.processed_ledger,
            updated_at       = EXCLUDED.updated_at
         WHERE EXCLUDED.processed_ledger > account_observer_watermark.processed_ledger
    `
	if _, err := s.db.ExecContext(ctx, q, int64(processedLedger)); err != nil {
		return fmt.Errorf("timescale: UpsertAccountObserverWatermark@%d: %w", processedLedger, err)
	}
	return nil
}

// MaxAccountObservationLedger returns how far the ACCOUNT OBSERVER has PROCESSED at-or-before
// asOfLedger (account_observer_watermark). Zero when none is recorded, which the refresher's
// freshness gate treats as a permissive bypass.
//
// It reads the watermark, not MAX(ledger) FROM account_observations: that table only gets a row
// when a watched balance CHANGES, so MAX(ledger) goes stale in a quiet period (false-rejecting the
// XLM freshness gate). The watermark advances every ledger (see
// [Store.UpsertAccountObserverWatermark]), so only a dead observer freezes it, and the gate then
// fails closed past the horizon.
func (s *Store) MaxAccountObservationLedger(ctx context.Context, asOfLedger uint32) (uint32, error) {
	const maxInt32 = uint32(math.MaxInt32)
	if asOfLedger > maxInt32 {
		asOfLedger = maxInt32
	}
	const q = `SELECT LEAST(processed_ledger, $1) FROM account_observer_watermark WHERE id = 1`
	var ledger int64
	err := s.db.QueryRowContext(ctx, q, int(asOfLedger)).Scan(&ledger)
	if errors.Is(err, sql.ErrNoRows) {
		// No watermark recorded yet → no freshness signal. The refresher's
		// gate reads 0 as its permissive bypass (same posture as an
		// unbacked-freshness computer).
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("timescale: MaxAccountObservationLedger@%d: %w", asOfLedger, err)
	}
	return uint32(ledger), nil
}
