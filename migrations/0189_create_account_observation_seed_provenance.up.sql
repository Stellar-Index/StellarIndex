-- 0189 up — audit trail for `supply seed-observations` (GH #1201).
--
-- The reserve-account seed wrote account_observations and left nothing
-- behind but a console summary, so a pass run weeks ago is
-- indistinguishable from one that never ran — unlike `supply
-- seed-sac-balances` (0183) and `supply seed-claimable-balances` (0184),
-- which both stamp provenance. Unlike claimable balances,
-- `[supply] sdf_reserve_accounts` has no -assets-style scope flag: every
-- pass seeds the whole configured watchlist, so a single singleton row is
-- enough — there is no per-asset key to upsert by.
--
-- Only a pass that reached the end of its watchlist with every account
-- resolved to seeded, missing or removed upserts this row (enforced by the
-- accounts_seeded + accounts_missing + accounts_removed = accounts_watched
-- check); a partial or failed pass stamps nothing, and a -dry-run pass
-- never calls the upsert at all.
--
-- New table only: nothing reads it, so the previous binary is unaffected
-- (rule 9).

BEGIN;

CREATE TABLE IF NOT EXISTS account_observation_seed_provenance (
    -- Fixed singleton key: 'sdf_reserve_accounts', the config section this
    -- pass reads. One row because the pass has no per-asset/-account scope.
    scope             text        PRIMARY KEY,
    -- Size of the [supply] sdf_reserve_accounts watchlist this pass covered.
    accounts_watched  integer     NOT NULL CHECK (accounts_watched > 0),
    -- Accounts written to account_observations by this pass.
    accounts_seeded   integer     NOT NULL CHECK (accounts_seeded >= 0),
    -- Watched accounts with no AccountEntry in the lake's capture window.
    accounts_missing  integer     NOT NULL CHECK (accounts_missing >= 0),
    -- Watched accounts whose latest change merged them away.
    accounts_removed  integer     NOT NULL CHECK (accounts_removed >= 0),
    -- Range of the seeded accounts' own last-modified ledgers; NULL when
    -- the pass seeded no account.
    min_ledger_seen   bigint      CHECK (min_ledger_seen IS NULL OR min_ledger_seen >= 0),
    max_ledger_seen   bigint      CHECK (max_ledger_seen IS NULL OR max_ledger_seen >= 0),
    seeded_at         timestamptz NOT NULL DEFAULT now(),
    CHECK (accounts_seeded + accounts_missing + accounts_removed = accounts_watched)
);

COMMENT ON TABLE account_observation_seed_provenance IS
    'Singleton row recording the most recent complete `supply '
    'seed-observations` pass: accounts seeded/missing/removed against the '
    '[supply] sdf_reserve_accounts watchlist, and the seeded ledger range. '
    'Audit trail only — not read by the supply computation.';

COMMIT;
