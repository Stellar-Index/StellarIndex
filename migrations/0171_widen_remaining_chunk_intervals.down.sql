-- 0171 down — intentionally a NO-OP, for the same reason as 0062's down.
--
-- Narrowing these hypertables back to 1-day (aquarius_rewards_events: 3-day)
-- chunks would re-arm the chunk-count / lock-pressure pathology 0062 and 0171
-- remove, and set_chunk_time_interval only affects future chunks, so a
-- rollback could not un-widen the chunks already written.
SELECT 1;
