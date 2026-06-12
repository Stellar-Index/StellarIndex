-- 0035 up — `source_entry_counts`: an always-on, per-source running
-- tally of ingested entries (trades + oracle_updates).
--
-- Why this exists: /v1/diagnostics/ingestion's coverage snapshot
-- needs a per-source "how much have we ingested" number that is
-- cheap to read EVEN DURING an all-time backfill. The prior path
-- (BackfillCoverageStats, a MIN/MAX/COUNT scan over the `trades`
-- hypertable) is exactly the IO-contended query rc.53 decoupled
-- density from — during backfill it never completes, so the count
-- column fell back to a misleading 0 for every source. And it was
-- structurally wrong for oracle sources, which write to
-- `oracle_updates`, never `trades`.
--
-- This table is a tiny key-value tally (~one row per source, ~20
-- rows) so a read is O(20) regardless of trades/oracle_updates
-- size. The writers (InsertTrade / InsertOracleUpdate) bump the
-- counter ATOMICALLY in the same statement as the row insert, via a
-- data-modifying CTE gated on the insert actually adding a row
-- (`ON CONFLICT DO NOTHING` → 0 rows → no bump). That makes the
-- counter idempotent under backfill re-walks: replaying a ledger
-- range whose trades already exist does not inflate the tally.
--
-- Authoritative reconciliation: `stellarindex-ops seed-entry-counts`
-- recomputes the tally from a full GROUP BY over trades +
-- oracle_updates and overwrites this table. Run it once after the
-- all-time backfill completes to absorb (a) pre-counter history and
-- (b) any O(process-crash) increment drift. Between reseeds the
-- counter is exact for everything ingested since the last seed.
--
-- NOT a hypertable: it has no time dimension and is keyed by source.

BEGIN;

CREATE TABLE source_entry_counts (
    source       text        PRIMARY KEY,
    entry_count  bigint      NOT NULL DEFAULT 0 CHECK (entry_count >= 0),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE source_entry_counts IS
    'Per-source running tally of ingested entries (trades + '
    'oracle_updates). Bumped atomically + idempotently by the '
    'writers; authoritatively reseeded by stellarindex-ops '
    'seed-entry-counts. Powers the `entries` column on '
    '/v1/diagnostics/ingestion. See migration 0035.';

COMMIT;
