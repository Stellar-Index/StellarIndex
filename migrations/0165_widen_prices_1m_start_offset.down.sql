-- 0165 down — restore prices_1m's refresh policy as 0147 set it
-- (5-minute start_offset).

BEGIN;

SELECT remove_continuous_aggregate_policy('prices_1m', if_not_exists => true);

SELECT add_continuous_aggregate_policy(
    'prices_1m',
    start_offset      => INTERVAL '5 minutes',
    end_offset        => INTERVAL '30 seconds',
    schedule_interval => INTERVAL '30 seconds'
);

COMMIT;
