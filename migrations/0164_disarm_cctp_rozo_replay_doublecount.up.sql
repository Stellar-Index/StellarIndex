-- 0164: disarm the cctp_events / rozo_events replay double-count
-- (T434, same shape as 0137's comet disarm).
--
-- Migration 0112 added event_index to cctp_events / rozo_events' PK and
-- backfilled existing rows to event_index=0, on the premise that a
-- prescribed re-ingest is the history-fix and needs no DELETE. That
-- premise is FALSE here for the same reason it was false for comet: both
-- dispatcher_adapter.go (internal/sources/cctp, internal/sources/rozo)
-- stamp EventIndex from the event's TRUE position in its op's
-- contract-event list, and cctp's own decoder doc says a single
-- deposit_for_burn op typically ALSO emits a message_sent event in the
-- same op — so a solo event commonly sits at a nonzero true index. A
-- catch-up projector-replay (internal/projector/registry.go: cctp.Event
-- and rozo.Event are both projected, ADR-0032) writes that event under
-- its TRUE (nonzero) event_index, a PK distinct from the surviving
-- legacy event_index=0 row for the SAME event, so every op landed
-- pre-0112 doubles rather than being corrected.
--
-- The disarm: drop every cctp_events / rozo_events row. Both are fully
-- re-derivable from the certified lake (cctp genesis 62,146,641; rozo
-- genesis 60,829,397 — internal/storage/timescale/per_source_gaps.go).
-- REQUIRED follow-up on any populated deployment, immediately after the
-- deploy that applies this migration:
--
--   stellarindex-ops projector-replay -config /etc/stellarindex.toml \
--     -source cctp -from 62146641 -write -refresh-caggs=false
--   stellarindex-ops projector-replay -config /etc/stellarindex.toml \
--     -source rozo -from 60829397 -write -refresh-caggs=false
--
-- Until each replay completes, reads of that source serve empty —
-- honest-absent, never double-counted. No other table is touched: the
-- cctp/rozo trade-adjacent tables (if any) key off a different PK and
-- were not part of the 0112 discriminator change.
--
-- Compression: both tables are compressed on r1 (measured 2026-09-28,
-- timescaledb 2.26.4: cctp_events 148,660 rows in 21 of 23 chunks
-- compressed, rozo_events 407 rows in 31 of 32; policy_compression jobs
-- 1063 and 1072 are scheduled), so this file's earlier "no compression
-- policy" claim was wrong. No decompress step is needed anyway: an
-- unqualified DELETE takes timescaledb's direct compressed-batch delete
-- (enable_compressed_direct_batch_delete, on by default), which drops
-- whole batches without decompressing them, so it does not count against
-- max_tuples_decompressed_per_dml_transaction (100,000). Pinned above the
-- cap on 2.26.4 by TestMigration0164_DeletesCompressedRowsAboveDMLDecompressCap.
--
-- The replays above need -write (projector-replay is a dry run without
-- it) and -refresh-caggs=false: neither source writes trades or oracle
-- rows, so the default cagg refresh would be a full-range price refresh
-- for nothing.

BEGIN;

DELETE FROM cctp_events;
DELETE FROM rozo_events;

COMMIT;
