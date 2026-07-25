-- 0116 up — remember the lowest ledger each projection target has ever
-- been verified from, so a bottom-edge truncation stops erasing its own
-- evidence (audit-2026-07-23 N-F2 residual).
--
-- compute-completeness derives each target's reconcile range from the
-- served tier's OWN data: `lo = MIN(ledger)` for that target
-- (internal/ops/chops/compute_completeness.go targetScope). That fixed a
-- real bug — the previous hardcoded `tip - 1_500_000` floor left
-- full-history tables unverified below ~100 days — but it left one
-- residual, and it is the nastiest shape a verifier can have: the floor
-- is derived from the very data it is meant to police.
--
-- If the OLDEST served rows are deleted, MIN(ledger) RISES with the loss,
-- the reconcile range follows it up, and the run reconciles the surviving
-- rows perfectly and reports complete. "Someone dropped the oldest 10M
-- ledgers of trades" and "we never projected below there" produce
-- byte-identical verdicts. The check cannot see the one event it exists
-- to catch.
--
-- The fix is a floor the served tier cannot rewrite: remember, per target,
-- the lowest ledger we have ever verified from, and treat a later MIN
-- ABOVE that as loss rather than as scope.
--
-- Why a new table rather than a column on completeness_snapshots: that
-- table is keyed per SOURCE, and the floor is per TARGET. Per-target is
-- the granularity the N-F2 fix deliberately moved to — one source's
-- targets legitimately begin at different ledgers (soroswap/sdex trades
-- start ~61.5M while the same source's other tables run to genesis), so a
-- source-level floor would either false-positive on the late-starting
-- target or be dragged down to uselessness by the early one.
--
-- The key is (source, target_table, target_filter) because a single table
-- is split across sources by whereFilter — `trades` carries several
-- sources' rows — so target_table alone is not unique, and two sources'
-- slices of one table can honestly have different true floors.
--
-- projection_verified_from is monotonically NON-INCREASING: the writer
-- applies LEAST(existing, new). A run that reconciles from a LOWER ledger
-- than we have seen before is new ground and lowers the floor; a run that
-- starts higher is either loss (which must fail, not overwrite) or a
-- narrowed `-from` window (which must not be recorded as the new truth).
-- Making the column only ever fall is what stops a partial or
-- deliberately-narrowed run from quietly ratcheting the floor upward and
-- re-creating the bug this migration exists to close — the same
-- regressive-run hazard CS-083 guards on completeness_snapshots.
--
-- NOTE ON RETENTION: this rule is unconditional because NO reconcile
-- target has a retention policy. Migration 0031 removed it from trades /
-- prices_1m / prices_15m, 0040 removed it from oracle_updates, and the
-- only surviving add_retention_policy in the tree is api_usage_events
-- (0027, 12 months), which is not a reconcile target. Verified against
-- r1's live timescaledb_information.jobs on 2026-07-25. If a retention
-- policy is ever added to a reconcile target, a rising MIN becomes
-- legitimate for that target and this rule must be revisited FIRST —
-- otherwise it will fire continuously and get muted, which is worse than
-- not having it.
--
-- Additive: a new table, no existing column touched, so an old binary
-- ignores it entirely (migrations/README.md rule 9).

BEGIN;

CREATE TABLE IF NOT EXISTS completeness_target_floors (
    source                   text        NOT NULL,
    target_table             text        NOT NULL,
    target_filter            text        NOT NULL DEFAULT '',
    projection_verified_from bigint      NOT NULL,
    first_recorded_at        timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source, target_table, target_filter)
);

COMMENT ON TABLE completeness_target_floors IS
    'Durable per-target projection floor (ADR-0033, audit N-F2 residual). '
    'projection_verified_from is the LOWEST ledger this target has ever '
    'been reconciled from; the writer applies LEAST() so it only ever '
    'falls. compute-completeness fails the projection axis when a '
    'target''s live MIN(ledger) sits ABOVE this floor, which is '
    'served-tier loss below the verified range — the case a floor '
    'derived from MIN(ledger) alone cannot distinguish from "never '
    'projected".';

COMMENT ON COLUMN completeness_target_floors.target_filter IS
    'reconTarget.whereFilter, '''' when the whole table belongs to the '
    'source. Part of the key because one table (trades) is split across '
    'sources, and those slices can honestly have different true floors.';

COMMIT;
