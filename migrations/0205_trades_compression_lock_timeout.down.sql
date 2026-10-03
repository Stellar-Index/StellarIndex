-- 0205 down — back to the built-in trades compression policy, carrying the
-- job's schedule, retries, scheduled flag and config over.

BEGIN;

DO $$
DECLARE
    bounded record;
    n_builtin int;
    n_bounded int;
    new_id int;
BEGIN
    SELECT count(*) INTO n_builtin
      FROM timescaledb_information.jobs j
     WHERE j.proc_name = 'policy_compression'
       AND j.hypertable_schema = current_schema()
       AND j.hypertable_name = 'trades';
    SELECT count(*) INTO n_bounded
      FROM timescaledb_information.jobs j
     WHERE j.proc_schema = current_schema()
       AND j.proc_name = 'trades_compression_policy';

    IF n_builtin = 1 AND n_bounded = 0 THEN
        RETURN;
    END IF;
    IF n_builtin <> 0 OR n_bounded <> 1 THEN
        RAISE EXCEPTION '0205 down: expected one trades_compression_policy job and no built-in trades compression policy, found % and %',
            n_bounded, n_builtin;
    END IF;

    SELECT j.job_id, j.schedule_interval, j.max_runtime, j.max_retries,
           j.retry_period, j.scheduled, j.config
      INTO bounded
      FROM timescaledb_information.jobs j
     WHERE j.proc_schema = current_schema()
       AND j.proc_name = 'trades_compression_policy';

    new_id := add_compression_policy('trades',
                                     compress_after => (bounded.config->>'compress_after')::interval);
    -- Positional schedule_interval: carried over, not a new value.
    PERFORM alter_job(new_id, bounded.schedule_interval,
                      max_runtime  => bounded.max_runtime,
                      max_retries  => bounded.max_retries,
                      retry_period => bounded.retry_period,
                      scheduled    => bounded.scheduled,
                      config       => bounded.config);
    PERFORM delete_job(bounded.job_id);
END
$$;

DROP PROCEDURE IF EXISTS trades_compression_policy(integer, jsonb);

COMMIT;
