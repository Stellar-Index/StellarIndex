-- 0165 up — widen prices_1m's refresh start_offset past the
-- ch-live-catchup worst-case stall (RLT-154 / GH #623).
--
-- 0002 set start_offset to 5 minutes, sized against late-arriving
-- Postgres backfills only. Under the CH feed-switch (ADR-0034 #10) a
-- lake hole stalls the projector's DEX-trade source until
-- ch-live-catchup heals it — deploy/systemd/ch-live-catchup.timer
-- runs every 10 minutes — so a stall that starts right after a catch-up
-- tick can hold up to ~10 minutes of trades before they drain. Those
-- trades land with ledger-close timestamps up to 10 minutes old; a
-- 5-minute lookback never revisits the earliest buckets, so
-- /v1/ohlc, /v1/history and /v1/chart permanently under-report them
-- while /v1/coverage still reports the range complete (trades itself
-- is not missing anything, only prices_1m's materialization of it).
--
-- This is latent today: production runs persist_per_source = true, so
-- the dispatcher still writes DEX trades live from ledgerstream and
-- the projector is not yet the sole writer. It becomes live loss at
-- the Phase-4 flip (persist_per_source = false) referenced in
-- stellarindex.toml.j2. 15 minutes covers the 10-minute catch-up
-- period with a 5-minute margin for the catch-up run's own drain time
-- and clock skew — the same margin 0002's original 5-minute offset
-- allowed for backfills, layered on top of the stall.
--
-- remove_continuous_aggregate_policy + add_continuous_aggregate_policy
-- is the supported way to change an existing policy's offsets — there
-- is no in-place ALTER for a continuous-aggregate refresh job. This
-- does not touch materialized data: prices_1m's rows, its index, and
-- every other CAGG's policy are unchanged.

BEGIN;

SELECT remove_continuous_aggregate_policy('prices_1m', if_not_exists => true);

SELECT add_continuous_aggregate_policy(
    'prices_1m',
    start_offset      => INTERVAL '15 minutes',
    end_offset        => INTERVAL '30 seconds',
    schedule_interval => INTERVAL '30 seconds'
);

COMMIT;
