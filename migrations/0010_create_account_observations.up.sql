-- 0010 up — `account_observations` hypertable.
--
-- Stores one row per AccountEntry-delta touching an operator-
-- configured G-strkey. Per ADR-0021, this table backs two
-- readers — metadata.LCMHomeDomainResolver (replaces the static
-- [metadata.issuer_home_domains] map) and supply.LCMReserveBalanceReader
-- (replaces the static [supply.reserve_balances_stroops] map).
-- The static config blocks remain in tree as bootstrap fallbacks.
--
-- Identity: (account_id, ledger). Multiple changes to the same
-- account in a single ledger (fee + op, multi-op tx) write
-- conflicting rows for the same PK; we keep the LAST one
-- (last-writer-wins via ON CONFLICT DO UPDATE) since the
-- AccountEntry post-state is monotonic within a ledger and the
-- final state is what readers want.
--
-- Retention: NONE for now. The watched-set is small (single-digit
-- accounts at v1) and observations are infrequent (a few per day
-- per account). Indefinite retention is the default; if the table
-- grows to multi-million rows over years, a retention policy
-- lands in a follow-up migration.

BEGIN;

CREATE TABLE account_observations (
    -- G-strkey of the observed account. Per ADR-0021 the observer
    -- only fires on operator-configured watched accounts, so this
    -- column carries G-strkeys exclusively (no muxed-account
    -- variants — those don't appear at the AccountEntry level).
    account_id        text         NOT NULL,

    -- Ledger sequence at which the delta landed.
    ledger            integer      NOT NULL CHECK (ledger >= 0),

    -- Ledger close time, UTC. Hypertable partition column.
    observed_at       timestamptz  NOT NULL,

    -- Post-change native XLM balance in stroops. NUMERIC per
    -- ADR-0003 — XLM is i64 in XDR but we carry NUMERIC end-to-end
    -- for consistency and future-proofing if Stellar widens the
    -- amount type.
    balance_stroops   numeric      NOT NULL,

    -- AccountEntry.HomeDomain (max 32 bytes per protocol). NULL
    -- when the account has no home_domain set; consumers handle
    -- nil explicitly per ADR-0011 ("we don't fabricate"). NOT
    -- represented as empty-string-equals-no-domain because that
    -- ambiguity would conflate "operator hasn't set it" with
    -- "operator set an empty string."
    home_domain       text,

    -- AccountEntry.Flags bitmask. Auth* bits relevant for SEP-1
    -- issuer overlay; metadata layer cross-checks against this.
    flags             integer      NOT NULL DEFAULT 0,

    -- AccountEntry.SeqNum after the change. Carried for cross-
    -- check / debug; readers should rely on observed_at + ledger
    -- for ordering rather than seq_num.
    seq_num           bigint       NOT NULL DEFAULT 0,

    -- True when the change removed the AccountEntry. Balance is 0
    -- and home_domain is NULL on removal rows; the reader treats
    -- is_removal=true as "account no longer exists at this ledger."
    is_removal        boolean      NOT NULL DEFAULT false,

    ingested_at       timestamptz  NOT NULL DEFAULT now(),

    -- Hypertable partition column must be in the PK; (account_id,
    -- ledger) is the natural identity, observed_at is dragged
    -- along to satisfy the partition-column-in-PK rule.
    PRIMARY KEY (account_id, ledger, observed_at)
);

COMMENT ON TABLE account_observations IS
    'Per-(account, ledger) AccountEntry deltas observed by '
    'internal/sources/accounts/. Backs the supply + metadata '
    'live readers per ADR-0021. Hypertable partitioned on observed_at.';

COMMENT ON COLUMN account_observations.balance_stroops IS
    'Post-change native XLM balance in stroops. NUMERIC per ADR-0003.';
COMMENT ON COLUMN account_observations.home_domain IS
    'AccountEntry.HomeDomain (max 32 bytes). NULL when unset; '
    'metadata.LCMHomeDomainResolver maps NULL → ("", false, nil).';
COMMENT ON COLUMN account_observations.is_removal IS
    'True when the change was Removed-variant. Balance is 0 and '
    'home_domain is NULL on these rows.';

-- Hypertable. 7-day chunks — per-account observations are
-- sparse (a few per day per watched account) so a 1-day chunk
-- would create lots of small chunks. 7 days strikes the balance
-- between chunk-count and chunk-size.
SELECT create_hypertable(
    'account_observations',
    'observed_at',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Most-common reader query: "what's the most recent observation
-- for this account at-or-before ledger N?" The
-- (account_id, observed_at DESC) index makes that an index-only
-- scan from the start of the partition.
CREATE INDEX account_observations_account_observed_idx
    ON account_observations (account_id, observed_at DESC);

-- Replay / debug — walk observations by ledger across all watched
-- accounts ("show me every account that changed in ledger N").
CREATE INDEX account_observations_ledger_idx
    ON account_observations (ledger DESC);

-- Compression — group by account_id (most queries are
-- per-account) and order by observed_at DESC within each chunk.
ALTER TABLE account_observations SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'account_id',
    timescaledb.compress_orderby   = 'observed_at DESC, ledger DESC'
);

COMMIT;
