-- 0105 up — classic_movements hypertable (ADR-0047 D1).
--
-- Pre-P23 classic-asset movements (payments, path payments, account
-- merges, clawbacks, claimable balances, liquidity-pool deposits/
-- withdrawals) are reconstructed from the ClickHouse raw lake
-- (stellar.operations / operation_results / ledger_entry_changes),
-- never from Horizon (ADR-0001) and never from a MinIO walk
-- (ADR-0034). This table is the shared destination for every phase
-- of that reconstruction; internal/sources/classicmovements is the
-- sole writer (ADR-0031 "one writer per domain" applied to a new,
-- non-projected, lake-derived domain — mirrors how internal/sources/sdex
-- sits outside the projector but owns its own destination table).
--
-- Phasing (docs/adr/0047-pre-p23-classic-movement-reconstruction.md):
--   Phase 1 (THIS migration's first writer): payment, create_account.
--   Phase 2: path_payment.
--   Phase 3: claimable_balance_create/claim/clawback, clawback.
--   Phase 4: account_merge, liquidity_pool_deposit/withdraw (+ the
--            CAP-0038 trustline-revocation auto-liquidation edge case).
--
-- The movement_kind and provenance CHECKs below admit ALL TEN of D1's
-- kinds (and both provenance values) up front, even though Phase 1's
-- decoder (internal/sources/classicmovements) only ever emits
-- 'payment' / 'create_account' today — so Phases 2-4 add rows, not
-- schema churn. 'cap67_event' is RESERVED per D1 for a possible
-- future normalization of post-P23 sep41_transfers 'transfer' rows
-- into this same table; no writer uses it yet.
--
-- Historical-only, not live (ADR-0047 D2): post-P23 (ledger >=
-- 58,762,517, Whisk/CAP-67) every classic movement already emits a
-- unified event that internal/sources/sep41_transfers decodes. This
-- decoder therefore NEVER runs in the live dispatcher — only in
-- `stellarindex-ops classic-movements-backfill`, which hard-clamps
-- its ledger range below the P23 boundary (see that command's flag
-- help + internal/sources/classicmovements/doc.go). Do not add a
-- live wiring edit for this source.
--
-- Retention: DEFERRED (ADR-0047 consequences). Postgres is projected
-- to grow by ~10-11B rows across all four phases; whether the served
-- tier keeps everything or a recent window (with the lake as deep-
-- history backing, per ADR-0034's lake/served split) is a decision
-- for AFTER the first real Phase-1 backfill sizes actual row bytes —
-- deliberately not pre-judged here. No `drop_after` / retention
-- policy is added by this migration. If you see one later without a
-- documented sizing pass behind it, that's drift.
--
-- Gap detector (ADR-0030): NOT registered as a
-- DefaultGapDetectorTargets entry (internal/storage/timescale/
-- per_source_gaps.go) and explicitly listed in that file's
-- excludedFromGapDetector map. The gap detector's job is "is the LIVE
-- writer keeping up with tip" — this table has no live writer; it is
-- filled by bounded, resumable, operator-run backfill windows, so a
-- "coverage vs current tip" gauge would be structurally meaningless
-- (tip is always far past the P23 ledger this source stops at).
-- Coverage verification instead follows ADR-0047 D4: a static
-- recognition test (per-phase op-type switch coverage,
-- internal/sources/classicmovements/recognition_test.go) plus, from
-- Phase 4 onward, an ADR-0033-style projection reconcile against
-- ledger_entry_changes balance deltas.
--
-- Serving: WRITE-PATH ONLY. No /v1/accounts/{g}/movements endpoint
-- reads this table yet (that lands once more phases exist and can
-- share one merged read with sep41_transfers per D1) — see
-- internal/sources/classicmovements/doc.go.
--
-- Operator backfill command shape (internal/ops/chops, subcommand
-- classic-movements-backfill — ADR-0034-lake-derived, same bucket as
-- ch-rebuild):
--
--   /usr/local/sbin/run-heavy-job.sh classic-movements-backfill \
--     stellarindex-ops classic-movements-backfill \
--       -config /etc/stellarindex.toml \
--       -from 2 -to 58762516 \
--       -window 500000 \
--       -write
--
-- Windowed (default -window 500000 ledgers per streamed ClickHouse
-- read + Postgres batch commit, bounding memory the same way
-- ch-rebuild's maxBufferedRange guard does) and resumable (-resume,
-- default true: checkpoints into ingestion_cursors as
-- source='classic-movements-backfill', sub_source='<from>-<to>' after
-- each window commits — same pattern as `stellarindex-ops
-- census-backfill`). Idempotent either way: the PK's
-- ON CONFLICT DO NOTHING makes a re-run over an already-written window
-- a no-op. -to is hard-clamped below the P23 boundary regardless of
-- what the operator passes (ADR-0047 D2) — always run under
-- run-heavy-job.sh per CLAUDE.md's heavy-job doctrine.

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
