-- 0205 up — run the trades compression policy under a 5 s lock_timeout.
--
-- WHY: compress_chunk ends by asking for ACCESS EXCLUSIVE on the chunk and
-- its indexes. Behind any long reader of that chunk (a historical CAGG
-- refresh, a multi-minute scan) the request queues with no bound, and
-- PostgreSQL queues every later request for the chunk behind it, so API
-- reads and writers stall for as long as the reader runs. With the bound
-- the request gives up after 5 s, the queue drains, the chunk stays
-- uncompressed and readable, and the job fails and retries on its own
-- retry_period. 5 s matches the restamp's bound in
-- internal/storage/timescale/trades_chunks.go.
--
-- HOW: the built-in policy offers no lock_timeout, and a SET clause on a
-- procedure forbids the per-chunk COMMIT the policy relies on. So a custom
-- job, trades_compression_policy, sets lock_timeout for its own session and
-- calls the same _timescaledb_functions.policy_compression with the same
-- config, then the built-in job is removed. Schedule, retries, scheduled
-- flag and config (compress_after = 15 days since 0197) carry over, and
-- policy_compression_check still validates config edits made via alter_job.
-- The job is no longer attached to a hypertable in
-- timescaledb_information.jobs: readers resolve it by proc name.
--
-- Rejected: ALTER ROLE ... SET lock_timeout. The job runs as the
-- application role, so that would bound every API and writer session too.
--
-- Lock: catalog rows only (add_job, alter_job, remove_compression_policy);
-- no table lock. Old-binary-safe: a previous restamp binary cannot resolve
-- the new job and refuses to start rather than run unpaused.

BEGIN;

CREATE OR REPLACE PROCEDURE trades_compression_policy(job_id integer, config jsonb)
LANGUAGE plpgsql
AS $$
DECLARE
    prev text := current_setting('lock_timeout');
BEGIN
    -- Session-level: SET LOCAL would end at the policy's first per-chunk
    -- COMMIT. Restored only after a successful run: a failed by-hand CALL
    -- run_job leaves that session at 5s.
    PERFORM set_config('lock_timeout', '5s', false);
    CALL _timescaledb_functions.policy_compression(job_id, config);
    PERFORM set_config('lock_timeout', prev, false);
END
$$;

DO $$
DECLARE
    builtin record;
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

    IF n_builtin = 0 AND n_bounded = 1 THEN
        RETURN;
    END IF;
    IF n_builtin <> 1 OR n_bounded <> 0 THEN
        RAISE EXCEPTION '0205: expected one built-in trades compression policy and no trades_compression_policy job, found % and %',
            n_builtin, n_bounded;
    END IF;

    -- A usd-volume-restamp -write run holds this session lock and ends by
    -- alter_job on the job it paused; deleting that job under it would leave
    -- the replacement paused. Key form: pg_advisory_lock(int4) is a bigint
    -- key split into classid (high 32) and objid (low 32), objsubid 1.
    IF EXISTS (SELECT 1 FROM pg_locks l
                WHERE l.locktype = 'advisory' AND l.objsubid = 1
                  AND ((l.classid::bigint << 32) | l.objid::bigint)
                      = hashtext('usd-volume-restamp:trades')::bigint
                  AND l.pid <> pg_backend_pid()) THEN
        RAISE EXCEPTION '0205: a usd-volume-restamp -write run holds advisory lock hashtext(''usd-volume-restamp:trades''); it will re-enable the built-in compression job by id on exit. Let it finish (or stop it), then re-run the migration';
    END IF;

    SELECT j.job_id, j.schedule_interval, j.max_runtime, j.max_retries,
           j.retry_period, j.scheduled, j.config
      INTO builtin
      FROM timescaledb_information.jobs j
     WHERE j.proc_name = 'policy_compression'
       AND j.hypertable_schema = current_schema()
       AND j.hypertable_name = 'trades';

    new_id := add_job('trades_compression_policy'::regproc,
                      builtin.schedule_interval,
                      config       => builtin.config,
                      scheduled    => builtin.scheduled,
                      check_config => '_timescaledb_functions.policy_compression_check'::regproc);
    PERFORM alter_job(new_id,
                      max_runtime  => builtin.max_runtime,
                      max_retries  => builtin.max_retries,
                      retry_period => builtin.retry_period);
    PERFORM remove_compression_policy('trades');
END
$$;

COMMIT;
