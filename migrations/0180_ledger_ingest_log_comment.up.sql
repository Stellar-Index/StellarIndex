-- 0180 up — correct the stored comment on ledger_ingest_log (GH #923).
--
-- 0051 told a `\d+` reader the row is "written post-persist", an
-- authoritative "this ledger is done" marker. It is not, and cannot be:
--
--   1. The live indexer writes it once pipeline.ProcessLedger returns,
--      i.e. once the ledger's events are ENQUEUED to the async sink
--      channel. The sink goroutines persist later, and a sink batch
--      failure after that point leaves the row in place.
--   2. `stellarindex-ops census-backfill` writes it straight from the
--      LedgerCloseMeta with no event persistence at all.
--   3. Projected domains (ADR-0032) are written by the projector from
--      the ClickHouse lake, independently of the indexer's ledger loop,
--      so no indexer-side acknowledgement could make the row mean
--      "the served rows are in Postgres".
--
-- What the row does prove is substrate continuity (contiguity + hash
-- chain) and the LCM-derived census counts. Persistence is proven only by
-- reconciling what should have been produced against the stored rows
-- (ADR-0033 Claims 2b/3: `stellarindex-ops verify-reconciliation`,
-- `compute-completeness`), never by this row's presence. persisted_at had
-- no stored comment; its inline note in 0051 also said "post-persist".
--
-- Why a migration: COMMENT ON strings live in pg_description, and 0051's
-- up body is immutable (migrations/README.md, "Amending a shipped
-- migration"; 0151 and 0173 are the precedents).
--
-- Catalog-only: no heap, index or chunk is touched, and nothing in Go
-- reads a catalog comment, so it is old-binary-safe (rule 9).

COMMENT ON TABLE ledger_ingest_log IS
    'Substrate-continuity record (ADR-0033 Claim 1). One row per ledger '
    'the indexer walked, written once its events were ENQUEUED to the '
    'async sink (or by census-backfill, from the LCM alone). NOT a '
    'persistence marker: rows may still be lost downstream, and projected '
    'domains are written later by the projector. Persistence is proven '
    'only by reconciliation against the stored rows (ADR-0033 Claims 2b/3), '
    'never by this row''s presence. soroban_event_count / '
    'classic_trade_effect_count are LCM-derived census counts. Contiguity '
    '+ hash-chain over this table is Claim 1.';

COMMENT ON COLUMN ledger_ingest_log.persisted_at IS
    'When this row was last written (after the ledger''s events were '
    'enqueued to the sink, or by census-backfill). Not a persistence '
    'acknowledgement for those events. Distinct from ledger_close_time, '
    'when the network closed the ledger.';
