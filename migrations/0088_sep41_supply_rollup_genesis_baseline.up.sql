-- 0088 up — pre-Soroban GENESIS BASELINE columns on `sep41_supply_rollup`.
--
-- Why this exists (incident 2026-07-06 follow-up). The SEP-41 Algorithm-3
-- supply refresher derives per-contract total as
--     Σ mint − Σ burn − Σ clawback
-- from `sep41_supply_events` (Postgres). That table is filled by the SEP-41
-- supply OBSERVER, which only ever sees the SOROBAN era: contract events do
-- not exist below the protocol-20 activation ledger (50457424). But a classic
-- asset's SAC wrapper (VELO, AQUA, yXLM, LIBRE, ACT, MBC, XAU/CC5U…,
-- BTC/CAO7…, GQX, …) was largely ISSUED before Soroban existed. Its lifetime
-- mint therefore lives ENTIRELY below ledger 50457424 — a range the observer
-- never populated. Reading only the Soroban-era window, the refresher saw
-- Σburn > Σmint (holders unwrapping/burning tokens minted pre-Soroban) →
-- negative derived total → the Algorithm-3 negative-total guard rejected the
-- snapshot → `stellarindex_aggregator_supply_refresh_error_dominant` +
-- `stellarindex_supply_cross_check_divergence` fired for all 9 SAC-wrappers.
--
-- The certified ClickHouse lake (`stellar.supply_flows`, ADR-0034) DOES carry
-- these pre-Soroban mint/burn/clawback flows: the post-P23 (CAP-67) captive
-- core replayed all classic history and synthesized the unified asset events
-- for it. `internal/storage/clickhouse.supplySumQuery` already sums them over
-- the FULL range and is the source of truth for lifetime SAC supply.
--
-- The fix (Option B — baseline seed, NOT re-source-from-CH). Migration 0085's
-- own header records why we do NOT re-point the aggregator's per-tick read at
-- ClickHouse: the CH `supply_flows` lake is network-wide + map/muxed-variant
-- aware, while the PG observer is watched-set-gated + bare-i128 — their
-- per-contract Soroban-era totals can legitimately differ, so serving live
-- reads from CH would SHIFT numbers that were already correct. Instead we take
-- from CH ONLY the pre-Soroban slice PG has NO data for (ledger < 50457424, a
-- DISJOINT partition of the event set) and seed it as a static per-kind
-- opening balance. The reader then serves
--     genesis(ledger < 50457424, from CH) ⊕ soroban(ledger ≥ 50457424, from PG)
-- so lifetime total = the correct positive value, and a Soroban-only token
-- (no pre-genesis flows) gets a genesis of ZERO — its served number is
-- unchanged (no double-count).
--
-- PROVENANCE (ADR-0033 substrate reproducibility). The pre-Soroban
-- `supply_flows` rows are REPLAY-DERIVED: a post-P23 captive core synthesized
-- the CAP-67 unified asset events for classic history that predates them. They
-- are legitimate on-chain-faithful data, but the exact event set is
-- core-version-dependent — a different replay core could enumerate classic
-- movements slightly differently. The seeded baseline is therefore a
-- point-in-time capture; `genesis_baseline_ledger` + `genesis_seeded_at` record
-- the boundary + when it was taken so a re-seed is auditable.
--
-- i128 discipline (ADR-0003): the three genesis totals are NUMERIC, never a
-- fixed-width int — Σmint alone can exceed i128. Go reads them as decimal
-- ::text → *big.Int.
--
-- Old-binary-safe: purely additive. Existing rows backfill genesis_* to 0 via
-- the column DEFAULT and genesis_baseline_ledger stays NULL (= not seeded);
-- the reader adds nothing until an operator runs the one-time seed
-- (`stellarindex-ops supply seed-sep41-genesis`). A pre-0088 binary ignores
-- the new columns entirely.

ALTER TABLE sep41_supply_rollup
    -- Per-kind pre-Soroban (ledger < genesis_baseline_ledger) running totals,
    -- seeded once from the ClickHouse lake. NUMERIC per ADR-0003; non-negative
    -- (each is a sum of non-negative event amounts — direction is
    -- discriminated by kind at read time, same convention as the Soroban-era
    -- mint_total / burn_total / clawback_total columns).
    ADD COLUMN genesis_mint_total     numeric NOT NULL DEFAULT 0 CHECK (genesis_mint_total     >= 0),
    ADD COLUMN genesis_burn_total     numeric NOT NULL DEFAULT 0 CHECK (genesis_burn_total     >= 0),
    ADD COLUMN genesis_clawback_total numeric NOT NULL DEFAULT 0 CHECK (genesis_clawback_total >= 0),

    -- Exclusive upper ledger bound of the seeded baseline (the protocol-20
    -- activation ledger, 50457424). NULL = not yet seeded — the reader adds no
    -- genesis contribution and a negative Soroban-era total is reported as a
    -- benign `missing_baseline` outcome rather than a paging `compute_error`.
    ADD COLUMN genesis_baseline_ledger integer CHECK (genesis_baseline_ledger IS NULL OR genesis_baseline_ledger >= 0),

    -- When the baseline was seeded (provenance for the replay-derived slice).
    ADD COLUMN genesis_seeded_at timestamptz;

COMMENT ON COLUMN sep41_supply_rollup.genesis_mint_total IS
    'Pre-Soroban (ledger < genesis_baseline_ledger) Σ mint, seeded once from '
    'the ClickHouse supply_flows lake. Added to the Soroban-era mint at read '
    'time so Algorithm-3 total reflects LIFETIME supply incl. pre-genesis '
    'issuance (incident 2026-07-06).';
COMMENT ON COLUMN sep41_supply_rollup.genesis_baseline_ledger IS
    'Exclusive upper ledger bound of the seeded pre-Soroban baseline (50457424, '
    'protocol-20 activation). NULL = not seeded.';
