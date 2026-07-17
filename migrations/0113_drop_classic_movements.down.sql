-- 0113 down — recreate the classic_movements hypertable dropped by
-- 0113 up (reverses the C2-18 / DAT-03 cleanup).
--
-- Restores migration 0105's exact schema (table + CHECKs + hypertable +
-- indexes + compression capability + compression policy) so
-- `migrate down 1` from 0113 leaves the database in the state 0105
-- established. The table is recreated EMPTY — it was UNPOPULATED by
-- design (superseded by ADR-0048 D2), so a rollback restores structure
-- only, with no data to replay. See
-- migrations/0105_create_classic_movements.up.sql for the original
-- per-column / per-index rationale.

BEGIN;

CREATE TABLE classic_movements (
    -- What kind of two-party asset movement this row represents.
    -- Phase 1 writes 'payment' / 'create_account' only; the other
    -- eight values are admitted so later phases need no migration.
    movement_kind      text         NOT NULL CHECK (movement_kind IN (
        'payment', 'create_account', 'path_payment', 'account_merge',
        'clawback', 'claimable_balance_create', 'claimable_balance_claim',
        'claimable_balance_clawback', 'liquidity_pool_deposit',
        'liquidity_pool_withdraw'
    )),

    -- How this row was derived. 'classic_derived' is everything this
    -- table has ever held; 'cap67_event' is RESERVED (D1) for a
    -- possible future normalization of post-P23 sep41_transfers rows
    -- into this table — no writer emits it today.
    provenance         text         NOT NULL DEFAULT 'classic_derived'
                                     CHECK (provenance IN ('classic_derived', 'cap67_event')),

    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            text         NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),

    -- Disambiguates multiple movement rows produced by the SAME op
    -- (e.g. a future liquidity-pool deposit's two asset legs, or a
    -- path payment's source vs dest leg). Phase 1's two kinds are
    -- always single-leg, so this is always 0 today.
    leg_index          integer      NOT NULL DEFAULT 0 CHECK (leg_index >= 0),

    -- Canonical asset_id string ("native" / "CODE-ISSUER"), same wire
    -- form as every other asset-tagged column in the API (matches
    -- xdrjson.AssetID / canonical.Asset.String()). One asset per row
    -- by construction — a two-asset movement (future LP deposit) is
    -- two rows via leg_index, not two columns here.
    asset              text         NOT NULL,

    -- Classic Int64 stroop amounts fit int64 without truncation risk
    -- (ADR-0003's i128 concern is Soroban-specific), but stored as
    -- NUMERIC per convention for uniformity with every other amount
    -- column the API serves.
    amount             numeric      NOT NULL CHECK (amount >= 0),

    -- G-strkey (or M-strkey for a muxed destination, SEP-23) of the
    -- payer/depositor/source side. NULLable: not every future kind
    -- has a resolvable single "from" (e.g. a claimable-balance claim
    -- takes FROM an escrow object with no G-address of its own).
    from_address       text,

    -- G-strkey (or M-strkey) of the payee/recipient side. NULLable
    -- for the same reason (e.g. claimable_balance_create's "to" is a
    -- Claimants list, not one address, until claimed).
    to_address         text,

    -- Kind-specific remainder (e.g. Phase 3's claimable-balance
    -- BalanceId correlation key). Admits future phases with zero
    -- schema churn — same decision migration 0099 (aquarius_rewards_events)
    -- took for its per-kind remainder. i128-shaped values inside here
    -- would be decimal strings per ADR-0003; none of Phase 1's fields
    -- need it.
    attributes         jsonb        NOT NULL DEFAULT '{}'::jsonb,

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- Natural key per ADR-0047 D1: (ledger, tx_hash, op_index[, leg_index]).
    -- ledger_close_time leads per TS103 (TimescaleDB hypertable PK
    -- must include the partitioning column).
    PRIMARY KEY (ledger_close_time, ledger, tx_hash, op_index, leg_index)
);

COMMENT ON TABLE classic_movements IS
    'Pre-P23 classic-asset movements reconstructed from the ClickHouse '
    'raw lake (ADR-0047). Phase 1 writes payment/create_account only; '
    'the movement_kind/provenance CHECKs admit all ten D1 kinds + both '
    'provenance values up front. Historical-only — never live-wired '
    '(the P23 boundary, ledger 58762517, is a hard upper bound enforced '
    'by the classic-movements-backfill command, not this schema). '
    'Write-path only until a merged read surface with sep41_transfers '
    'ships. See internal/sources/classicmovements/doc.go.';
COMMENT ON COLUMN classic_movements.attributes IS
    'Kind-specific remainder (e.g. a future claimable-balance BalanceId '
    'correlation key). Empty object for every kind Phase 1 writes.';

SELECT create_hypertable(
    'classic_movements',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-kind feed ("every payment across all history").
CREATE INDEX classic_movements_kind_ts_idx
    ON classic_movements (movement_kind, ledger_close_time DESC);

-- Per-account walk, sent side ("what did G... send").
CREATE INDEX classic_movements_from_ts_idx
    ON classic_movements (from_address, ledger_close_time DESC)
    WHERE from_address IS NOT NULL;

-- Per-account walk, received side ("what did G... receive").
CREATE INDEX classic_movements_to_ts_idx
    ON classic_movements (to_address, ledger_close_time DESC)
    WHERE to_address IS NOT NULL;

ALTER TABLE classic_movements SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'movement_kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'classic_movements',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
