-- sep41_transfers has no ledger-led index (unlike sep41_supply_events,
-- which has sep41_supply_events_ledger_idx from 0015), so the ADR-0033
-- projection reconcile's windowed CountRowsByLedger
-- (WHERE ledger BETWEEN … GROUP BY ledger) seq-scans the whole
-- hypertable and hits statement_timeout — found 2026-07-06 when the
-- first-ever sep41_transfers verdict tail timed out and the daily
-- 25k-chunk refresh would have timed out every day thereafter.
-- IF NOT EXISTS because r1 gets this DDL applied by hand (CONCURRENTLY)
-- ahead of the migration run.
CREATE INDEX IF NOT EXISTS sep41_transfers_ledger_idx
    ON sep41_transfers (ledger DESC);
