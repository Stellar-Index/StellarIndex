-- 0111 up — add `intra_ledger_seq` to the five balance-observation
-- hypertables so the FINAL intra-ledger change deterministically wins the
-- last-writer-wins upsert regardless of the order the parallel PersistEvents
-- workers commit in (audit-2026-07-16 C2-6 — the 8-worker last-writer-wins
-- that persists a NON-final intra-ledger balance).
--
-- The problem: PersistEvents runs PersistWorkers (=8) concurrent drain
-- goroutines that do NOT preserve per-source order (a later event can flush
-- before an earlier one — see internal/pipeline/sink.go). The five
-- `*_observations` writers upsert with `ON CONFLICT (<identity>, ledger,
-- observed_at) DO UPDATE SET <value cols> = EXCLUDED...` — pure
-- last-writer-wins. So when a single (account/holder/pool/claimable, asset,
-- ledger) has MULTIPLE changes WITHIN one ledger, whichever WORKER commits
-- last wins — which is NOT necessarily the final intra-ledger state. A stale
-- intra-ledger balance can be persisted as the observation for that ledger →
-- a wrong classic-supply component (the served supply the operator keeps
-- re-backfilling).
--
-- The fix: stamp every observation with its within-ledger position and guard
-- the upsert so an earlier intra-ledger change can never overwrite a later
-- one:
--   ON CONFLICT (...) DO UPDATE SET <value cols> = EXCLUDED...,
--                                   intra_ledger_seq = EXCLUDED.intra_ledger_seq
--     WHERE <table>.intra_ledger_seq <= EXCLUDED.intra_ledger_seq
-- A higher-or-equal position wins; a lower position (an earlier intra-ledger
-- change delivered late by an out-of-order worker) is rejected. Equal is
-- allowed so a deterministic re-backfill (which re-assigns the SAME position
-- per change) stays idempotent-corrective — it re-writes the identical final
-- value rather than no-op'ing.
--
-- intra_ledger_seq is a MONOTONIC POSITION, not a money value (NUMERIC-lint:
-- bigint is correct — it is not a stroop/price/supply amount, so it is
-- deliberately NOT numeric). It is the dispatcher's per-ledger entry-change
-- counter (internal/dispatcher: LedgerEntryChangeContext.IntraLedgerSeq),
-- assigned in the canonical meta-walk order — transactions in apply order,
-- and within each transaction fee-changes, tx-changes-before, operations (in
-- op_index order, each op's changes in change_index order), tx-changes-after.
-- That is a faithful single-integer encoding of (tx position, op_index,
-- change_index) — the same canonical within-ledger order
-- internal/storage/clickhouse/entry_change_reader.go uses — so the
-- highest-position change is the final intra-ledger state.
--   * 0 (the DEFAULT) is the FIRST change in a ledger's walk AND the value
--     every pre-existing row backfills to. A re-observed ledger overwrites a
--     seq-0 legacy row (0 <= EXCLUDED), so no legacy row is stranded.
--   * the OPS SEED writers (stellarindex-ops supply-seed / supply-seed-sac)
--     stamp the sentinel `4294967295` (= math.MaxUint32,
--     timescale.SeedIntraLedgerSeq) — the seed is the authoritative
--     reconstructed FINAL state for its ledger (the latest entry from the
--     ClickHouse lake, ADR-0034), so it must sit at the END of the
--     intra-ledger order and a live per-ledger change (a much smaller
--     position) can never overwrite it, while a re-seed (equal sentinel)
--     stays corrective. The live counter cannot reach this value (it would
--     require 4.3e9 entry-changes in one ledger).
--
-- Composition with the INV-3 `derive_generation` guard (migrations
-- 0109/0110): these five observation tables DO NOT carry derive_generation —
-- 0109/0110 added it to the three core served-tier tables and the protocol
-- projector tables, not here (the served supply VALUE these components feed
-- lives in asset_supply_history, which already has the generation guard). So
-- there is no composition to encode today; the guard is purely on
-- intra_ledger_seq. If derive_generation is ever added to these tables, the
-- guard must compose with GENERATION AS THE OUTER key and SEQ AS THE INNER —
-- a higher generation (a corrected re-derive) wins regardless of position,
-- and WITHIN a generation the higher intra-ledger position wins — i.e.
--   WHERE (<t>.derive_generation, <t>.intra_ledger_seq)
--      <= (EXCLUDED.derive_generation, EXCLUDED.intra_ledger_seq)
-- (a lexicographic ROW comparison). Do NOT collapse the two into one column.
--
-- Additive with a DEFAULT so the currently-deployed binary (whose INSERT
-- column lists don't mention this column) keeps working unmodified —
-- old-binary-safe per repo convention (cf. 0108/0109/0110). TimescaleDB
-- 2.11+ (r1 runs 2.26) supports ADD COLUMN with a DEFAULT on a compressed
-- hypertable directly — no decompress/recompress dance and no chunk rewrite.

BEGIN;

ALTER TABLE account_observations
    ADD COLUMN intra_ledger_seq bigint NOT NULL DEFAULT 0;

ALTER TABLE trustline_observations
    ADD COLUMN intra_ledger_seq bigint NOT NULL DEFAULT 0;

ALTER TABLE claimable_observations
    ADD COLUMN intra_ledger_seq bigint NOT NULL DEFAULT 0;

ALTER TABLE lp_reserve_observations
    ADD COLUMN intra_ledger_seq bigint NOT NULL DEFAULT 0;

ALTER TABLE sac_balance_observations
    ADD COLUMN intra_ledger_seq bigint NOT NULL DEFAULT 0;

COMMENT ON COLUMN account_observations.intra_ledger_seq IS
    'Within-ledger position of the change that produced this row, in the '
    'dispatcher''s canonical meta-walk order (audit-2026-07-16 C2-6). Guards '
    'the last-writer-wins upsert (intra_ledger_seq <= EXCLUDED.intra_ledger_seq) '
    'so an out-of-order PersistEvents worker can never persist a stale '
    'intra-ledger balance as the final observation. Ops seeds stamp '
    'math.MaxUint32 (the authoritative final state for the ledger).';
COMMENT ON COLUMN trustline_observations.intra_ledger_seq IS
    'Within-ledger change position (audit-2026-07-16 C2-6). See '
    'account_observations.intra_ledger_seq.';
COMMENT ON COLUMN claimable_observations.intra_ledger_seq IS
    'Within-ledger change position (audit-2026-07-16 C2-6). See '
    'account_observations.intra_ledger_seq.';
COMMENT ON COLUMN lp_reserve_observations.intra_ledger_seq IS
    'Within-ledger change position (audit-2026-07-16 C2-6). See '
    'account_observations.intra_ledger_seq.';
COMMENT ON COLUMN sac_balance_observations.intra_ledger_seq IS
    'Within-ledger change position (audit-2026-07-16 C2-6). See '
    'account_observations.intra_ledger_seq.';

COMMIT;
