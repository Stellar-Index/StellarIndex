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

// InsertAccountObservation appends one [domain.AccountObservation]
// (the persisted shape of internal/sources/accounts.Observation — see
// domain's doc.go for why storage takes the domain type rather than
// importing the accounts package, D8 M0-1) to `account_observations`.
// Per the migration, identity is (account_id, ledger, observed_at) —
// the partition column is dragged into the PK because Timescale
// requires it.
//
// Last-writer-wins on conflict: an account touched multiple times
// in the same ledger (e.g. fee + op + op) writes successive rows
// for the same (account_id, ledger), and the AccountEntry post-
// state is monotonic within a ledger so the latest write is the
// authoritative final state. Implemented as ON CONFLICT DO UPDATE
// with the new values overriding the old; observed_at stays the
// same (it's the ledger close time, not write time).
//
// Defensive guards: rejects zero-value AccountID + nil Balance
// before touching the database. These are precondition violations
// upstream (the observer always populates both) but a misbehaving
// caller would otherwise hit a NOT NULL violation deeper in the
// stack with a less-obvious error message.
func (s *Store) InsertAccountObservation(ctx context.Context, o domain.AccountObservation) error {
	if o.AccountID == "" {
		return errors.New("timescale: InsertAccountObservation: AccountID is empty")
	}
	if o.Balance == nil {
		return fmt.Errorf("timescale: InsertAccountObservation: AccountID=%s Balance is nil", o.AccountID)
	}
	// intra_ledger_seq guards the upsert so a LATER intra-ledger change
	// always wins regardless of which parallel PersistEvents worker commits
	// last (audit-2026-07-16 C2-6). `<=` (not `<`) keeps a deterministic
	// re-backfill — which re-assigns the SAME position per change —
	// idempotent-corrective rather than a no-op.
	const q = `
        INSERT INTO account_observations (
            account_id, ledger, observed_at,
            balance_stroops, home_domain, flags, seq_num, is_removal,
            intra_ledger_seq
        ) VALUES (
            $1, $2, $3,
            $4, $5, $6, $7, $8,
            $9
        )
        ON CONFLICT (account_id, ledger, observed_at) DO UPDATE SET
            balance_stroops  = EXCLUDED.balance_stroops,
            home_domain      = EXCLUDED.home_domain,
            flags            = EXCLUDED.flags,
            seq_num          = EXCLUDED.seq_num,
            is_removal       = EXCLUDED.is_removal,
            intra_ledger_seq = EXCLUDED.intra_ledger_seq
        WHERE account_observations.intra_ledger_seq <= EXCLUDED.intra_ledger_seq
    `
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
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertAccountObservation %s@%d: %w", o.AccountID, o.Ledger, err)
	}
	return nil
}

// AccountObservation is the read-side row shape — same fields as
// [accounts.Observation] but with sql-friendly types where the
// columns are NULL-able (HomeDomain is *string).
//
// Returned by the readers. Why not reuse accounts.Observation
// directly: that type is the wire shape the dispatcher emits;
// keeping the read-side type separate means evolving one shouldn't
// force the other to follow.
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

// LatestAccountObservationAtOrBefore returns the most-recent
// observation for accountID with ledger ≤ asOf. Returns
// [ErrNotFound] when the account has no observations in scope
// (caller falls back to operator-static config per ADR-0021).
//
// Used by the LCMReserveBalanceReader + LCMHomeDomainResolver
// shipping in the next PR.
//
// Schema constraint: `account_observations.ledger` is `integer`
// (postgres int4 = signed 32-bit, max 2,147,483,647). The Go
// signature accepts uint32 for symmetry with Stellar's ledger
// type, but values > MaxInt32 must be capped before reaching
// `pq` — otherwise the driver returns
// `pq: value "..." is out of range for type integer (22003)`
// and the lookup fails. Callers passing "no upper bound" should
// use `math.MaxInt32`, NOT `^uint32(0)`. This function defensively
// caps high values so a regression in any caller doesn't surface
// as a flood of resolver errors. (At Stellar's current ~62M
// ledger and ~5 ledgers/sec there's ~13 years of headroom before
// MaxInt32 is reached; switching to bigint is a future migration.)
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
	var (
		row      AccountObservation
		balRaw   string
		homeStr  sql.NullString
		flagsInt int
		ledger   int
	)
	err := s.db.QueryRowContext(ctx, q, accountID, int(asOfLedger)).Scan(
		&row.AccountID,
		&ledger,
		&row.ObservedAt,
		&balRaw,
		&homeStr,
		&flagsInt,
		&row.SeqNum,
		&row.IsRemoval,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountObservation{}, ErrNotFound
	}
	if err != nil {
		return AccountObservation{}, fmt.Errorf("timescale: LatestAccountObservationAtOrBefore %s@%d: %w", accountID, asOfLedger, err)
	}
	bal, ok := new(big.Int).SetString(balRaw, 10)
	if !ok {
		return AccountObservation{}, fmt.Errorf("timescale: LatestAccountObservationAtOrBefore: parse balance %q for %s", balRaw, accountID)
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

// UpsertAccountObserverWatermark advances the account observer's TRUE
// processed-ledger watermark to processedLedger (migration 0144). The live
// indexer calls this every ledger it drives the observer over — REGARDLESS of
// whether any watched account's balance changed — so the watermark tracks the
// observer's liveness, not its last write.
//
// Singleton row (id=1). Monotonic-advance guard in the WHERE clause: a
// lower-or-equal value is a silent DB no-op, so an out-of-order or replayed
// caller can never regress the anchor (mirrors [Store.UpsertCursor]).
// processed_ledger is `bigint`, so no int4 cap is needed here (unlike the
// int4-columned account_observations reads).
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

// MaxAccountObservationLedger returns how far the ACCOUNT OBSERVER has
// PROCESSED at-or-before asOfLedger — the true observer watermark
// (account_observer_watermark, migration 0144), bounded by asOfLedger. Zero
// when no watermark has been recorded yet (fresh cluster before the first live
// tick), which the refresher's freshness gate treats as its permissive bypass.
//
// F-1320 / R-002 / CS-102 tail (live r1 root-cause). This USED to return
// MAX(ledger) FROM account_observations — but that table only gets a row when
// a watched account's balance CHANGES, so MAX(ledger) was the most recent SDF-
// reserve balance change, NOT the observer's progress. During any quiet period
// it went stale while the observer was healthy: the XLM freshness gate crossed
// its dormancy horizon and false-rejected, firing a continuous
// supply_refresh_error_dominant ticket AND freezing XLM's served as_of on a
// value that was actually current — while MASKING a genuinely-dead observer
// behind the same stale signal.
//
// The watermark advances every ledger the indexer drives the observer over
// (see [Store.UpsertAccountObserverWatermark]), so a healthy-but-quiet
// observer keeps the anchor fresh (gate permissive, supply accepted as
// current) and only a genuinely dead observer stops advancing it — at which
// point the anchor freezes and the gate correctly fails closed past the
// horizon (dead-observer detection preserved).
//
// asOfLedger is capped at int4 for symmetry with the sibling reads, then used
// to bound the returned value: a watermark ahead of the snapshot ledger (which
// should not happen — the indexer advances the watermark no faster than tip)
// is clamped so the gate never sees a negative gap.
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
