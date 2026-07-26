-- 0119 up — give the ADR-0019 freeze LIFECYCLE a durable home on
-- `freeze_events`, so the extension ladder survives losing Redis.
--
-- What exists today. `freeze_events` records that a pair froze and,
-- eventually, that it recovered. The freeze's LIFECYCLE — how many of
-- ADR-0019's four 30-minute extensions it has spent, whether it has
-- ESCALATED to operator review, when the current hold expires, and
-- whether a corroborating lens was available at fire time (which chose
-- between the 30-minute and 10-minute initial holds) — lives in exactly
-- two places, both volatile:
--
--   1. the aggregator's in-process `o.freezeStates` map, and
--   2. a JSON blob inside the Redis `freeze:<asset>:<quote>` marker.
--
-- Redis is a cache. It is deployed without persistence, it is flushed
-- during incidents, and it is the one component in this system whose
-- data loss is explicitly considered survivable everywhere else.
--
-- What that costs. From `internal/aggregate/orchestrator/phase2_freeze.go`,
-- a missing marker under a live freeze is read as the ADR-0019 operator
-- override ("force unfreeze"), because until now that was the only way it
-- could happen. So a Redis flush does not merely forget the ladder — the
-- next tick RELEASES every live freeze, and a pair that had climbed the
-- whole 2-hour ladder to ESCALATED ("stays active until manual unfreeze",
-- ADR-0019) silently auto-unfreezes and republishes the price a P1 alert
-- had already asked a human to look at. If the aggregator restarts after
-- the flush instead, the pair re-freezes from extensions_used=0 and the
-- 2-hour escalation clock starts over, so a restart cadence shorter than
-- two hours holds a pair frozen indefinitely while never paging anyone.
--
-- Both failure directions are silent, and both are on the money surface:
-- a freeze serves a last-known-good price to every /v1/price caller.
--
-- The columns mirror `freeze.State` field-for-field (that struct is the
-- lifecycle's authority; see internal/aggregate/freeze/lifecycle.go):
--
--   hold_until       when the current hold segment expires and the
--                    lifecycle re-evaluates (release / extend / escalate).
--                    Slides forward on every extension. ALSO the freshness
--                    bound on the durable ladder itself — see below.
--   extensions_used  granted extensions; at Policy.MaxExtensions the next
--                    expiry escalates instead of extending.
--   escalated        the ladder ran out and a human has been paged. The
--                    one flag that must never be lost, because an
--                    escalated freeze does not auto-unfreeze.
--   corroborated     whether a corroborating lens (triangulation chain or
--                    cross-oracle reference) produced a reading at fire
--                    time. Chose the initial hold; kept so an operator can
--                    tell WHY a freeze's first hold was 10 minutes not 30.
--
-- `fired_at` is deliberately NOT added: the existing `frozen_at` column
-- already is it, and duplicating it would create two answers to "how old
-- is this freeze".
--
-- Why hold_until is the freshness bound. The reader only honours a durable
-- ladder while `now() < hold_until + grace` on a row that is still OPEN
-- (recovered_at IS NULL). That is the Redis marker's TTL semantics
-- reproduced durably, and it keeps the change narrow in both directions:
--
--   * Redis flushed mid-hold  → row open, hold live  → ladder rehydrates,
--     the freeze and its escalation survive. This is the fix.
--   * aggregator down for a week → row open, hold long expired → the
--     ladder is NOT resurrected, exactly as today. A week-old freeze does
--     not spring back to life because a stale row was never closed.
--   * operator ran `stellarindex-ops freeze-unfreeze` → that command
--     stamps recovered_at, so the row is CLOSED and the override stands.
--
-- Additive: four nullable columns, no existing column touched, no
-- constraint tightened. The previous released binary neither reads nor
-- writes them and keeps working against this schema unchanged
-- (migrations/README.md rule 9). `freeze_events` is a compressed
-- hypertable; TimescaleDB supports ADD COLUMN of a nullable column with
-- no default on a compressed hypertable, and no chunk is rewritten —
-- existing rows read NULL, which the reader treats as "pre-0119 row, no
-- durable ladder" and falls back to today's Redis-only behaviour.

BEGIN;

ALTER TABLE freeze_events
    ADD COLUMN IF NOT EXISTS hold_until      timestamptz,
    ADD COLUMN IF NOT EXISTS extensions_used integer,
    ADD COLUMN IF NOT EXISTS escalated       boolean,
    ADD COLUMN IF NOT EXISTS corroborated    boolean;

COMMENT ON COLUMN freeze_events.hold_until IS
    'ADR-0019 lifecycle: when the current hold segment expires and the '
    'freeze re-evaluates (release / extend / escalate). Slides forward on '
    'every extension. Also the freshness bound on the durable ladder — a '
    'reader only rehydrates from an OPEN row whose hold has not lapsed, so '
    'a long-dead aggregator cannot resurrect a stale freeze. NULL on rows '
    'written before 0119.';

COMMENT ON COLUMN freeze_events.extensions_used IS
    'ADR-0019 lifecycle: granted 30-minute extensions. At '
    'Policy.MaxExtensions (default 4 = 2 hours) the next hold expiry '
    'escalates instead of extending. NULL on rows written before 0119.';

COMMENT ON COLUMN freeze_events.escalated IS
    'ADR-0019 lifecycle: the extension ladder ran out and this freeze is '
    'awaiting operator action. An escalated freeze does NOT auto-unfreeze '
    '("stays active until manual unfreeze"), which is why losing this flag '
    'to a Redis flush silently republished a price a P1 had already '
    'escalated. NULL on rows written before 0119.';

COMMENT ON COLUMN freeze_events.corroborated IS
    'ADR-0019 lifecycle: whether a corroborating lens (triangulation chain '
    'or cross-oracle reference set) produced a reading for this pair at '
    'fire time. Selected the initial hold — 30 minutes corroborated, 10 '
    'uncorroborated (freeze.DefaultUncorroboratedInitialHold) — and is kept '
    'so an operator can see why. NULL on rows written before 0119.';

-- The rehydrate read is "the open row for THIS (asset, quote)", which the
-- existing freeze_events_asset_idx (asset_id, quote_id, frozen_at DESC)
-- already serves; the open set is a handful of rows at most. No new index.

COMMIT;
