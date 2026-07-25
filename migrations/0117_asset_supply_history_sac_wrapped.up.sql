-- 0117 up — persist Algorithm 2's SACWrapped component on
-- `asset_supply_history` so the supply cross-check can run the REAL
-- subset compare instead of only the total-vs-total tripwire
-- (audit E4 / N-F3(b); the "(b) follow-up" BACKLOG #59 left unbuilt).
--
-- ── What was missing ──
--
-- Algorithm 2 (classic) derives
--     total = Trustline + Claimable + LPReserve + SACWrapped
-- (internal/supply.ClassicSupplyComponents). Algorithm 3 (SEP-41)
-- derives the SAC wrapper's own total as Σmint − Σburn − Σclawback.
--
-- SACWrapped and the Algorithm-3 total measure THE SAME QUANTITY — the
-- amount of the classic asset currently escrowed inside the SAC — via
-- two independent data paths: a ledger-entry snapshot sum
-- (`sac_balance_observations`) versus an event-flow sum
-- (`sep41_supply_events` / the certified lake's supply_flows). That is
-- the comparison worth making.
--
-- It could not be made, because `ClassicComputer.Compute` FOLDED
-- SACWrapped into `total_supply` before returning, and only the folded
-- total was persisted here. The cross-check refresher reads this table
-- (`SnapshotReader.LatestSupply`), so the component was gone by the time
-- the comparison ran. `internal/supply/crosscheck.go` named the two ways
-- to fix that; this migration is the first half of option (1), the
-- cleaner one: keep the component on the snapshot.
--
-- ── Why NULLable with no DEFAULT (load-bearing, not laziness) ──
--
-- NULL means "this snapshot did not record a SACWrapped component" —
-- either a row written before this migration, or a snapshot from an
-- algorithm that has no such component (Algorithm 1 XLM, Algorithm 3
-- SEP-41, the static text-file computer). The cross-check reads NULL as
-- the CS-087 "unchecked" state and DECLINES to evaluate the new leg,
-- reporting `subset_bound_checked=false`.
--
-- `NOT NULL DEFAULT 0` would be actively WRONG here, which is why it is
-- not used despite being the shape 0109/0114 chose for their columns:
-- zero is a MEANINGFUL value (an asset with no SAC deployment genuinely
-- wraps nothing), so defaulting legacy rows to 0 would assert
-- "SACWrapped = 0" for every historical snapshot. The new leg checks
-- SACWrapped <= sac_total, and 0 <= anything, so every legacy row would
-- pass VACUOUSLY and the check would report green having verified
-- nothing — precisely the fail-open CS-087 exists to prevent.
--
-- Old-binary-safe (README rule 9): the currently-deployed binary's
-- INSERT column list does not mention `sac_wrapped_stroops`, so a row it
-- writes takes SQL NULL, which the reader already interprets as
-- "unchecked". A nullable ADD COLUMN with no default is metadata-only on
-- PostgreSQL and needs no chunk rewrite, so this is cheap even though
-- `asset_supply_history` is a compressed hypertable (compression policy
-- from 0005; r1 runs TimescaleDB 2.26 — same direct-ALTER support 0109
-- and 0114 rely on).
--
-- ── No CHECK constraint, deliberately ──
--
-- The natural instinct is `CHECK (sac_wrapped_stroops >= 0)`, matching
-- the 0005 column CHECKs. It is deliberately omitted:
--
--   * Adding a constraint to a COMPRESSED hypertable needs the
--     decompress-every-chunk dance migration 0101 had to perform, on a
--     table with years of chunks, inside the deploy's pre-tasks. That is
--     a real operational hazard for a guard that has no reachable
--     violation.
--   * The value cannot be negative by construction: the reader's
--     `validateClassicComponents` (internal/supply/classic.go) REJECTS a
--     negative SACWrapped and fails `Compute` before any Supply is
--     returned, and `Store.InsertSupply` re-guards non-negativity at the
--     write boundary with an explicit error.
--   * Rule 9 classes `ADD CONSTRAINT` as a tightening.
--
-- If a CHECK is ever wanted, it belongs in its own migration, after a
-- backfill audit, not bundled with the additive column.
--
-- ── Backfill ──
--
-- None, and none is possible from within Postgres: SACWrapped is not
-- recoverable from `total_supply` alone. Existing rows stay NULL; the
-- column self-populates from the next aggregator refresh cycle onward
-- (the per-asset Refresher writes a fresh snapshot every bucket close),
-- so the new cross-check leg starts reporting `checked` for a
-- classic/SAC pair within one refresh interval of deploy. Until then it
-- reports `unchecked` — honestly, rather than green.

BEGIN;

ALTER TABLE asset_supply_history
    ADD COLUMN sac_wrapped_stroops NUMERIC;

COMMENT ON COLUMN asset_supply_history.sac_wrapped_stroops IS
    'Algorithm 2''s SACWrapped component in stroops — the amount of this '
    'classic asset escrowed inside its Stellar-Asset-Contract wrapper, '
    'broken out of total_supply so the supply cross-check can compare it '
    'against the SAC''s own Algorithm-3 total (audit E4/N-F3(b), '
    'BACKLOG #59 follow-up (b)). NULL means the snapshot recorded no '
    'component — a pre-0117 row, or a non-classic algorithm (XLM / '
    'SEP-41 / static) that has none. NULL is the CS-087 "unchecked" '
    'state and MUST NOT be read as zero: the cross-check declines to '
    'evaluate the SACWrapped <= sac_total leg rather than passing it '
    'vacuously.';

COMMIT;
