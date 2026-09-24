-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the backfill runbook it carries. A
-- FRESH host gets these objects from deploy/clickhouse/tier1_schema.sql,
-- which declares each of them identically — the gate named below pins that,
-- so the DDL here is a mirror and the runbook is the reason the file exists.
-- Scope markers are enforced by scripts/ci/lint-ch-apply-scope.sh; the
-- fresh-host apply set is declared in
-- configs/ansible/roles/archival-node/tasks/08-clickhouse.yml.
--
-- account_activity: per-account activity watermark (#31) — (account → the
-- max ledger the account was EVER named in, across every role the
-- account-history readers serve), MV-maintained from stellar.operations,
-- stellar.transactions and stellar.operation_participants.
--
-- Why this table exists: GET /v1/accounts/{g}/operations resolves each of its
-- two keyset arms with a reverse primary-key read over stellar.operations
-- (10.6B rows, ORDER BY (ledger_seq, tx_index, op_index)) — `ORDER BY pk DESC
-- LIMIT n` streams granules backwards FROM THE TIP until it accumulates the
-- account's rows. For a long-idle account that walk covers every granule
-- between the tip and the account's last activity: MEASURED ~4s live
-- (2026-08-24, r1) for a 46d-idle account
-- (GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4), worse under
-- load — the scan that ate the 8s request budget (the tx-outcome starvation
-- SYMPTOM was separately fixed by PR #155's detached budget; this fixes the
-- scan itself). With the watermark, the reader first does a point lookup here
-- (primary-key read over ~53M single-column-keyed rows, ms) and adds
-- `ledger_seq <= last_ledger` to each arm's resolve — partition + primary-key
-- pruning then starts the reverse read AT the account's real last activity.
--
-- ╔════════════════════════════════════════════════════════════════════════╗
-- ║ CORRECTNESS INVARIANT — DATA-HIDING (do not weaken):                  ║
-- ║ the watermark must be an UPPER bound on the ledger_seq of EVERY row   ║
-- ║ the account-history queries can return for the account, in ANY role.  ║
-- ║ Soroban/SAC token movements can postdate a classic account's own      ║
-- ║ sourced activity — the account then appears ONLY as a non-source      ║
-- ║ participant. That is why THREE MVs feed this table: they fire on      ║
-- ║ inserts to the SAME tables that feed the readers' key sources         ║
-- ║ (operations → ops_by_source ops arm, operation_participants,          ║
-- ║ transactions → ops_by_source tx-sentinel arm), so any row that can    ║
-- ║ surface also raised the watermark to at least its own ledger — an     ║
-- ║ EXACT upper bound by construction, immune to drift in participant     ║
-- ║ extraction. A too-HIGH watermark only costs scan range; a too-LOW     ║
-- ║ one silently HIDES history rows (unacceptable). Never narrow the MV   ║
-- ║ set; readers take max() over un-merged RMT rows, never FINAL-trust a  ║
-- ║ single row; a missing row falls back to the UNBOUNDED scan.           ║
-- ╚════════════════════════════════════════════════════════════════════════╝
--
-- Engine: ReplacingMergeTree(last_ledger) keyed by account_id — merges keep
-- the max-last_ledger row per account; readers max() over whatever is not
-- yet merged, so merge timing never changes an answer. No first_ledger
-- column: RMT keeps the max-version row, which would silently discard a
-- min-aggregate — if "first active" is ever wanted, that is a separate
-- min-semantics table, not a column here.
--
-- Bonus the shape gives for free: max(last_ledger)/argMax(last_seen,
-- last_ledger) is the account's "last active" fact for directory surfaces
-- (not yet wired into the account payload — API/OpenAPI follow-up).
--
-- Operator sequence (deploy order matters — see the hazard note in Step 2):
--   Step 1 (table + MVs, instant) → Step 2 (windowed backfill, heavy)
--   → Step 3 (verify) → deploy the binary that reads it.
-- The reader degrades gracefully (no row → unbounded scan), but Step 2 must
-- complete BEFORE the reading binary deploys: see the Step-2 hazard.

-- ── Step 1: table + the three MVs ───────────────────────────────────────────

CREATE TABLE IF NOT EXISTS stellar.account_activity
(
    account_id  String,
    last_ledger UInt32,
    last_seen   DateTime('UTC')
)
ENGINE = ReplacingMergeTree(last_ledger)
ORDER BY account_id;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_activity_ops_mv
TO stellar.account_activity AS
SELECT source_account AS account_id, max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
FROM stellar.operations
WHERE source_account != ''
GROUP BY account_id;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_activity_tx_mv
TO stellar.account_activity AS
SELECT source_account AS account_id, max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
FROM stellar.transactions
WHERE source_account != ''
GROUP BY account_id;

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_activity_participants_mv
TO stellar.account_activity AS
SELECT account AS account_id, max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
FROM stellar.operation_participants
GROUP BY account_id;

-- ── Step 2: windowed historical backfill ────────────────────────────────────
-- ***Heavy op.*** run-heavy-job.sh, ONE window at a time, 2M-ledger windows
-- (all three source tables are PARTITION BY intDiv(ledger_seq, 1000000); the
-- selects are 2-3 narrow columns + a GROUP BY, no XDR decode). Capture TIP at
-- MV-creation time; overlapping the MV era is idempotent (max-wins RMT +
-- max() reads). Re-running any window is SAFE.
--
-- HAZARD (why Step 2 precedes the reader deploy): until the backfill covers
-- an account, a post-MV re-ingest of OLD ledgers can create the account's
-- ONLY watermark row with a ledger far below its true (pre-MV-ingested) last
-- activity — a too-LOW bound, i.e. hidden rows. Run the backfill immediately
-- after Step 1 and before deploying a binary that reads the bound; from then
-- on the invariant holds unconditionally (backfill + MVs jointly cover all
-- history, and max() only ever rises).
--
-- FAIL-CLOSED (F112): nothing watches this loop, and the invariant above is
-- only as good as its coverage, so every way the block can end WITHOUT
-- having covered every window is made to end non-zero and WITHOUT the final
-- COMPLETE line:
-- * a job fails (OOM, a network blip, a bad SETTINGS value) → the run
--  aborts on the spot instead of ploughing on to the next table/window;
-- * the TIP probe fails or answers empty/0/garbage → the loop would run NO
--  window and an unchecked one "succeeds" over nothing, so TIP is validated
--  before anything runs (the windows are counted in bash, not by `seq`:
--  BSD seq prints 2e+06 for a large value);
-- * run-heavy-job.sh finds the per-job lock held → a manual run is refused
--  non-zero, but one launched from a systemd unit prints "skipping this
--  fire" and exits 0 WITHOUT running the payload. Exit status cannot tell
--  that from success, so each payload writes a marker file only after its
--  INSERT returns 0, and a job with no marker aborts the run;
-- * the closing count must see every job's marker (N of N, N > 0).
-- The whole loop is ONE parenthesised subshell with the COMPLETE line
-- chained behind `&&`: an abort ends the subshell, never the operator's
-- login shell, and no later line of the same paste can print success after
-- a failure (pasted into a nested shell, a bare `exit 1` used to kill the
-- inner shell and hand the rest of the paste — the success echo — to the
-- outer one). Paste the block WHOLE. After an abort, fix the cause and
-- re-run with the same TIP (every window is idempotent, see above); do not
-- move on to Step 3 until COMPLETE has printed.
--
--   TIP=$(clickhouse-client --port 9300 -q "SELECT max(ledger_seq) FROM stellar.ledgers") || TIP=""
--   (
--     case "$TIP" in ''|*[^0-9]*|0*)
--       echo "account_activity backfill: TIP='$TIP' is not a ledger number (probe failed) - aborting, NOTHING was backfilled" >&2; exit 1 ;;
--     esac
--     AA_MARKS=$(mktemp -d) || exit 1
--     AA_DONE=0; AA_WANT=0
--     aa_job() {
--       AA_MARK="$AA_MARKS/$1-$2.ok"
--       /usr/local/sbin/run-heavy-job.sh "acct-activity-$1-$2" bash -c 'clickhouse-client --port 9300 -q "$1" && : > "$2"' aa-job "$3" "$AA_MARK" </dev/null \
--         || { echo "account_activity backfill: $1 window $2 FAILED - aborting, the watermark is INCOMPLETE from window $2 up" >&2; exit 1; }
--       [ -e "$AA_MARK" ] \
--         || { echo "account_activity backfill: $1 window $2 DID NOT RUN (the wrapper exited 0 with no success marker - its per-job lock was held and it skipped) - aborting, the watermark is INCOMPLETE from window $2 up" >&2; exit 1; }
--       AA_DONE=$((AA_DONE + 1))
--     }
--     W=2
--     while [ "$W" -le "$TIP" ]; do
--       AA_WANT=$((AA_WANT + 3))
--       aa_job ops "$W" "
--         INSERT INTO stellar.account_activity
--         SELECT source_account, max(ledger_seq), max(close_time)
--         FROM stellar.operations
--         WHERE source_account != '' AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--         GROUP BY source_account
--         SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--       aa_job tx "$W" "
--         INSERT INTO stellar.account_activity
--         SELECT source_account, max(ledger_seq), max(close_time)
--         FROM stellar.transactions
--         WHERE source_account != '' AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--         GROUP BY source_account
--         SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--       aa_job part "$W" "
--         INSERT INTO stellar.account_activity
--         SELECT account, max(ledger_seq), max(close_time)
--         FROM stellar.operation_participants
--         WHERE ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--         GROUP BY account
--         SETTINGS max_threads = 4, max_memory_usage = 8000000000"
--       W=$((W + 2000000))
--     done
--     echo "account_activity backfill: $AA_DONE of $AA_WANT jobs ran to success"
--     [ "$AA_WANT" -gt 0 ] && [ "$AA_DONE" -eq "$AA_WANT" ] \
--       || { echo "account_activity backfill: NOT every job ran (or TIP=$TIP yields no window) - aborting" >&2; exit 1; }
--     rm -rf "$AA_MARKS"
--   ) && echo "account_activity backfill: COMPLETE - every window 2..$TIP covered"
--
-- ── Step 3: verify ──────────────────────────────────────────────────────────
-- ***Heavy op.*** Same discipline as Step 2: run-heavy-job.sh, one 2M-ledger
-- window at a time, every source read scoped by ledger_seq (partition
-- pruning) and capped by SETTINGS. This is NOT a cheap spot check and must
-- not be run as a bare query on the serving host.
--
-- What it checks, per window: for a hash sample of the accounts ACTIVE IN
-- THAT WINDOW (1 in AA_MOD, default 64; AA_MOD=1 is exhaustive and needs
-- the memory to group every account), the watermark is >= the account's
-- max ledger in the window, read from the same three tables the MVs and
-- the readers' key sources are fed from. A MISSING watermark row counts
-- too (wm = 0): after a complete Step 2 every account has one. Too-high is
-- fine; ANY too-low row is a data-hiding bug.
--
-- Why per window (F112): the sample has to be drawn from the population a
-- backfill gap can actually hurt, in EVERY window. Sampling recently
-- active accounts tests only what the live MVs cover regardless of Step 2,
-- and sampling the oldest accounts tests only the first window — either
-- returns 0 over a backfill that skipped a window in between. Here a
-- skipped window W leaves the accounts active in W with a watermark below
-- their W activity (or none), and W's own check counts them.
--
-- Uses the TIP from Step 2 (a later re-probe is also fine: the extra
-- windows are MV-covered). A failed or lock-skipped job aborts — the
-- wrapper's skip prints nothing on stdout, and an empty answer is refused
-- rather than read as 0 (the count is captured through a file, not $(...):
-- as root the wrapper's disk watchdog leaves a `sleep 30` holding stdout, and
-- a command substitution would wait on it after every window). Every bad
-- window is named, then the block ends non-zero without the PASSED line;
-- re-run Step 2 for the named windows (or all of it) and verify again.
--
--   (
--     case "$TIP" in ''|*[^0-9]*|0*)
--       echo "account_activity verify: TIP='$TIP' is not a ledger number - re-run the TIP probe (first line of Step 2), NOTHING was verified" >&2; exit 1 ;;
--     esac
--     AA_MOD="${AA_MOD:-64}"
--     case "$AA_MOD" in ''|*[^0-9]*|0*)
--       echo "account_activity verify: AA_MOD='$AA_MOD' is not a positive integer - aborting" >&2; exit 1 ;;
--     esac
--     AA_OUT=$(mktemp) || exit 1
--     AA_SEEN=0; AA_BAD=0
--     W=2
--     while [ "$W" -le "$TIP" ]; do
--       /usr/local/sbin/run-heavy-job.sh "acct-activity-verify-$W" clickhouse-client --port 9300 -q "
--         SELECT countIf(wm < truth) FROM (
--           SELECT acct, maxIf(l, src = 0) AS truth, maxIf(l, src = 1) AS wm FROM (
--             SELECT source_account AS acct, ledger_seq AS l, 0 AS src FROM stellar.operations
--             WHERE source_account != '' AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--               AND cityHash64(source_account) % $AA_MOD = 0
--             UNION ALL
--             SELECT source_account AS acct, ledger_seq AS l, 0 AS src FROM stellar.transactions
--             WHERE source_account != '' AND ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--               AND cityHash64(source_account) % $AA_MOD = 0
--             UNION ALL
--             SELECT account AS acct, ledger_seq AS l, 0 AS src FROM stellar.operation_participants
--             WHERE ledger_seq >= $W AND ledger_seq < $((W + 2000000))
--               AND cityHash64(account) % $AA_MOD = 0
--             UNION ALL
--             SELECT account_id AS acct, last_ledger AS l, 1 AS src FROM stellar.account_activity
--             WHERE cityHash64(account_id) % $AA_MOD = 0)
--           GROUP BY acct
--           HAVING truth > 0)
--         SETTINGS max_threads = 4, max_memory_usage = 8000000000" </dev/null > "$AA_OUT" \
--         || { echo "account_activity verify: window $W FAILED to run - aborting, NOTHING is verified" >&2; exit 1; }
--       AA_N=$(cat "$AA_OUT")
--       case "$AA_N" in ''|*[^0-9]*)
--         echo "account_activity verify: window $W answered '$AA_N', not a count (the wrapper's lock-skip exits 0 and prints nothing) - aborting, NOTHING is verified" >&2; exit 1 ;;
--       esac
--       AA_SEEN=$((AA_SEEN + 1))
--       [ "$AA_N" -eq 0 ] \
--         || { echo "account_activity verify: window $W has $AA_N sampled account(s) with a too-LOW or MISSING watermark - re-run Step 2 for this window" >&2; AA_BAD=$((AA_BAD + 1)); }
--       W=$((W + 2000000))
--     done
--     echo "account_activity verify: $AA_SEEN window(s) checked, $AA_BAD with a too-LOW watermark"
--     rm -f "$AA_OUT"
--     [ "$AA_SEEN" -gt 0 ] && [ "$AA_BAD" -eq 0 ]
--   ) && echo "account_activity verify: PASSED - every window 2..$TIP is covered for the sample"
--
-- Expect PASSED. Anything else: do NOT deploy the reading binary.
--
-- ── ROLLBACK ────────────────────────────────────────────────────────────────
-- Additive watermark; the reader falls back to the unbounded scan when the
-- table is absent (schema probe) — no binary rollback required, though the
-- idle-account pages regress to the tip-walk latency:
--   DROP TABLE IF EXISTS stellar.account_activity_ops_mv;
--   DROP TABLE IF EXISTS stellar.account_activity_tx_mv;
--   DROP TABLE IF EXISTS stellar.account_activity_participants_mv;
--   DROP TABLE IF EXISTS stellar.account_activity SYNC;
