-- 0163 up — give the durable ADR-0019 freeze ladder a WINDOW dimension.
--
-- What exists today. Migration 0119 put the freeze lifecycle on the pair's
-- open `freeze_events` row — hold_until / extensions_used / escalated /
-- corroborated — so the ladder survives losing Redis. Those four columns
-- are keyed the way the row is: (asset_id, quote_id). One ladder per pair.
--
-- The lifecycle is not per pair. The aggregator runs one independent
-- ADR-0019 state machine per (pair, WINDOW) — 5m, 1h and 24h by default —
-- and every frozen window mirrors its ladder here on every tick. So the
-- durable record is simply whichever window wrote LAST, with nothing to say
-- whose it is.
--
-- What that costs. The durable ladder is read at exactly one moment: after
-- Redis has lost the `freeze:<asset>:<quote>` marker, when it is the only
-- authority left. At that moment:
--
--   * a 1h window that has spent the whole 2-hour ladder and ESCALATED
--     ("stays active until manual unfreeze") is rehydrated from a 5m
--     sibling's fresh ten-minute, zero-extension ladder — and resumes
--     auto-unfreezing a price a P1 already put in front of a human. This is
--     the dangerous direction;
--   * every OTHER window of the pair rehydrates the same ladder, so windows
--     nothing was wrong with come back frozen, walk the ladder on evidence
--     that was never about them, and escalate.
--
-- The Redis marker was given one ladder per window for the same reason; this
-- is that fix's durable twin, and it deliberately takes the same shape: a
-- per-window map on the PAIR's record, not a row per window.
--
-- Why a map on the open row and not a `window_seconds` key column. A row
-- per (pair, window) changes what a `freeze_events` row MEANS. One row is
-- one freeze event: it is what /v1/anomalies lists, what fires the
-- `anomaly.freeze` customer webhook, what RecordFreeze de-duplicates on
-- (F-1250), what the recovery worker closes and what
-- `stellarindex-ops freeze-unfreeze` stamps. Keying rows by window would
-- triple every one of those for a single market event — three timeline
-- entries, three webhook deliveries — to carry state none of those readers
-- want. The window dimension belongs to the LADDER, so it lives with the
-- ladder.
--
--   window_ladders   jsonb object, one entry per window that currently
--                    holds a ladder. Key = the window in whole seconds as
--                    a decimal string ("300", "3600", "86400"). Value = the
--                    lifecycle state, field-for-field `freeze.State`:
--                    fired_at, hold_until, extensions_used, escalated,
--                    unfreeze_streak, corroborated.
--                    Key "0" is the UNOWNED ladder — see below.
--
-- The 0119 columns stay, and stay meaningful: the writer keeps them as the
-- fail-closed SUMMARY of the per-window entries (furthest hold_until,
-- highest extensions_used, escalated if ANY window is). That is what keeps
-- this additive in both directions:
--
--   * the recovery worker and `freeze-unfreeze -list` read the pair-level
--     view and need no change — "is any window of this pair still inside a
--     hold" is exactly what the summary answers;
--   * the previous released binary neither reads nor writes the new column
--     and keeps working (migrations/README.md rule 9). It sees the summary,
--     which can only over-state a freeze, never drop one.
--
-- hold_until remains the master switch. A reader honours window_ladders
-- only on an OPEN row whose hold_until IS NOT NULL. Retiring the ladder
-- (auto-release of the last frozen window, or the operator override) nulls
-- hold_until exactly as before, and a previous binary that does so without
-- knowing about window_ladders still switches the whole record off.
--
-- Rows written before this migration read window_ladders IS NULL. Their
-- pair-level ladder has no recorded owner, so it is treated as the UNOWNED
-- ladder and keeps rehydrating onto EVERY window — the pre-0163 behaviour,
-- kept on purpose: narrowing "owner unknown" to "nobody's" would release a
-- freeze that is still running. The first window-aware write preserves it
-- under key "0"; it ages out on the same hold-plus-grace bound as every
-- other durable ladder.
--
-- Rollback note. A roll BACK to the previous binary leaves this column in
-- place (migrations/README.md rule 9), and that binary neither reads nor
-- writes it: it keeps ADVANCING the four pair-level columns and leaves
-- window_ladders exactly as the new binary last wrote it. After a roll
-- FORWARD inside the same freeze the map is therefore STALE, in two ways:
--
--   * it can be BEHIND. The old binary extends or ESCALATES the freeze in
--     the pair-level columns only. A reader that simply preferred a non-NULL
--     map would resume the window from the map's older rung and discard the
--     escalation — an UNDER-hold, the dangerous direction. (An earlier
--     revision of this note claimed over-holding was the worst case. It was
--     not: that reader shipped, and did exactly this.)
--   * it can hold an entry for a window the old binary released while a
--     sibling stayed frozen (it has no way to retire it). The reader bounds
--     every entry by its own hold_until plus the grace, so that window is
--     over-held for the remainder of a hold it had already been granted.
--
-- So the reader does not trust a map the pair-level columns have outrun.
-- When the columns record a WORSE freeze than the map's own summary —
-- escalated where no entry is, a higher rung, or a later hold — every held
-- entry is raised to the fail-closed fold of itself and the pair-level
-- ladder, and the pair-level ladder is carried as the UNOWNED entry (key
-- "0") for the windows the map does not name. Folded, not replaced: the old
-- binary's 5m window can overwrite the columns with a fresh, LATER hold
-- while the map still holds a 1h window's escalation, and that must survive
-- too. This binary itself only ever writes the columns AS the map's
-- summary, so on a row no other binary touched the rule never fires.
--
-- With that, the worst case of a rollback really is over-holding: windows of
-- the pair that were never frozen rehydrate the ownerless ladder, as they do
-- for a row written before this migration, until it lapses or an operator
-- lifts the freeze — and, like everything above, only if Redis is also lost
-- in that span. Executed against real TimescaleDB with the previous
-- binary's own UPDATE in
-- test/integration/freeze_window_ladders_rollback_test.go.
--
-- Additive: one nullable column, no default, no existing column touched, no
-- constraint tightened. `freeze_events` is a compressed hypertable;
-- TimescaleDB supports ADD COLUMN of a nullable column with no default on a
-- compressed hypertable and rewrites no chunk.

BEGIN;

ALTER TABLE freeze_events
    ADD COLUMN IF NOT EXISTS window_ladders jsonb;

COMMENT ON COLUMN freeze_events.window_ladders IS
    'ADR-0019 lifecycle PER aggregation window: a jsonb object keyed by the '
    'window in whole seconds ("300", "3600", "86400"), each value the '
    'window''s own ladder (fired_at, hold_until, extensions_used, escalated, '
    'unfreeze_streak, corroborated). Key "0" is a ladder with no recorded '
    'owning window, carried over from a row written before 0163. The '
    'hold_until / extensions_used / escalated / corroborated columns are the '
    'fail-closed summary of these entries, and hold_until IS NOT NULL is '
    'still the switch a reader honours any of it on. NULL on rows written '
    'before 0163, whose pair-level ladder then answers for every window.';

COMMIT;
