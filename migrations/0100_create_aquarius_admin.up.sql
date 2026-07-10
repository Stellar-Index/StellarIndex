-- 0100 up — Aquarius governance / upgrade admin event surface.
--
-- ROADMAP #89 (2026-07-10 topic census): the same census that found
-- the rewards-gauge gap (migration 0099) found 8 real, distinct
-- governance/admin topics with no decoder — router + pool lifecycle
-- actions. Lifetime counts (raw `stellar.contract_events`,
-- 2026-07-10):
--
--   apply_upgrade (router)          706  contract-code upgrade applied
--   commit_upgrade (router)         705  contract-code upgrade queued
--   set_privileged_addrs            173  privileged-address-set update
--   apply_transfer_ownership         48  ownership transfer applied
--   commit_transfer_ownership        48  ownership transfer queued
--   enable_emergency_mode            35  circuit-breaker engaged
--   disable_emergency_mode           35  circuit-breaker released
--   pool_gauge_switch_token          31  gauge reward-token swap
--
-- Storage shape mirrors blend_admin (migration 0045): ONE table with
-- an event_kind discriminator + universal promoted columns (admin /
-- target) + a jsonb `attributes` remainder for kind-specific fields,
-- rather than one table per kind. Unlike the original blend_admin
-- migration, event_index is in the PK from the start (blend_admin
-- needed a follow-up migration, 0054-adjacent, to add it — F-1324
-- class bug: without it, two admin events in one op collide under
-- ON CONFLICT DO NOTHING).
--
-- These are governance-layer events, not pricing/liquidity signals;
-- additive analytics only, never VWAP inputs.
--
-- Retention: NONE — granular-coverage mission keeps this forever.
--
-- Historical fill: live ingest writes here from deploy onward; the
-- back-window is re-derived from the raw lake with
--   stellarindex-ops projector-replay -source aquarius -from 52728375
-- Run under /usr/local/sbin/run-heavy-job.sh (CLAUDE.md heavy-job
-- doctrine).

BEGIN;

CREATE TABLE aquarius_admin (
    -- Emitting contract: the router C-strkey for apply_upgrade /
    -- commit_upgrade / set_privileged_addrs / *_transfer_ownership /
    -- enable|disable_emergency_mode, or a pool C-strkey for
    -- pool_gauge_switch_token — see decode_admin.go's per-kind doc
    -- comment for the exact emitter per kind.
    contract_id        text         NOT NULL,

    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            text         NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    event_index        integer      NOT NULL CHECK (event_index >= 0),

    event_kind         text         NOT NULL CHECK (event_kind IN (
        'apply_upgrade', 'commit_upgrade', 'set_privileged_addrs',
        'apply_transfer_ownership', 'commit_transfer_ownership',
        'enable_emergency_mode', 'disable_emergency_mode',
        'pool_gauge_switch_token'
    )),

    -- Universal typed columns; NULL when the event kind doesn't
    -- carry them. admin: the topic-slot actor address, when the
    -- event carries one. target: the "addressed entity" of the
    -- event — new Wasm hash (upgrade) / new owner (ownership
    -- transfer) / new reward token (pool_gauge_switch_token).
    admin              text,
    target             text,

    -- Event-kind-specific remainder — see decode_admin.go's per-kind
    -- doc comment for the exact field set landed here.
    attributes         jsonb        NOT NULL DEFAULT '{}'::jsonb,

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_kind, event_index)
);

COMMENT ON TABLE aquarius_admin IS
    'Aquarius router + pool governance/upgrade lifecycle events (8 '
    'kinds: apply_upgrade, commit_upgrade, set_privileged_addrs, '
    'apply_transfer_ownership, commit_transfer_ownership, '
    'enable_emergency_mode, disable_emergency_mode, '
    'pool_gauge_switch_token). Hypertable on ledger_close_time. '
    'Governance-layer events; never VWAP inputs. See '
    'internal/sources/aquarius/README.md (ROADMAP #89).';
COMMENT ON COLUMN aquarius_admin.target IS
    'The "addressed entity" of the event — new Wasm hash / new owner / '
    'new reward token, depending on event_kind.';

SELECT create_hypertable(
    'aquarius_admin',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '30 days',
    if_not_exists       => TRUE
);

-- Per-contract walk ("this router/pool's admin activity").
CREATE INDEX aquarius_admin_contract_ts_idx
    ON aquarius_admin (contract_id, ledger_close_time DESC);

-- Per-kind feed ("every apply_upgrade across Aquarius").
CREATE INDEX aquarius_admin_kind_ts_idx
    ON aquarius_admin (event_kind, ledger_close_time DESC);

ALTER TABLE aquarius_admin SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id, event_kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'aquarius_admin',
    INTERVAL '30 days',
    if_not_exists => TRUE
);

COMMIT;
