-- 0110 up — extend the INV-3 generation guard (migration 0109) from the
-- 3 CORE served-tier tables to the PROTOCOL projector tables. Adds
-- `derive_generation bigint NOT NULL DEFAULT 0` to every protocol-specific
-- projector-output table so a corrected re-derive can UPDATE a wrong value
-- in place instead of silently no-op'ing (audit-2026-07-16 INV-3 — the
-- re-backfill treadmill, wave 2).
--
-- Before this migration these writers used `ON CONFLICT (natural key) DO
-- NOTHING` with the derived value (an i128 amount scaled by token decimals,
-- a reserve/supply/shares figure, a percent, or governance attributes that
-- carry i128 amounts) OUTSIDE the conflict key, so a re-derive of a
-- corrected value was discarded (rowsInserted=0) — the only fix was
-- DELETE/TRUNCATE + a full re-backfill. The writers now use the same
-- generation-guarded idempotent-corrective upsert 0109 introduced:
--   ON CONFLICT (...) DO UPDATE SET <every value col> = EXCLUDED.<col>,
--                                   derive_generation  = EXCLUDED.derive_generation
--     WHERE <table>.derive_generation <= EXCLUDED.derive_generation
-- so a higher-or-equal generation wins and a lower generation (e.g. a live
-- gen-0 replay) can never revert a correction.
--
-- derive_generation is a MONOTONIC COUNTER, not a money value (NUMERIC-lint:
-- bigint is correct — it is not a stroop/price/supply amount, so it is
-- deliberately NOT numeric):
--   * 0 (the DEFAULT) is the LIVE-ingest generation. Live writes / replays
--     write 0; a gen-0 write can never revert a correction (gen N>0) via the
--     `<=` guard, and a gen-0 write over an existing gen-0 row just re-writes
--     the identical decoded value (harmless).
--   * a POSITIVE generation is stamped only by the re-derive entry points
--     (stellarindex-ops projected-rebuild / ch-rebuild / backfill-router),
--     whose corrections must win over the live value. They use
--     time.Now().Unix(), so a later re-derive supersedes an earlier one.
--
-- Additive with a DEFAULT so the currently-deployed binary (whose INSERT
-- column lists don't mention this column) keeps working unmodified —
-- old-binary-safe per repo convention (cf. 0108/0109). Existing rows
-- backfill to 0 (the live-ingest generation), so the first corrected
-- re-derive (gen N>0) wins over every pre-existing row.
--
-- Every targeted table is a low-to-moderate-volume protocol projector table;
-- adding a plain column WITH a DEFAULT is metadata-only on modern
-- PostgreSQL, and on any of these that is a TimescaleDB hypertable the same
-- holds on 2.11+ (r1 runs 2.26) — no decompress/recompress and no chunk
-- rewrite. No constraints are added, so nothing forces a table rewrite.

BEGIN;

-- sorocredit (migration 0090) — credit_* family
ALTER TABLE credit_positions   ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE credit_statements  ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE credit_settlements ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE credit_events      ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

-- phoenix (migration 0044)
ALTER TABLE phoenix_stake_events ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE phoenix_liquidity    ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

-- aquarius (migrations 0089/0099/0100)
ALTER TABLE aquarius_admin           ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE aquarius_reserves        ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE aquarius_liquidity       ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE aquarius_rewards_events  ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

-- blend (migrations 0042/0045/0058/0063/0095)
ALTER TABLE blend_positions       ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE blend_emitter_events  ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE blend_auctions        ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE blend_backstop_events ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE blend_admin           ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE blend_emissions       ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

-- defindex (migration 0050)
ALTER TABLE defindex_flows ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

-- cctp (migration 0038)
ALTER TABLE cctp_events ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

-- rozo
ALTER TABLE rozo_events ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

-- soroswap (migrations 0042/0056)
ALTER TABLE soroswap_skim_events  ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE soroswap_router_swaps ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

-- comet (migration 0059)
ALTER TABLE comet_liquidity ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

-- sep41 (migration 0047/0057)
ALTER TABLE sep41_transfers     ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE sep41_supply_events ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

COMMENT ON COLUMN blend_positions.derive_generation IS
    'Monotonic re-derive generation (audit-2026-07-16 INV-3, wave 2). 0 = live '
    'ingest; a positive value is stamped by an ops re-derive so its corrected '
    'value wins the ON CONFLICT guard '
    '(derive_generation <= EXCLUDED.derive_generation). A lower generation can '
    'never revert a higher-generation correction. See trades.derive_generation.';

COMMIT;
