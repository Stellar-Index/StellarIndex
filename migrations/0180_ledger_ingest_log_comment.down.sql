-- 0180 down — restore 0051's table comment verbatim
-- (0051_ledger_ingest_log.up.sql) and remove the persisted_at comment,
-- which 0051 never set, so `migrate down` leaves pg_description as it
-- was before 0180.

COMMENT ON TABLE ledger_ingest_log IS
    'Substrate-continuity record (ADR-0033). One row per fully-'
    'processed ledger, written post-persist. soroban_event_count / '
    'classic_trade_effect_count are LCM-derived checksums reconciled '
    'against soroban_events / trades. Contiguity + hash-chain over '
    'this table is Claim 1 of the completeness model.';

COMMENT ON COLUMN ledger_ingest_log.persisted_at IS NULL;
