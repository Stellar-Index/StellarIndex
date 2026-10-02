-- 0196 up — `trades.tx_index`: the 0-based intra-ledger apply order of the
-- transaction behind an on-chain trade.
--
-- WHY: same-ledger trades of one pair have no execution order in `trades`;
-- every reader breaks the tie on tx_hash, which is not apply order. Apply
-- order lives only in the lake (stellar.transactions.tx_index, mirrored into
-- stellar.tx_hash_index), so a POST-INSERT tagger fills it — the 0150
-- `signer` pattern: pipeline.RunTxIndexTagger (live, trailing window) and
-- `stellarindex-ops tag-tx-index` (historical windows), both through
-- timescale.Store.TagTradesTxIndex. No reader keys on it yet.
--
-- Nullable + no default = catalog-only, including on compressed chunks:
-- test/integration/migration_0196_trades_tx_index_test.go asserts every
-- chunk is still compressed after this file. No index and no CHECK (a
-- CHECK would validate against compressed chunks).
--
-- Lock: ACCESS EXCLUSIVE on trades and its chunk catalog entries —
-- sub-second of work, but it queues behind any long reader, a
-- compression-policy run or a CAGG refresh, and every new trades query
-- queues behind it. No SET LOCAL lock_timeout here: the deploy runs
-- migrate under PGOPTIONS lock_timeout. If it trips, schema_migrations is
-- left DIRTY at 196 with the DDL rolled back; recover with
-- `stellarindex-migrate force 195` and re-run in a quiet window.
--
-- Old-binary-safe (rule 9): the column is in no INSERT or ON CONFLICT
-- DO UPDATE list, so the previous binary writes NULL and a re-derive
-- UPSERT never clobbers a tagged value.
--
-- NULL = not yet tagged, or an off-chain row (ledger = 0). Historical fill,
-- before any reader keys on the column:
--   stellarindex-ops tag-tx-index -config PATH -from T0 -to T1 -write

BEGIN;

ALTER TABLE trades ADD COLUMN tx_index integer;

COMMENT ON COLUMN trades.tx_index IS
    '0-based intra-ledger transaction apply order, back-tagged post-insert '
    'from the lake (stellar.tx_hash_index). NULL = untagged or off-chain '
    '(ledger = 0). First-wins; kept out of the trades UPSERT so re-derive '
    'cannot clobber it.';

COMMIT;
