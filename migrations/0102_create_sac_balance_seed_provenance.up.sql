-- 0102 up — `sac_balance_seed_provenance`: one row per watched SAC-wrapper
-- contract recording the most recent bootstrap seed run.
--
-- Why this exists (PHO/BLND VERDICT follow-up, incident 2026-07-06 /
-- ROADMAP #14 residual). Algorithm 2's `SACWrapped` component
-- (migration 0014 `sac_balance_observations`) is bootstrapped by
-- `stellarindex-ops supply seed-sac-balances`, which reads current SAC
-- Balance(Address) entries from the ClickHouse lake. Two sources now
-- exist:
--
--   - `current_state` — stellar.ledger_entries_current (fast, but has a
--     coverage FLOOR: the current-state materialized view only reflects
--     ledger_entry_changes rows inserted AFTER the MV was created,
--     ~ledger 62,000,000 — a Balance entry dormant since before that
--     floor is invisible to it).
--   - `full_history` — stellar.ledger_entry_changes directly (the
--     certified append-log; complete back to genesis per ADR-0034, but a
--     much heavier scan — MUST run under run-heavy-job.sh).
--
-- The final 2026-07-06 investigation traced PHO/BLND's Algorithm-2
-- under-count to exactly this floor: their largest holders are
-- Phoenix/Blend pool contracts whose SAC Balance entry has been dormant
-- since before ~62M, so `current_state` seeding recovers only a sliver
-- (PHO: ~15 post-floor holders vs the true holder set) while
-- `full_history` recovers the rest. Algorithm-3 (SAC lifetime
-- Σmint−burn−clawback) was independently verified correct to the stroop
-- against the raw lake + stellar.expert — so it is Algorithm 2 that was
-- incomplete, not Algorithm 3 overcounting.
--
-- This table is NOT itself part of the supply computation — it is an
-- auditable "when was this contract last (and how) seeded" record, the
-- SAC-balance analogue of `sep41_supply_rollup`'s
-- genesis_baseline_ledger / genesis_seeded_at columns (migration 0088)
-- and the same ADR-0033 substrate-reproducibility discipline: a seed
-- against a source with a coverage floor should be visibly
-- distinguishable from one that scanned full history, so an operator
-- (or a future decision on the supply_cross_check_divergence downgrade,
-- notes/ROADMAP.md §2 "Supply cross-check downgrade") can tell, per
-- contract, whether a residual divergence is expected (never
-- full-history seeded) or actually anomalous (full-history seeded, still
-- diverging).
--
-- Old-binary-safe: purely additive — a new standalone table, no existing
-- table/column/policy touched. A pre-0102 binary ignores it entirely;
-- `supply seed-sac-balances [-full-history]` upserts a row after each
-- non-dry-run pass.

CREATE TABLE sac_balance_seed_provenance (
    -- SAC-wrapper contract C-strkey — one row per watched contract,
    -- upserted (not appended) so the table always reflects the MOST
    -- RECENT seed pass, mirroring sep41_supply_rollup's one-row-per-
    -- contract shape.
    contract_id      text        PRIMARY KEY,

    -- Operator-mapped classic asset_key (CODE:ISSUER) at seed time, for
    -- readable reporting without a [supply.sac_wrappers] config lookup.
    asset_key        text        NOT NULL,

    -- Which reader produced this pass. 'full_history' is the ONLY source
    -- that can close a dormant-below-the-floor gap; 'current_state' is
    -- the cheap, floor-limited default seed_sac_balances has always run.
    source           text        NOT NULL CHECK (source IN ('current_state', 'full_history')),

    -- Distinct (contract_id, holder) rows the pass found for this
    -- contract (0 is valid — a watched wrapper with no on-chain Balance
    -- entries yet).
    holders_seeded   integer     NOT NULL DEFAULT 0 CHECK (holders_seeded >= 0),

    -- Ledger range of the seeded entries' own last-modified ledgers (NOT
    -- the ledger the scan ran at) — lets an operator see at a glance
    -- whether the pass actually reached below the ~62M current-state
    -- floor (min_ledger_seen < 62,000,000) or only refreshed recent
    -- holders. NULL when holders_seeded = 0 (nothing to bound).
    min_ledger_seen  integer     CHECK (min_ledger_seen IS NULL OR min_ledger_seen >= 0),
    max_ledger_seen  integer     CHECK (max_ledger_seen IS NULL OR max_ledger_seen >= 0),

    seeded_at        timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE sac_balance_seed_provenance IS
    'One row per watched SAC-wrapper contract recording the most recent '
    '`supply seed-sac-balances` pass: which source it read from '
    '(current_state = ledger_entries_current, floor-limited; full_history '
    '= ledger_entry_changes, complete-to-genesis) and how many holders it '
    'found. Audit trail for the PHO/BLND/EURC/KALE dormant-SAC-balance '
    'fix (incident 2026-07-06) — not consumed by the supply computation '
    'itself.';
COMMENT ON COLUMN sac_balance_seed_provenance.source IS
    '''current_state'': read from the floor-limited ledger_entries_current '
    'MV (fast, may miss holders dormant since before ~ledger 62,000,000). '
    '''full_history'': read from the complete ledger_entry_changes '
    'append-log (heavy — run-heavy-job.sh only), closes the floor gap.';
COMMENT ON COLUMN sac_balance_seed_provenance.min_ledger_seen IS
    'Lowest last-modified ledger among the holders this pass found. A '
    'full_history pass with min_ledger_seen well below ~62,000,000 is '
    'direct evidence the floor gap was actually reached, not just a '
    'source-label claim.';
