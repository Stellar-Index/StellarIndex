-- 0152 down — re-create the six scaffold tables and the four Stripe
-- columns exactly as their original migrations created them.
--
-- Every CREATE / COMMENT / index / hypertable / compression setting
-- below is transcribed from the migration that first shipped it —
-- 0017 (wasm_versions, contract_wasm_history), 0021 (tvl_observations),
-- 0023 (anchors), 0024 (classic_asset_stats_5m), 0025
-- (aggregator_exposures), 0118 + 0121 (the stripe_event_log columns,
-- indexes and their COMMENTs) — so a down-then-up cycle and a fresh
-- `migrate up` land on the same schema.
--
-- Two deliberate differences from "byte-identical to the originals":
--
--   1. The compression POLICIES on tvl_observations,
--      classic_asset_stats_5m and aggregator_exposures are NOT re-added
--      here, because the original migrations never added them: they set
--      `timescaledb.compress` only, and the policies come from the
--      operator script scripts/ops/add-missing-compression-policies.sql.
--      Re-run that script after a down if you want them back.
--
--   2. stripe_event_log's TABLE comment is reset to NULL — 0027 created
--      the table without one, and 0152's tombstone comment describes a
--      state this down is undoing.
--
-- Re-created empty. Nothing has ever written any of these tables, so
-- there is no data to restore.

BEGIN;

-- ─── 0017: wasm_versions + contract_wasm_history ────────────────────

CREATE TABLE wasm_versions (
    -- Content-addressed: sha256 of the bytecode, lower-hex.
    wasm_hash         char(64)    NOT NULL,

    -- The bytecode itself. NOT NULL — a row without bytes is meaningless.
    bytecode          bytea       NOT NULL,
    bytecode_size     integer     NOT NULL CHECK (bytecode_size > 0),

    -- First time we saw this hash on-chain. May predate our ingest;
    -- for backfilled rows from JSONL this is the earliest ledger that
    -- referenced the hash.
    first_seen_at     timestamptz NOT NULL DEFAULT now(),
    first_seen_ledger integer     NOT NULL CHECK (first_seen_ledger >= 0),

    PRIMARY KEY (wasm_hash)
);

COMMENT ON TABLE wasm_versions IS
    'Content-addressed WASM bytecode store. One row per unique '
    'sha256(bytecode). Powers the contract-page "WASM bytecode + WAT" '
    'panel (see docs/architecture/showcase-site-data-inventory.md '
    '§7.10).';

COMMENT ON COLUMN wasm_versions.bytecode IS
    'Inline bytea; postgres TOAST handles compression. Typical size '
    '50-500 KB per row.';

CREATE TABLE contract_wasm_history (
    -- The C-strkey of the contract.
    contract_id   text     NOT NULL,

    -- The ledger range this WASM version was active for this contract.
    -- last_ledger NULL = current version; one such row per contract
    -- at most.
    first_ledger  integer  NOT NULL CHECK (first_ledger >= 0),
    last_ledger   integer  CHECK (last_ledger IS NULL OR last_ledger >= first_ledger),

    -- The WASM hash this contract was running. References the
    -- wasm_versions row that holds the bytes.
    wasm_hash     char(64) NOT NULL REFERENCES wasm_versions(wasm_hash),

    PRIMARY KEY (contract_id, first_ledger)
);

COMMENT ON TABLE contract_wasm_history IS
    'Temporal mapping of contract → WASM version. One row per '
    'upgrade. last_ledger IS NULL marks the currently-running '
    'version. Powers the contract-page WASM-history timeline.';

CREATE INDEX contract_wasm_history_current_idx
    ON contract_wasm_history (contract_id) WHERE last_ledger IS NULL;

CREATE INDEX contract_wasm_history_wasm_idx
    ON contract_wasm_history (wasm_hash);

-- ─── 0021: tvl_observations ─────────────────────────────────────────

CREATE TABLE tvl_observations (
    -- Protocol slug — matches /v1/protocols/{slug}.
    protocol_slug      text         NOT NULL,

    observed_at        timestamptz  NOT NULL,
    observed_at_ledger integer      NOT NULL CHECK (observed_at_ledger >= 0),

    -- USD-denominated total at observation time. Numeric for full
    -- precision (we never want to lose cents on multi-billion TVLs).
    tvl_usd            numeric      NOT NULL CHECK (tvl_usd >= 0),

    -- How many pools / vaults / loci of capital contributed to the
    -- total. Helps rank the "depth" of the protocol independent of
    -- the dollar amount.
    pool_count         integer      NOT NULL CHECK (pool_count >= 0),

    -- Per-pool detail: [{"pool_id": "...", "tvl_usd": ..., "tokens": [...]}, ...].
    -- Loose schema deliberately — different protocol kinds have
    -- different per-pool shapes (lending pool vs AMM pair vs
    -- aggregator vault).
    breakdown          jsonb,

    PRIMARY KEY (protocol_slug, observed_at)
);

COMMENT ON TABLE tvl_observations IS
    'Per-protocol TVL ticks (1-minute cadence) with per-pool '
    'breakdown jsonb. Powers protocol-page TVL charts + the macro '
    'TVL aggregate.';

SELECT create_hypertable(
    'tvl_observations',
    'observed_at',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

CREATE INDEX tvl_observations_recent_idx
    ON tvl_observations (observed_at DESC);

ALTER TABLE tvl_observations SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'protocol_slug',
    timescaledb.compress_orderby   = 'observed_at DESC'
);

-- ─── 0023: anchors ──────────────────────────────────────────────────

CREATE TABLE anchors (
    -- The home_domain — primary identity. Many G-accounts can
    -- share one home_domain, so this is the natural aggregation
    -- key.
    home_domain          text         PRIMARY KEY,

    -- SEP-1 payload + parsed convenience fields. Parsed fields
    -- are denormalized from the full payload for fast reads.
    org_name             text,
    description          text,
    contact_email        text,

    sep1_payload         jsonb,
    sep1_resolved_at     timestamptz,

    -- Resolution status — distinguishes "we tried and got a
    -- valid stellar.toml" from "we tried and the fetch errored"
    -- so the UI can show appropriate badges.
    sep1_resolved_status text         NOT NULL DEFAULT 'pending'
                                      CHECK (sep1_resolved_status IN
                                            ('pending','ok','fetch_failed','parse_failed','tls_failed'))
);

COMMENT ON TABLE anchors IS
    'Aggregated SEP-1 metadata per home_domain. One row per domain '
    'regardless of how many G-accounts the anchor runs.';

-- ─── 0024: classic_asset_stats_5m ───────────────────────────────────

CREATE TABLE classic_asset_stats_5m (
    bucket            timestamptz NOT NULL,
    asset_id          text        NOT NULL,    -- references classic_assets.asset_id

    -- Trustlines that have been opened to this asset. NULL when
    -- we don't have data yet (asset newly observed).
    trustline_count   bigint      CHECK (trustline_count IS NULL OR trustline_count >= 0),

    -- Issuer's outstanding balance — supply computer's Algorithm 2
    -- output. NULL when the issuer is not in our watched-classic
    -- list (we skip the supply computation for unwatched assets).
    outstanding_supply numeric    CHECK (outstanding_supply IS NULL OR outstanding_supply >= 0),

    -- Rolling 24h trade volume in USD. NULL when no recent trades
    -- OR when no USD-volume rate is available for the pair (off-
    -- chain CEX/FX feeds typically have this; on-chain assets
    -- only get USD volume if their quote is in the operator's
    -- usd_pegged_classic_assets list per L2.2).
    volume_24h_usd    numeric     CHECK (volume_24h_usd IS NULL OR volume_24h_usd >= 0),

    last_trade_ledger integer     CHECK (last_trade_ledger IS NULL OR last_trade_ledger >= 0),

    PRIMARY KEY (bucket, asset_id)
);

COMMENT ON TABLE classic_asset_stats_5m IS
    'Per-asset summary stats refreshed every 5 minutes. Pre-computed '
    'so /v1/coins + /coins/{slug} list views are O(1) lookups.';

SELECT create_hypertable(
    'classic_asset_stats_5m',
    'bucket',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

CREATE INDEX classic_asset_stats_5m_asset_idx
    ON classic_asset_stats_5m (asset_id, bucket DESC);

ALTER TABLE classic_asset_stats_5m SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'asset_id',
    timescaledb.compress_orderby   = 'bucket DESC'
);

-- ─── 0025: aggregator_exposures ─────────────────────────────────────

CREATE TABLE aggregator_exposures (
    -- The vault contract id.
    vault_contract_id   text         NOT NULL,

    -- Which underlying protocol the capital is deployed in.
    -- Free-form (joins to protocols.slug); not constrained because
    -- the set grows over time.
    underlying_protocol text         NOT NULL,

    observed_at         timestamptz  NOT NULL,
    observed_at_ledger  integer      NOT NULL CHECK (observed_at_ledger >= 0),

    -- USD-denominated exposure at observation time.
    exposure_usd        numeric      NOT NULL CHECK (exposure_usd >= 0),

    -- Per-protocol detail — schema varies by underlying protocol.
    -- e.g. for Blend: {"supply": ..., "borrow": ..., "rate": ...}.
    -- For Aquarius: {"lp_share": ..., "pool_id": ...}.
    detail              jsonb,

    PRIMARY KEY (vault_contract_id, underlying_protocol, observed_at)
);

COMMENT ON TABLE aggregator_exposures IS
    'Per-vault capital allocation tracking. One row per (vault, '
    'underlying_protocol, tick). Distinct from trades.routed_via '
    'because vaults hold capital persistently — this captures the '
    'state, not per-tx flow.';

SELECT create_hypertable(
    'aggregator_exposures',
    'observed_at',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

CREATE INDEX aggregator_exposures_vault_idx
    ON aggregator_exposures (vault_contract_id, observed_at DESC);
CREATE INDEX aggregator_exposures_protocol_idx
    ON aggregator_exposures (underlying_protocol, observed_at DESC);

ALTER TABLE aggregator_exposures SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'vault_contract_id, underlying_protocol',
    timescaledb.compress_orderby   = 'observed_at DESC'
);

-- ─── 0118 + 0121: the stripe_event_log dead-letter + claim shape ────

ALTER TABLE stripe_event_log
    ADD COLUMN IF NOT EXISTS dead_lettered_at        timestamptz,
    ADD COLUMN IF NOT EXISTS dead_letter_reason      text,
    ADD COLUMN IF NOT EXISTS dead_letter_resolved_at timestamptz,
    ADD COLUMN IF NOT EXISTS claimed_at              timestamptz;

COMMENT ON COLUMN stripe_event_log.dead_lettered_at IS
    'Set when a Stripe event was acknowledged but this system provisioned '
    'nothing for it (paid-with-no-keys, or every per-key upgrade failing). '
    'Sticky — the recovery is recorded in dead_letter_resolved_at, never by '
    'clearing this. NULL for every event that completed normally.';

COMMENT ON COLUMN stripe_event_log.dead_letter_reason IS
    'Machine-readable dead-letter class: no_keys_for_identifier | '
    'key_upgrade_failed. Distinct from `error`, which is also set on '
    'transient still-retrying failures and is cleared by the processed-mark.';

COMMENT ON COLUMN stripe_event_log.dead_letter_resolved_at IS
    'Set when a later delivery of a dead-lettered event finally provisioned. '
    'Rows with dead_lettered_at IS NOT NULL AND this NULL are the OPEN '
    'money-in-nothing-provisioned set an operator must reconcile.';

COMMENT ON COLUMN stripe_event_log.claimed_at IS
    'Set when a webhook delivery claims this event for processing; cleared '
    'when that attempt concludes without success (failed / dead-lettered) so '
    'the next retry re-attempts immediately. A claim older than the handler '
    'lease is re-claimable, which is what recovers a processor that died '
    'mid-flight. NULL means no delivery currently holds the row.';

CREATE INDEX IF NOT EXISTS stripe_event_log_open_dead_letters_idx
    ON stripe_event_log (dead_lettered_at)
    WHERE dead_lettered_at IS NOT NULL AND dead_letter_resolved_at IS NULL;

CREATE INDEX IF NOT EXISTS stripe_event_log_claimed_idx
    ON stripe_event_log (claimed_at)
    WHERE claimed_at IS NOT NULL AND processed_at IS NULL;

COMMENT ON TABLE stripe_event_log IS NULL;

COMMIT;
