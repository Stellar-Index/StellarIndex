-- 0150 up — `trades.signer`: the transaction source account (fee-payer)
-- behind an AMM/Soroban swap.
--
-- WHY: the AMM decoders (comet/soroswap/aquarius/phoenix) set `taker` to
-- the on-chain caller (the event's to/sender/caller/user) and leave
-- `maker` empty — so a router/contract-driven swap has no human/EOA
-- attribution (the taker is the router contract). The tx source account
-- is that missing initiator, and it is NOT re-derivable from the lake
-- events the projector replays (ClickHouse contract_events + Postgres
-- soroban_events carry no per-event source account), so it cannot be set
-- on the decode path. It IS in the lake's `stellar.transactions`
-- (source_account, keyed by ledger + tx_hash), so a POST-INSERT tagger
-- (internal/pipeline signer sweeper) back-tags it — exactly the
-- `routed_via` (migration 0025) pattern.
--
-- Nullable + no default = O(1), no rewrite, no lock on the existing
-- multi-billion-row hypertable (proven by 0025's routed_via ADD COLUMN on
-- the same compression-enabled table). The column is DELIBERATELY absent
-- from the trades INSERT / ON CONFLICT DO UPDATE (internal/storage/
-- timescale/trades.go) so the projector's ~5s re-derive UPSERT cannot
-- clobber a tagged value to NULL — first-wins, just like routed_via.
--
-- NULL = not (yet) tagged, or a non-AMM/non-Soroban trade the tagger
-- does not touch (SDEX already carries a real maker; classic trades have
-- no contract tx to attribute).
--
-- ── APPLYING TO AN ALREADY-POPULATED DATABASE ──
-- Unlike 0037/0123, `trades_signer_idx` below is a plain in-transaction
-- CREATE INDEX with no `IF NOT EXISTS` and no `SET LOCAL lock_timeout`
-- (it predates README rule 10; a shipped up body cannot change). On a
-- populated node it holds a SHARE lock that blocks every write to
-- `trades` for a full-table scan (a partial index still reads every
-- row), and it waits indefinitely behind any open transaction while
-- every later writer queues behind it.
--
-- Do NOT pre-build it: hypertables reject CONCURRENTLY, and a per-chunk
-- build (`WITH (timescaledb.transaction_per_chunk)`, 0123's recipe)
-- under the same name makes this migration fail on the duplicate and
-- leaves schema_migrations dirty. Instead apply 0150 with
-- ingest writers stopped, after checking pg_stat_activity for
-- long-open transactions.
-- A fresh database pays nothing (`trades` is empty).

BEGIN;

ALTER TABLE trades ADD COLUMN signer text;

COMMENT ON COLUMN trades.signer IS
    'Transaction source account (fee-payer / initiator) behind an '
    'AMM/Soroban swap, back-tagged post-insert from stellar.transactions '
    '(the lake events the projector replays carry no source account). '
    'NULL = untagged or not an AMM/Soroban trade. First-wins; kept out of '
    'the trades UPSERT DO-UPDATE so re-derive cannot clobber it.';

-- Partial index — most trades are non-AMM (no signer), so index only the
-- tagged rows to keep it small (mirrors trades_routed_via_idx).
CREATE INDEX trades_signer_idx ON trades (signer)
    WHERE signer IS NOT NULL;

COMMIT;
