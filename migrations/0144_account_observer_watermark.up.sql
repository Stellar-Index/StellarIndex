-- 0144 up — `account_observer_watermark`: the account observer's TRUE
-- processed-ledger watermark for the XLM circulating-supply freshness gate.
--
-- Why this exists (F-1320 / R-002 / CS-102 tail, live r1 root-cause).
-- The XLM freshness gate (internal/supply/refresher.go) rejects a snapshot
-- when snapshot_ledger − MinComponentLedger exceeds the dormancy horizon. For
-- native XLM that anchor comes from Store.MaxAccountObservationLedger, which
-- used to compute MAX(ledger) FROM account_observations. That table only gets
-- a row when a WATCHED account's balance CHANGES, so MAX(ledger) is the most
-- recent SDF-reserve balance change — NOT how far the observer has processed.
-- On r1 the 16 reserve accounts move every few days-to-weeks; during any quiet
-- period MAX(ledger) went stale while the observer was perfectly healthy, the
-- gap crossed DefaultMaxDormantComponentLedgers (~1 day), and the gate false-
-- rejected — firing a continuous supply_refresh_error_dominant ticket AND
-- freezing XLM's served as_of on a value that was actually current. It also
-- MASKED a genuinely-dead observer behind the same signal.
--
-- The fix is a TRUE watermark: the live indexer advances processed_ledger
-- every ledger it drives the account observer over, regardless of whether any
-- watched account changed. A healthy-but-quiet observer keeps the anchor fresh
-- (gate stays permissive, supply accepted as current); a genuinely dead
-- observer stops advancing it, the anchor freezes, and the gate correctly
-- fails closed past the horizon (dead-observer detection preserved).
--
-- Singleton by construction: one row for the whole account observer (there is
-- one observer, watching one operator-curated account set). id is pinned to 1
-- by a CHECK so a second row can never appear; the writer upserts ON CONFLICT
-- (id) with a monotonic-advance guard (never regresses processed_ledger). An
-- empty table (no watermark yet — fresh cluster before the first live tick)
-- reads as 0, the gate's existing permissive-bypass sentinel.
--
-- Deliberately a dedicated table, NOT a row in ingestion_cursors: that table
-- feeds LatestKnownLedger's MAX(), the density/coverage projection, the gap
-- detector and the cursor-lag alerts, none of which should see a synthetic
-- non-ingestion cursor. And NOT a per-ledger heartbeat row in
-- account_observations: that would bloat a ~16-account table by ~15k rows/day
-- for no value. O(1), isolated, additive + old-binary-safe (rule 9): the
-- previous released binary neither reads nor writes it, and
-- MaxAccountObservationLedger degrades to 0 (permissive) until the first tick.

BEGIN;

CREATE TABLE account_observer_watermark (
    id                smallint     PRIMARY KEY,
    processed_ledger  bigint       NOT NULL,
    updated_at        timestamptz  NOT NULL DEFAULT now(),

    -- One row only: the account observer is a singleton.
    CONSTRAINT account_observer_watermark_singleton CHECK (id = 1)
);

COMMENT ON TABLE account_observer_watermark IS
    'Singleton (id=1) TRUE watermark of the account observer''s processed-'
    'ledger progress (F-1320/R-002/CS-102). Advanced every live ledger by the '
    'indexer regardless of balance change; read by '
    'Store.MaxAccountObservationLedger as the XLM supply freshness anchor. A '
    'quiet observer keeps it fresh; a dead observer freezes it and the gate '
    'fails closed past the dormancy horizon.';

COMMIT;
