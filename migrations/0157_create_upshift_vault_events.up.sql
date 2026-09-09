-- 0157 up — `upshift_vault_events` hypertable (#503).
--
-- One row per decoded Upshift tokenized-vault event. Upshift vaults are
-- ERC-4626-shaped: they take one underlying asset and mint proportional
-- SHARES, and the vault contract IS the share token. Two vaults exist on
-- pubnet, both curated in internal/sources/upshift (MainnetVaults) and
-- both verified against r1's ClickHouse lake on 2026-09-09:
--
--   CCL3WITW… earnUSDC — underlying native USDC (SAC CCW67TSZ…)
--   CC6TRAPQ… earnXLM  — underlying native XLM  (SAC CAS3J7GY…)
--
-- Each vault's underlying is PROVEN, not assumed: the SAC `transfer`
-- that lands one event index ahead of the vault's `deposit`, in the
-- same transaction, moves exactly the amount the deposit reports as
-- `assets`.
--
-- Four of the twelve symbols either vault emits produce a row here:
--
--   deposit    — underlying in, shares minted.  assets + shares,
--                and the (caller, receiver, owner) address triple.
--   withdraw   — shares burned, underlying out. Same columns.
--   transfer   — a SEP-41 movement of the vault's OWN share token:
--                it reassigns the claim without minting or burning,
--                so `shares` is populated and `assets` is NULL.
--   deployed_assets_changed
--              — the vault-level total of capital DEPLOYED into
--                strategies, before and after. old_amount/new_amount.
--
-- NOT written here (recognised and gated by the decoder, projected as
-- zero rows so the ADR-0033 re-derive counts their ledgers rather than
-- going blind on them):
--
--   - deposit_to_subaccount / withdraw_from_subaccount /
--     wallet_deployed_updated / wallet_net_deployed_seeded — the
--     custody-side mirror of the SAME capital movement
--     deployed_assets_changed already reports at vault level. Writing
--     them here would double-count deployed capital.
--   - subaccount_added / admin_set / operator_set — governance, no
--     economic state.
--   - approve — a SEP-41 allowance, not a balance change. The vault's
--     full SEP-41 audit trail belongs to sep41_transfers whenever an
--     operator adds the vault to watched_sep41_contracts; that is a
--     different source writing a different table, so `transfer` living
--     in both is not a double write.
--
-- WHAT THIS TABLE IS NOT: a TVL series. `assets` and `shares` are on
-- DIFFERENT scales — every genesis-era deposit mints shares == assets ×
-- 1,000,000 exactly, the OpenZeppelin ERC-4626 decimals offset of 6 —
-- and the ratio drifts as the share price accrues. Nothing in the
-- pipeline divides one by the other. `new_amount` is the DEPLOYED leg
-- only; the vault's idle balance is not observable in any event, so
-- total assets cannot be derived from these rows and must not be served
-- as if they could. Share SUPPLY *is* derivable and exactly so —
-- SUM(shares) over deposits minus SUM(shares) over withdrawals, since
-- neither vault has ever emitted a mint or burn outside those two.
--
-- Amounts are NUMERIC per ADR-0003: these are i128 values and
-- truncating one to bigint silently mis-reports it.
--
-- Identity: (ledger_close_time, contract_id, ledger, tx_hash, op_index,
-- event_index). event_index is load-bearing — a single operation emits
-- the SAC transfer AND the vault event, and a redemption emits several
-- vault events on one op (the earnUSDC withdraw at ledger 63,812,816
-- sits at event_index 5), so without it all but one collapse under the
-- upsert. ledger_close_time leads because TimescaleDB requires the
-- partition column in the PK (TS103) — 0041's lesson.
--
-- Retention: NONE. Granular-coverage mission keeps vault history
-- forever; compression after 30 days is the only ageing policy.
--
-- Historical fill: every event this table serves is ALREADY in the
-- ClickHouse lake from the protocol's genesis ledger (62,623,313) — it
-- was landed by the raw soroban_events path and simply never decoded.
-- Fill with
--   stellarindex-ops projector-replay -source upshift -from 62623313
-- per the replay decision rule in docs/architecture/ingest-pipeline.md
-- (upshift is a PROJECTED source: one writer, so `backfill` /
-- `ch-rebuild` are not the command here).

BEGIN;

CREATE TABLE upshift_vault_events (
    -- Emitting vault contract C-strkey. Always a member of the
    -- decoder's curated gate set (ADR-0035/0040) — a foreign contract's
    -- identical `deposit` topic never reaches this table.
    contract_id        text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            char(64)     NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    event_index        integer      NOT NULL CHECK (event_index >= 0),

    -- Which of the four decoded kinds this row is. Pinned to the
    -- Event* constants in internal/sources/upshift/events.go.
    event_kind         text         NOT NULL CHECK (event_kind IN (
        'deposit', 'withdraw', 'transfer', 'deployed_assets_changed')),

    -- topic[1]. The account that invoked the vault on deposit/withdraw,
    -- the `from` on a share transfer, the operator on
    -- deployed_assets_changed. Always present.
    caller             text         NOT NULL,

    -- topic[2]: who receives the underlying on a withdraw (the shares
    -- on a deposit), or the `to` on a share transfer. NULL on
    -- deployed_assets_changed, which carries only two topics.
    receiver           text,

    -- topic[3]: whose shares are minted or burned. NULL on transfer and
    -- deployed_assets_changed.
    --
    -- The (caller, receiver, owner) reading is the OpenZeppelin
    -- ERC-4626 `Withdraw` ordering. Exactly TWO events in the two
    -- vaults' combined history carry three DIFFERENT addresses, one per
    -- vault (earnXLM @ 63,812,795 and earnUSDC @ 63,812,816), and they
    -- corroborate each other: the SAME account sits in the middle slot
    -- of both while the outer contract differs — one holder redeeming
    -- both vaults 21 ledgers apart through a different router each time.
    -- Six ledgers before the earnUSDC one, that account had approved
    -- that contract and transferred it the shares, so the outer two are
    -- the caller/owner and the middle one is who received the
    -- underlying. Every `deposit` carries the three IDENTICAL, so
    -- deposit's reading is carried over from withdraw and is not
    -- independently proven — recorded here because a reader of these
    -- columns is entitled to know that.
    owner              text,

    -- Underlying moved in (deposit) or out (withdraw). NULL on transfer
    -- and deployed_assets_changed. Non-negative: the vault has never
    -- emitted a negative flow, and a negative one would be a schema
    -- change worth failing on rather than storing.
    assets             numeric      CHECK (assets IS NULL OR assets >= 0),

    -- Share-token delta: minted (deposit), burned (withdraw), moved
    -- (transfer). NULL on deployed_assets_changed. NOT on the same
    -- scale as `assets` — see the header note on the decimals offset.
    shares             numeric      CHECK (shares IS NULL OR shares >= 0),

    -- deployed_assets_changed only: the vault-level deployed-capital
    -- total before and after the change. NULL on every other kind.
    -- These are the DEPLOYED leg alone and are NOT the vault's total
    -- assets; the idle balance is not observable on-chain.
    old_amount         numeric,
    new_amount         numeric,

    -- INV-3 generation guard (migration 0110 convention): a corrected
    -- re-derive lands in place when its generation is >= the stored
    -- one; a live gen-0 replay can never revert it.
    derive_generation  bigint       NOT NULL DEFAULT 0,

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- Per-kind column population, enforced in the table rather than
    -- trusted from the decoder: a row that reaches here by any other
    -- path still cannot claim, say, a transfer that moved underlying.
    CONSTRAINT upshift_vault_events_kind_columns CHECK (
        CASE event_kind
            WHEN 'deposit'  THEN receiver IS NOT NULL AND owner IS NOT NULL
                             AND assets IS NOT NULL AND shares IS NOT NULL
                             AND old_amount IS NULL AND new_amount IS NULL
            WHEN 'withdraw' THEN receiver IS NOT NULL AND owner IS NOT NULL
                             AND assets IS NOT NULL AND shares IS NOT NULL
                             AND old_amount IS NULL AND new_amount IS NULL
            WHEN 'transfer' THEN receiver IS NOT NULL AND owner IS NULL
                             AND assets IS NULL AND shares IS NOT NULL
                             AND old_amount IS NULL AND new_amount IS NULL
            ELSE                  receiver IS NULL AND owner IS NULL
                             AND assets IS NULL AND shares IS NULL
                             AND old_amount IS NOT NULL AND new_amount IS NOT NULL
        END
    ),

    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE upshift_vault_events IS
    'Per-event Upshift tokenized-vault activity (deposit / withdraw / '
    'share transfer / deployed-assets change) for the curated pubnet '
    'vaults earnUSDC and earnXLM. Amounts are raw i128; assets and '
    'shares are on different scales (ERC-4626 decimals offset). NOT a '
    'TVL series — new_amount is the deployed leg only. See #503 and '
    'internal/sources/upshift/README.md.';
COMMENT ON COLUMN upshift_vault_events.shares IS
    'Share-token delta in the vault''s own units, NOT the underlying''s. '
    'Genesis-era deposits mint shares = assets x 1,000,000 exactly; the '
    'ratio drifts upward as the share price accrues.';
COMMENT ON COLUMN upshift_vault_events.new_amount IS
    'deployed_assets_changed only: vault-level capital DEPLOYED into '
    'strategies after the change. The idle balance is not observable '
    'on-chain, so this is never the vault''s total assets.';
COMMENT ON COLUMN upshift_vault_events.owner IS
    'topic[3] on deposit/withdraw. Ordering (caller, receiver, owner) is '
    'proven by the two withdrawals whose three topic addresses differ - '
    'earnXLM at ledger 63,812,795 and earnUSDC at 63,812,816, the same '
    'holder in the middle slot of both through a different router each.';

SELECT create_hypertable(
    'upshift_vault_events',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '30 days',
    if_not_exists       => TRUE
);

-- Per-vault walk ("this vault's activity, newest first") — the shape
-- every serving read and every share-supply roll-up takes.
CREATE INDEX upshift_vault_events_contract_ts_idx
    ON upshift_vault_events (contract_id, ledger_close_time DESC);

-- Per-kind cross-vault scan ("recent redemptions across both vaults") —
-- surfaces a withdrawal burst.
CREATE INDEX upshift_vault_events_kind_ts_idx
    ON upshift_vault_events (event_kind, ledger_close_time DESC);

-- Same-tx correlation (the vault event ↔ the SAC transfer that proves
-- the underlying, and the approve/transfer/withdraw redemption chain).
CREATE INDEX upshift_vault_events_tx_hash_idx
    ON upshift_vault_events (tx_hash, ledger_close_time DESC);

ALTER TABLE upshift_vault_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id, event_kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'upshift_vault_events',
    INTERVAL '30 days',
    if_not_exists => TRUE
);

COMMIT;
