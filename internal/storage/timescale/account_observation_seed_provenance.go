package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AccountObservationSeedProvenanceScope is the fixed singleton key for
// account_observation_seed_provenance (migration 0189). Unlike
// seed-claimable-balances, `supply seed-observations` has no -assets-style
// scope: every pass seeds the whole configured `[supply]
// sdf_reserve_accounts` watchlist, so one row is enough.
const AccountObservationSeedProvenanceScope = "sdf_reserve_accounts"

// AccountObservationSeedProvenance is one complete `supply seed-observations`
// pass's audit record (migration 0189).
type AccountObservationSeedProvenance struct {
	AccountsWatched int
	AccountsSeeded  int
	AccountsMissing int
	AccountsRemoved int
	MinLedgerSeen   *uint32 // nil when AccountsSeeded == 0
	MaxLedgerSeen   *uint32 // nil when AccountsSeeded == 0
	SeededAt        time.Time
}

// UpsertAccountObservationSeedProvenance records (or overwrites) the most
// recent complete seed pass. A row whose counts don't sum to the watchlist
// size is refused: it would claim coverage the pass never actually walked.
func (s *Store) UpsertAccountObservationSeedProvenance(ctx context.Context, p AccountObservationSeedProvenance) error {
	if p.AccountsWatched <= 0 {
		return errors.New("timescale: UpsertAccountObservationSeedProvenance: AccountsWatched must be > 0 — the pass did not cover a watchlist")
	}
	if p.AccountsSeeded < 0 || p.AccountsMissing < 0 || p.AccountsRemoved < 0 {
		return fmt.Errorf("timescale: UpsertAccountObservationSeedProvenance: negative count (seeded %d, missing %d, removed %d)",
			p.AccountsSeeded, p.AccountsMissing, p.AccountsRemoved)
	}
	if sum := p.AccountsSeeded + p.AccountsMissing + p.AccountsRemoved; sum != p.AccountsWatched {
		return fmt.Errorf("timescale: UpsertAccountObservationSeedProvenance: seeded+missing+removed (%d) != watched (%d) — the pass did not cover the whole watchlist",
			sum, p.AccountsWatched)
	}
	const q = `
        INSERT INTO account_observation_seed_provenance (
            scope, accounts_watched, accounts_seeded, accounts_missing, accounts_removed,
            min_ledger_seen, max_ledger_seen, seeded_at
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, now())
        ON CONFLICT (scope) DO UPDATE SET
            accounts_watched = EXCLUDED.accounts_watched,
            accounts_seeded  = EXCLUDED.accounts_seeded,
            accounts_missing = EXCLUDED.accounts_missing,
            accounts_removed = EXCLUDED.accounts_removed,
            min_ledger_seen  = EXCLUDED.min_ledger_seen,
            max_ledger_seen  = EXCLUDED.max_ledger_seen,
            seeded_at        = now()
    `
	var minLedger, maxLedger sql.Null[int64]
	if p.MinLedgerSeen != nil {
		minLedger = sql.Null[int64]{V: int64(*p.MinLedgerSeen), Valid: true}
	}
	if p.MaxLedgerSeen != nil {
		maxLedger = sql.Null[int64]{V: int64(*p.MaxLedgerSeen), Valid: true}
	}
	if _, err := s.db.ExecContext(ctx, q, AccountObservationSeedProvenanceScope, p.AccountsWatched, p.AccountsSeeded,
		p.AccountsMissing, p.AccountsRemoved, minLedger, maxLedger); err != nil {
		return fmt.Errorf("timescale: UpsertAccountObservationSeedProvenance: %w", err)
	}
	return nil
}

// AccountObservationSeedProvenanceRow returns the singleton provenance row;
// ok=false when no complete pass has stamped it.
func (s *Store) AccountObservationSeedProvenanceRow(ctx context.Context) (AccountObservationSeedProvenance, bool, error) {
	const q = `
        SELECT accounts_watched, accounts_seeded, accounts_missing, accounts_removed,
               min_ledger_seen, max_ledger_seen, seeded_at
          FROM account_observation_seed_provenance
         WHERE scope = $1
    `
	var (
		p                    AccountObservationSeedProvenance
		minLedger, maxLedger sql.Null[uint32]
	)
	err := s.db.QueryRowContext(ctx, q, AccountObservationSeedProvenanceScope).Scan(&p.AccountsWatched, &p.AccountsSeeded,
		&p.AccountsMissing, &p.AccountsRemoved, &minLedger, &maxLedger, &p.SeededAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountObservationSeedProvenance{}, false, nil
	}
	if err != nil {
		return AccountObservationSeedProvenance{}, false, fmt.Errorf("timescale: AccountObservationSeedProvenanceRow: %w", err)
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
