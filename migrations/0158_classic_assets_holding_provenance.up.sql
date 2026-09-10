-- 0158 up — split `classic_assets` observation provenance into TRADE and
-- HOLDING, ahead of the registry gaining a holdings-fed writer.
--
-- WHAT WAS WRONG
--
-- Migration 0023 documented `classic_assets` as "auto-populated by an
-- observer that hooks every trade + every ChangeTrust op + every payment
-- crossing an issuer". Only the trade half was ever built:
-- `registerClassicAssetSeen` has two call sites and both are inside
-- InsertTrade / BatchInsertTrades. An asset that is HELD but never traded
-- on the SDEX therefore has no row here — and, because `issuers` is
-- written only from inside that same function, no issuer row either, so
-- no SEP-1 fetch and no RWA candidacy.
--
-- Measured on the production lake, 2026-09-10:
--
--     distinct classic assets with a trustline   512,496
--     rows in classic_assets                     199,793
--     ------------------------------------------ -------
--     absent                                     312,703  (61%)
--
-- The presenting case is Franklin Templeton's BENJI
-- (BENJI-GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5,
-- 12,498 trustlines, more than all eighteen impersonating BENJIs
-- combined): a money-market fund, bought and held rather than traded, so
-- it never produced the trade that would have registered it.
--
-- WHAT THIS MIGRATION DOES, AND WHAT IT DELIBERATELY DOES NOT
--
-- It does NOT insert the missing rows — that is `stellarindex-ops
-- asset-registry-backfill`, which reads the lake. It prepares the columns
-- so that when those rows land, nothing that already reads this table
-- starts quietly answering a different question.
--
-- Every value in `first_seen_*` / `last_seen_*` / `observation_count`
-- TODAY is a trade observation, by construction: the trade hook is the
-- only writer that has ever existed. Those three facts are copied into
-- explicitly-named columns before a second source can touch them, so the
-- trade-only reading of the registry stays available and exact rather
-- than being reconstructed later from a table that has since been mixed.
--
--   first_trade_*  / last_trade_*    what `*_seen_*` has meant so far.
--                                    Written only by the trade hook.
--                                    NULL = never traded (only possible
--                                    on rows the holdings path created).
--
--   first_holding_* / last_holding_* the holdings-scan provenance.
--                                    Written only by the holdings path.
--                                    NULL = no trustline evidence yet
--                                    (every row predating the backfill).
--
--   first_seen_*   / last_seen_*     become the UNION over both sources —
--                                    the earliest / latest ledger at which
--                                    we hold ANY evidence of the asset.
--                                    That is what 0023's own docblock
--                                    said they were; it is only the
--                                    implementation that was trade-only.
--                                    Widening them can move first_seen_*
--                                    EARLIER and last_seen_* LATER, never
--                                    the reverse, so the pair remains an
--                                    upper bound on the asset's genesis
--                                    ledger exactly as before.
--
--   observation_count                UNCHANGED — it stays a count of TRADE
--                                    observations. A holdings-derived row
--                                    is inserted with 0 and the holdings
--                                    path never increments it. A reader
--                                    ranking by it is ranking by trading
--                                    activity, which is what it has always
--                                    done and what the explorer's
--                                    "Observations" column claims.
--
-- WHY NOT NULLABLE-EVERYTHING INSTEAD
--
-- Relaxing `first_seen_at NOT NULL` so a holdings-only row could carry
-- NULLs would push a three-valued case into every existing reader of this
-- table at once. Keeping `*_seen_*` total and adding the provenance
-- alongside means each reader can be moved deliberately, and a reader
-- left alone keeps working on a strictly better-informed value.
--
-- HONESTY NOTE ON THE HOLDING LEDGER
--
-- `first_holding_ledger` is the earliest ledger at which a holdings scan
-- has OBSERVED a trustline for the asset, not the ledger the asset was
-- created in. `stellar.ledger_entries_current` holds the current state of
-- each entry, so its `ledger_seq` is that entry's last modification. The
-- value is therefore an upper bound on the asset's genesis, and it
-- improves (moves earlier) as later scans find older-modified trustlines,
-- because the writer takes LEAST. It must never be read as a genesis
-- ledger. Neither must `first_seen_ledger`, which has always been the same
-- kind of bound.
--
-- Rule 9 (old-binary safety): every column added here is nullable with no
-- default, and the only data written is a copy of columns that already
-- exist. The previous binary neither reads nor writes any of them, and
-- its INSERT / ON CONFLICT statement on this table is unaffected.
--
-- Runtime: eight ADD COLUMNs (catalog-only on PG 11+) plus one UPDATE over
-- ~200k rows of a plain, non-hypertable, uncompressed table — seconds.

BEGIN;

ALTER TABLE classic_assets
    ADD COLUMN first_trade_at       timestamptz,
    ADD COLUMN first_trade_ledger   integer CHECK (first_trade_ledger   IS NULL OR first_trade_ledger   >= 0),
    ADD COLUMN last_trade_at        timestamptz,
    ADD COLUMN last_trade_ledger    integer CHECK (last_trade_ledger    IS NULL OR last_trade_ledger    >= 0),
    ADD COLUMN first_holding_at     timestamptz,
    ADD COLUMN first_holding_ledger integer CHECK (first_holding_ledger IS NULL OR first_holding_ledger >= 0),
    ADD COLUMN last_holding_at      timestamptz,
    ADD COLUMN last_holding_ledger  integer CHECK (last_holding_ledger  IS NULL OR last_holding_ledger  >= 0);

-- Every existing row is trade-derived: the trade hook is the only writer
-- this table has ever had. Copying rather than recomputing is what makes
-- that claim exact — there is no trade scan here, and none is needed.
UPDATE classic_assets
   SET first_trade_at     = first_seen_at,
       first_trade_ledger = first_seen_ledger,
       last_trade_at      = last_seen_at,
       last_trade_ledger  = last_seen_ledger;

-- Serves the backfill's own coverage question ("which registered assets
-- have no holdings evidence yet?") and the API's never-traded filter,
-- both of which are `IS NULL` scans over a table heading for ~512k rows.
CREATE INDEX classic_assets_no_holding_idx
    ON classic_assets (asset_id) WHERE last_holding_at IS NULL;
CREATE INDEX classic_assets_no_trade_idx
    ON classic_assets (asset_id) WHERE last_trade_at IS NULL;

COMMENT ON COLUMN classic_assets.first_seen_at IS
    'Earliest observation of this asset from ANY source (trade or trustline holding). Upper bound on the asset genesis ledger, never the genesis itself.';
COMMENT ON COLUMN classic_assets.first_seen_ledger IS
    'Ledger of first_seen_at. Union over sources; see first_trade_ledger / first_holding_ledger for the per-source values.';
COMMENT ON COLUMN classic_assets.last_seen_at IS
    'Latest observation of this asset from ANY source. NOT "last traded" — read last_trade_at for that.';
COMMENT ON COLUMN classic_assets.last_seen_ledger IS
    'Ledger of last_seen_at. Union over sources.';
COMMENT ON COLUMN classic_assets.observation_count IS
    'Count of TRADE observations only. A holdings-derived row carries 0 and the holdings writer never increments it, so this stays a trading-activity proxy.';
COMMENT ON COLUMN classic_assets.first_trade_at IS
    'First trade observed for this asset. NULL = never traded on the SDEX.';
COMMENT ON COLUMN classic_assets.first_trade_ledger IS
    'Ledger of first_trade_at.';
COMMENT ON COLUMN classic_assets.last_trade_at IS
    'Most recent trade observed for this asset. NULL = never traded on the SDEX.';
COMMENT ON COLUMN classic_assets.last_trade_ledger IS
    'Ledger of last_trade_at.';
COMMENT ON COLUMN classic_assets.first_holding_at IS
    'Close time of the earliest ledger at which a holdings scan observed a trustline for this asset. NULL = no trustline evidence yet.';
COMMENT ON COLUMN classic_assets.first_holding_ledger IS
    'Ledger of first_holding_at. An entry ledger_seq is that entry last modification, so this is an upper bound on genesis that improves as later scans find older trustlines.';
COMMENT ON COLUMN classic_assets.last_holding_at IS
    'Close time of the latest ledger at which a holdings scan observed a trustline for this asset. NULL = no trustline evidence yet.';
COMMENT ON COLUMN classic_assets.last_holding_ledger IS
    'Ledger of last_holding_at.';

COMMENT ON TABLE classic_assets IS
    'Auto-populated registry of every classic asset observed on Stellar. '
    'One row per (code, issuer) — never a bare code; slug is the URL-safe '
    'identifier used by /assets/{slug}. Two observation sources, kept '
    'apart: trades (the indexer hot path) and trustline holdings (the '
    'asset-registry-backfill ops job, reading the ClickHouse lake). '
    'observation_count counts TRADES ONLY.';

COMMIT;
