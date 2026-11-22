#!/usr/bin/env bash
# lake-dedup-driver-test.sh - fixture tests for the fail-open guards in
# deploy/clickhouse/lake-dedup-driver.sh.
#
# clickhouse-client and df are STUBBED (fake-ch / fake-df in a throwaway PATH
# dir), so this runs anywhere in under a second and never touches a real lake.
# The stub records every statement and answers from fixture env vars, so tests
# can assert "OPTIMIZE was never issued", not just that something printed.
#
# Pinned cases:
#   1. a FAILING partition-enumeration query (auth, OOM, server down) must ABORT
#      (exit 1), not look like a quiet run ("done: 0 partitions", exit 0).
#   2. a legitimate quiet run (query ok, nothing found) still exits 0 and logs 0.
#   3. the disk guard must validate free_bytes as digits BEFORE the comparison:
#      non-numeric df output makes `[ -lt ]` error, `if` reads false, and the
#      ABORT is skipped into an unguarded OPTIMIZE.
#   4. a nonzero OPTIMIZE exit code aborts the run.
#   5. the ordinary success path processes every candidate and counts correctly.
#   5b-d. duplicates are counted one partition at a time via FINAL (never a
#      whole-table uniqExact); a zero-dup partition is skipped; a failed probe
#      aborts before any OPTIMIZE.
#   6. the <table> argument is spliced into SQL and the log path, so names
#      outside the lake tables are refused before any statement is issued.
#   6b. a non-integer max_partitions or a DRY_RUN other than 0/1 is refused the
#      same way, instead of silently running uncapped or for real.
#   7. progress reaches monitoring as textfile-collector gauges
#      (stellarindex_lake_dedup_*), and the terminal state is written.
#   8. an ABORT before the candidate count is known still overwrites the previous
#      run's last_exit_ok=1 with 0.
#   9. SIGTERM mid-OPTIMIZE exits non-zero promptly, kills the in-flight
#      clickhouse-client and records last_exit_ok=0 / running=0.
#
# Run: bash deploy/clickhouse/lake-dedup-driver-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
DRIVER="$PWD/deploy/clickhouse/lake-dedup-driver.sh"
[ -r "$DRIVER" ] || { echo "lake-dedup-driver-test: missing $DRIVER" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

mkdir -p "$TMP/bin"

# ─── the fake clickhouse-client ─────────────────────────────────────
# Answers the driver's three query shapes from env fixtures and appends every
# statement (whitespace squashed) to $CH_LOG, so a test can assert one was NEVER
# issued.
cat > "$TMP/bin/fake-ch" <<'STUB'
#!/usr/bin/env bash
sql=""
while [ $# -gt 0 ]; do
  case "$1" in
    -q) sql="${2-}"; shift 2 ;;
    *)  shift ;;
  esac
done
printf '%s\n' "$(printf '%s' "$sql" | tr '\n\t' '  ' | tr -s ' ')" >> "$CH_LOG"
case "$sql" in
  *"modification_time"*)
    if [ -n "${HOT_FAIL:-}" ]; then
      echo "Code: 210. DB::NetException: Connection refused." >&2
      exit 1
    fi
    printf '%b' "${HOT_PARTS:-}"
    ;;
  *"partition_id FROM system.parts"*)
    if [ -n "${ENUM_FAIL:-}" ]; then
      echo "Code: 210. DB::NetException: Connection refused." >&2
      exit 1
    fi
    printf '%b' "${ENUM_PARTS:-}"
    ;;
  *"FINAL WHERE _partition_id"*)
    if [ -n "${PROBE_FAIL:-}" ]; then
      echo "Code: 241. DB::Exception: Memory limit exceeded." >&2
      exit 1
    fi
    part="${sql#*_partition_id = \'}"; part="${part%%\'*}"
    case " ${ZERO_DUP_PARTS:-} " in
      *" $part "*) echo 0 ;;
      *) printf '%s\n' "${DUP_ROWS:-60}" ;;
    esac
    ;;
  *"sum(rows), sum(bytes_on_disk)"*)
    if [ -n "${STATS_FAIL:-}" ]; then
      echo "Code: 60. DB::Exception: Table stellar.x does not exist." >&2
      exit 1
    fi
    printf '%s\n' "${STATS_BEFORE:-100	1000}"
    ;;
  *"OPTIMIZE TABLE"*)
    if [ -n "${OPT_FAIL:-}" ]; then
      echo "Code: 159. DB::Exception: Timeout exceeded while receiving data." >&2
      exit 1
    fi
    if [ -n "${OPT_BLOCK:-}" ]; then
      # A long merge: announce this client's pid, then block in place
      # (exec keeps the pid) so a test can signal the driver mid-OPTIMIZE.
      echo "$$" > "$OPT_STARTED"
      exec sleep "$OPT_BLOCK"
    fi
    ;;
  *"SELECT sum(rows) FROM system.parts"*)
    if [ -n "${AFTER_FAIL:-}" ]; then
      echo "Code: 210. DB::NetException: Connection refused." >&2
      exit 1
    fi
    printf '%s\n' "${ROWS_AFTER:-50}"
    ;;
esac
exit 0
STUB
chmod +x "$TMP/bin/fake-ch"

# ─── the fake df ─────────────────────────────────────────────────────
# Ignores its arguments; prints a header + one avail-bytes line, as
# `df --output=avail -B1 <dir> | tail -1` would.
cat > "$TMP/bin/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
if [ -n "${DF_GARBAGE:-}" ]; then
  echo "${DF_GARBAGE_VALUE:-N/A}"
else
  echo "${DF_AVAIL:-1000000000000}"
fi
STUB
chmod +x "$TMP/bin/df"

STATS_BEFORE_DEFAULT="$(printf '100\t1000')"

# run <case-name> [ENV=VAL …] -- <table> [max_partitions]
# Leaves the statement log in $LOG_STMT, the driver's own log+stdout+stderr
# in $LOG, and the exit status in $RC.
run() {
  local name="$1"; shift
  local -a envs=()
  while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do envs+=("$1"); shift; done
  shift   # the --
  LOG_STMT="$TMP/stmt.$name.log"; : > "$LOG_STMT"
  LOG="$TMP/log.$name"; : > "$LOG"
  env -u ENUM_FAIL -u STATS_FAIL -u OPT_FAIL -u AFTER_FAIL -u DF_GARBAGE -u PROBE_FAIL \
      -u ZERO_DUP_PARTS -u DUP_ROWS -u HOT_PARTS -u HOT_FAIL \
      PATH="$TMP/bin:$PATH" CH="$TMP/bin/fake-ch" CH_LOG="$LOG_STMT" \
      OUT="$LOG" STOP="$TMP/stop.$name" TEXTFILE_DIR=/dev/null \
      STATS_BEFORE="$STATS_BEFORE_DEFAULT" ROWS_AFTER=40 DF_AVAIL=1000000000000 \
      ENUM_PARTS='' \
      ${envs[@]+"${envs[@]}"} bash "$DRIVER" "$@" >> "$LOG" 2>&1
  RC=$?
}

no_optimize() { # no_optimize <case-name> <label> — OPTIMIZE must never fire.
  local log="$TMP/stmt.$1.log" label="$2" hit
  hit="$(grep -Eic 'OPTIMIZE TABLE' "$log")"
  if [ "$hit" -eq 0 ]; then
    ok "$label: OPTIMIZE never reached ClickHouse"
  else
    bad "$label: OPTIMIZE was issued despite the guard that should have refused it"
    sed 's/^/       /' "$log"
  fi
}

echo "lake-dedup-driver-test: enumeration-failure and disk-guard closed forms"

# ── 1. partition-enumeration query FAILS ─────────────────────────────
run enum_fail ENUM_FAIL=1 -- transactions
if [ "$RC" -ne 0 ]; then
  ok "enumeration query failure ⇒ non-zero exit"
else
  bad "enumeration query failure ⇒ exited 0"
  sed 's/^/       /' "$LOG"
fi
if grep -q 'ABORT: partition-enumeration query FAILED' "$LOG"; then
  ok "enumeration query failure ⇒ ABORT is logged"
else
  bad "enumeration query failure ⇒ no ABORT line"
  sed 's/^/       /' "$LOG"
fi
if grep -q '=== done' "$LOG"; then
  bad "enumeration query failure ⇒ still logged a done summary (masks the failure as success)"
else
  ok "enumeration query failure ⇒ no done summary is logged"
fi

# ── 2. a legitimate quiet run (query succeeds, finds nothing) ────────
run quiet_ok -- transactions
if [ "$RC" -eq 0 ]; then
  ok "legitimate zero candidates ⇒ exits 0"
else
  bad "legitimate zero candidates ⇒ exited $RC"
  sed 's/^/       /' "$LOG"
fi
if grep -q 'active partitions: 0' "$LOG" && grep -q '=== done: 0 partitions processed ===' "$LOG"; then
  ok "legitimate zero candidates ⇒ zero is logged and distinguishable from a failure"
else
  bad "legitimate zero candidates ⇒ expected log lines missing"
  sed 's/^/       /' "$LOG"
fi
if grep -q 'ABORT' "$LOG"; then
  bad "legitimate zero candidates ⇒ unexpected ABORT logged"
else
  ok "legitimate zero candidates ⇒ no ABORT logged"
fi

# ── 3. disk guard on unparseable df output ────────────────────────────
run df_garbage ENUM_PARTS=$'0000000001\n' DF_GARBAGE=1 DF_GARBAGE_VALUE='N/A' -- transactions
if [ "$RC" -ne 0 ]; then
  ok "unparseable df output ⇒ non-zero exit"
else
  bad "unparseable df output ⇒ exited 0 (guard failed OPEN)"
  sed 's/^/       /' "$LOG"
fi
if grep -q "free_bytes='N/A' is not a number" "$LOG"; then
  ok "unparseable df output ⇒ ABORT names free_bytes as non-numeric"
else
  bad "unparseable df output ⇒ no free_bytes ABORT line"
  sed 's/^/       /' "$LOG"
fi
no_optimize df_garbage "unparseable df output"

# ── 3b. pool floor: 3x one partition is not the bound ─────────────────
run pool_floor ENUM_PARTS=$'0000000001\n' DF_AVAIL=200000000000 HEAVY_JOB_FLOOR_FILE=/nonexistent -- transactions
if [ "$RC" -ne 0 ] && grep -q "pool floor" "$LOG"; then
  ok "free below the 300 GiB default floor ⇒ ABORT"
else
  bad "free below the pool floor ⇒ no ABORT (rc=$RC)"
  sed 's/^/       /' "$LOG"
fi
no_optimize pool_floor "pool below floor"
printf 'HEAVY_MIN_DATA_KB=1000\n' > "$TMP/floor"
run pool_floor_file ENUM_PARTS=$'0000000001\n' DF_AVAIL=200000000000 HEAVY_JOB_FLOOR_FILE="$TMP/floor" -- transactions
if [ "$RC" -eq 0 ] && ! grep -q "pool floor" "$LOG"; then
  ok "floor file lowers the floor ⇒ partition runs"
else
  bad "floor file ignored (rc=$RC)"
  sed 's/^/       /' "$LOG"
fi

# ── 4. OPTIMIZE itself fails ──────────────────────────────────────────
run optimize_fail ENUM_PARTS=$'0000000002\n' OPT_FAIL=1 -- operations
if [ "$RC" -ne 0 ]; then
  ok "OPTIMIZE failure ⇒ non-zero exit"
else
  bad "OPTIMIZE failure ⇒ exited 0 (rc was logged but never acted on)"
  sed 's/^/       /' "$LOG"
fi
if grep -q 'ABORT partition=0000000002: OPTIMIZE FAILED rc=1' "$LOG"; then
  ok "OPTIMIZE failure ⇒ ABORT names the partition and rc"
else
  bad "OPTIMIZE failure ⇒ no OPTIMIZE-FAILED ABORT line"
  sed 's/^/       /' "$LOG"
fi
if grep -q '=== done' "$LOG"; then
  bad "OPTIMIZE failure ⇒ still logged a done summary"
else
  ok "OPTIMIZE failure ⇒ no done summary is logged"
fi

# ── 5. the ordinary success path still works ─────────────────────────
run happy ENUM_PARTS=$'0000000001\n0000000002\n' STATS_BEFORE="$(printf '100\t1000')" ROWS_AFTER=40 -- operations
if [ "$RC" -eq 0 ]; then
  ok "two clean candidate partitions ⇒ exits 0"
else
  bad "two clean candidate partitions ⇒ exited $RC"
  sed 's/^/       /' "$LOG"
fi
if grep -q '=== done: 2 partitions processed ===' "$LOG"; then
  ok "two clean candidate partitions ⇒ both are processed"
else
  bad "two clean candidate partitions ⇒ done summary does not report 2"
  sed 's/^/       /' "$LOG"
fi
if grep -q 'dup_removed=60' "$LOG"; then
  ok "two clean candidate partitions ⇒ dup_removed is computed correctly (100-40)"
else
  bad "two clean candidate partitions ⇒ dup_removed not as expected"
  sed 's/^/       /' "$LOG"
fi

if grep -q "PARTITION '0000000001' FINAL SETTINGS optimize_throw_if_noop = 1" "$LOG_STMT"; then
  ok "OPTIMIZE asks ClickHouse to throw when it merges nothing"
else
  bad "OPTIMIZE is issued without optimize_throw_if_noop = 1"
  sed 's/^/       /' "$LOG_STMT"
fi

# ── 5a. an OPTIMIZE that returns success but removes nothing aborts ──
run noop_merge ENUM_PARTS=$'0000000001\n' ROWS_AFTER=100 -- operations
if [ "$RC" -ne 0 ] \
   && grep -q 'ABORT partition=0000000001: OPTIMIZE removed 0 of 60 duplicate rows' "$LOG" \
   && ! grep -q '=== done' "$LOG"; then
  ok "no-op OPTIMIZE ⇒ ABORT, non-zero exit, no done summary"
else
  bad "no-op OPTIMIZE ⇒ rc=$RC; counted as processed"
  sed 's/^/       /' "$LOG"
fi

# ── 5a'. a partition written in the last hour (the live tip) is never merged ──
run hot_skip ENUM_PARTS=$'0000000001\n0000000002\n' HOT_PARTS=$'0000000002\n' -- operations
if [ "$RC" -eq 0 ] && grep -q 'skip partition=0000000002 hot' "$LOG" \
   && [ "$(grep -c 'OPTIMIZE TABLE' "$LOG_STMT")" -eq 1 ] \
   && ! grep -q "PARTITION '0000000002'" "$LOG_STMT" \
   && ! grep -q "_partition_id = '0000000002'" "$LOG_STMT"; then
  ok "hot partition ⇒ skipped before the probe; only the cold one is merged"
else
  bad "hot partition ⇒ rc=$RC; the live partition was probed or merged"
  sed 's/^/       /' "$LOG_STMT"
fi
run hot_fail ENUM_PARTS=$'0000000001\n' HOT_FAIL=1 -- operations
if [ "$RC" -ne 0 ] && grep -q 'ABORT: hot-partition query FAILED' "$LOG"; then
  no_optimize hot_fail "hot-partition query failure"
else
  bad "hot-partition query failure ⇒ rc=$RC; did not abort"
  sed 's/^/       /' "$LOG"
fi

# ── 5b. the duplicate probe is per partition, never a whole-table scan ──
# A whole-table uniqExact holds a hash per row of operations (~25B) at once and
# never returns. Every statement that reads the lake table must be scoped to one
# partition, and the count must come from FINAL (the engine's own collapse).
run probe_shape ENUM_PARTS=$'0000000003\n0000000004\n' -- transactions
unscoped="$(grep 'FROM stellar\.transactions' "$LOG_STMT" | grep -vc "_partition_id = '" || true)"
if [ "$unscoped" -eq 0 ] && grep -q "FROM stellar.transactions FINAL WHERE _partition_id = '0000000004'" "$LOG_STMT"; then
  ok "duplicate probe reads one partition per statement, via FINAL"
else
  bad "duplicate probe issued $unscoped unscoped read(s) of the table, or no per-partition FINAL"
  sed 's/^/       /' "$LOG_STMT"
fi
if grep -q 'uniqExact' "$LOG_STMT"; then
  bad "duplicate probe still issues uniqExact"
  sed 's/^/       /' "$LOG_STMT"
else
  ok "duplicate probe issues no uniqExact"
fi

# ── 5c. a partition with no duplicates is skipped, and max_partitions ──
# counts only partitions that are actually deduped.
run probe_skip ENUM_PARTS=$'0000000001\n0000000002\n0000000003\n' ZERO_DUP_PARTS=0000000001 -- operations 1
if [ "$RC" -eq 0 ] && grep -q "skip partition=0000000001 dup_rows=0" "$LOG" \
   && [ "$(grep -c 'OPTIMIZE TABLE' "$LOG_STMT")" -eq 1 ] \
   && grep -q "OPTIMIZE TABLE stellar.operations PARTITION '0000000002' FINAL" "$LOG_STMT"; then
  ok "zero-dup partition skipped; max_partitions=1 dedups exactly the next candidate"
else
  bad "zero-dup skip / max_partitions accounting wrong (rc=$RC)"
  sed 's/^/       /' "$LOG" "$LOG_STMT"
fi

# ── 5d. a failed or unparseable probe aborts before any OPTIMIZE ──────
run probe_fail ENUM_PARTS=$'0000000001\n' PROBE_FAIL=1 -- operations
if [ "$RC" -ne 0 ] && grep -q 'ABORT partition=0000000001: duplicate-count probe FAILED' "$LOG"; then
  ok "probe failure ⇒ ABORT naming the partition"
else
  bad "probe failure ⇒ no ABORT (rc=$RC)"
  sed 's/^/       /' "$LOG"
fi
no_optimize probe_fail "probe failure"
run probe_garbage ENUM_PARTS=$'0000000001\n' DUP_ROWS='Code: 241' -- operations
if [ "$RC" -ne 0 ] && grep -q "dup_rows='Code: 241' is not a number" "$LOG"; then
  ok "unparseable probe answer ⇒ ABORT"
else
  bad "unparseable probe answer ⇒ no ABORT (rc=$RC)"
  sed 's/^/       /' "$LOG"
fi
no_optimize probe_garbage "unparseable probe answer"

# ── 6. the table argument is spliced into SQL: only lake tables pass ──
for bad_table in 'transactions GROUP BY 1; DROP TABLE stellar.ledgers --' '../../etc/cron.d/x' 'ledger_entries_current'; do
  run bad_table -- "$bad_table"
  if [ "$RC" -ne 0 ] && [ ! -s "$LOG_STMT" ]; then
    ok "table '$bad_table' ⇒ refused before any statement reached ClickHouse"
  else
    bad "table '$bad_table' ⇒ exit $RC, statements issued:"
    sed 's/^/       /' "$LOG_STMT"
  fi
done

# ── 6b. a malformed max_partitions or DRY_RUN must refuse, not run unbounded ──
for bad_arg in 'MAXP:5x' 'MAXP:-1' 'DRY:true' 'DRY:yes'; do
  if [ "${bad_arg%%:*}" = MAXP ]; then
    run bad_arg ENUM_PARTS=$'0000000001\n0000000002\n' -- operations "${bad_arg#*:}"
  else
    run bad_arg ENUM_PARTS=$'0000000001\n0000000002\n' DRY_RUN="${bad_arg#*:}" -- operations
  fi
  if [ "$RC" -eq 2 ] && [ ! -s "$LOG_STMT" ]; then
    ok "$bad_arg ⇒ exit 2 before any statement reached ClickHouse"
  else
    bad "$bad_arg ⇒ exit $RC, statements issued:"
    sed 's/^/       /' "$LOG_STMT"
  fi
done

# ── 7. textfile-collector metrics reach monitoring ────────────
# Point TEXTFILE_DIR at a real directory and assert the terminal .prom state:
# running=0, partitions_processed=2, last_exit_ok=1.
PROMDIR="$TMP/textfile-collector"
mkdir -p "$PROMDIR"
run metrics_happy ENUM_PARTS=$'0000000001\n0000000002\n' STATS_BEFORE="$(printf '100\t1000')" \
    ROWS_AFTER=40 TEXTFILE_DIR="$PROMDIR" -- operations
PROM_FILE="$PROMDIR/lake_dedup_operations.prom"
if [ "$RC" -eq 0 ]; then
  ok "metrics run: exits 0"
else
  bad "metrics run: exited $RC"
  sed 's/^/       /' "$LOG"
fi
if [ -f "$PROM_FILE" ]; then
  ok "metrics run: textfile-collector .prom was written"
else
  bad "metrics run: no .prom file at $PROM_FILE"
fi
if grep -qE '^stellarindex_lake_dedup_running\{table="operations"\} 0$' "$PROM_FILE" 2>/dev/null; then
  ok "metrics run: terminal state reports running=0"
else
  bad "metrics run: running gauge missing or not 0 at exit"
  sed 's/^/       /' "$PROM_FILE" 2>/dev/null
fi
if grep -qE '^stellarindex_lake_dedup_partitions_processed\{table="operations"\} 2$' "$PROM_FILE" 2>/dev/null; then
  ok "metrics run: partitions_processed reflects both completed partitions"
else
  bad "metrics run: partitions_processed did not report 2"
  sed 's/^/       /' "$PROM_FILE" 2>/dev/null
fi
if grep -qE '^stellarindex_lake_dedup_last_exit_ok\{table="operations"\} 1$' "$PROM_FILE" 2>/dev/null; then
  ok "metrics run: last_exit_ok=1 on a clean run"
else
  bad "metrics run: last_exit_ok missing or not 1"
  sed 's/^/       /' "$PROM_FILE" 2>/dev/null
fi

# ── 8. an enumeration ABORT overwrites a previous clean run's state ───
# Case 7's .prom says last_exit_ok=1; an abort before the candidate count is
# known must replace it with 0, or the failure is invisible.
# A group member of the textfile directory plants the old fixed progress name as a link: root must not write through it.
PRIV_TMP="$TMP/privtmp"; mkdir -p "$PRIV_TMP"
ln -s "$TMP/planted-victim" "$PROM_FILE.progress"
run metrics_enum_fail ENUM_FAIL=1 TEXTFILE_DIR="$PROMDIR" TMPDIR="$PRIV_TMP" -- operations
if [ -e "$TMP/planted-victim" ]; then
  bad "metrics enum-fail run: the driver wrote through a link planted at the progress name"
else
  ok "metrics enum-fail run: a link planted at the old progress name is not written through"
fi
if [ "$RC" -ne 0 ] && grep -q 'ABORT: partition-enumeration query FAILED' "$LOG"; then
  ok "metrics enum-fail run: aborts non-zero"
else
  bad "metrics enum-fail run: expected an enumeration ABORT, exit $RC"
  sed 's/^/       /' "$LOG"
fi
if grep -qE '^stellarindex_lake_dedup_last_exit_ok\{table="operations"\} 0$' "$PROM_FILE" 2>/dev/null; then
  ok "metrics enum-fail run: last_exit_ok=0 replaces the previous run's 1"
else
  bad "metrics enum-fail run: last_exit_ok not 0 after an aborted run"
  sed 's/^/       /' "$PROM_FILE" 2>/dev/null
fi
if grep -qE '^stellarindex_lake_dedup_running\{table="operations"\} 0$' "$PROM_FILE" 2>/dev/null; then
  ok "metrics enum-fail run: running=0 after the abort"
else
  bad "metrics enum-fail run: running gauge missing or not 0 after the abort"
  sed 's/^/       /' "$PROM_FILE" 2>/dev/null
fi
if grep -qE '^stellarindex_lake_dedup_partitions_processed\{table="operations"\} 0$' "$PROM_FILE" 2>/dev/null; then
  ok "metrics enum-fail run: partitions_processed=0, not the previous run's count"
else
  bad "metrics enum-fail run: partitions_processed not reset to 0"
  sed 's/^/       /' "$PROM_FILE" 2>/dev/null
fi
if [ -e "$PROM_FILE.progress" ]; then
  bad "metrics enum-fail run: progress state file left behind"
else
  ok "metrics enum-fail run: progress state file removed"
fi
if ls "$PRIV_TMP"/lake-dedup.* >/dev/null 2>&1; then
  bad "metrics enum-fail run: private progress directory left behind"
else
  ok "metrics enum-fail run: private progress directory removed"
fi

# ── 8b. a directory link planted at the .prom name is not written into ──
# mv onto a link to a directory moves the file INTO that directory.
LINK_DIR="$TMP/textfile-linkdir"; LINK_VICTIM="$TMP/planted-dir"
mkdir -p "$LINK_DIR" "$LINK_VICTIM"
ln -s "$LINK_VICTIM" "$LINK_DIR/lake_dedup_operations.prom"
run metrics_dir_link TEXTFILE_DIR="$LINK_DIR" -- operations
if [ -z "$(find "$LINK_VICTIM" -mindepth 1)" ] && [ -L "$LINK_DIR/lake_dedup_operations.prom" ]; then
  ok "metrics dir-link run: nothing is moved through a link planted at the .prom name"
else
  bad "metrics dir-link run: the driver moved its metrics file into the linked directory"
  find "$LINK_VICTIM" -mindepth 1 | sed 's/^/       /'
fi

# ── 9. SIGTERM mid-OPTIMIZE is a failed run, not a clean one ─────────
# The EXIT trap alone sees the last command's status on a signal. The fake
# OPTIMIZE blocks 30s; the driver must exit within 10s of SIGTERM, non-zero,
# with the client killed and the gauges reporting failure.
LOG="$TMP/log.metrics_sigterm"; : > "$LOG"
OPT_STARTED="$TMP/opt-started"; rm -f "$OPT_STARTED"
env -u ENUM_FAIL -u STATS_FAIL -u OPT_FAIL -u AFTER_FAIL -u DF_GARBAGE -u PROBE_FAIL \
    -u ZERO_DUP_PARTS -u DUP_ROWS \
    PATH="$TMP/bin:$PATH" CH="$TMP/bin/fake-ch" CH_LOG="$TMP/stmt.metrics_sigterm.log" \
    OUT="$LOG" STOP="$TMP/stop.metrics_sigterm" TEXTFILE_DIR="$PROMDIR" \
    STATS_BEFORE="$STATS_BEFORE_DEFAULT" ROWS_AFTER=50 DF_AVAIL=1000000000000 \
    ENUM_PARTS=$'0000000001\n' \
    OPT_BLOCK=30 OPT_STARTED="$OPT_STARTED" \
    bash "$DRIVER" operations >> "$LOG" 2>&1 &
driver_pid=$!
for _ in $(seq 100); do [ -s "$OPT_STARTED" ] && break; sleep 0.1; done
opt_pid="$(cat "$OPT_STARTED" 2>/dev/null)"
kill -TERM "$driver_pid"
exited=0
for _ in $(seq 100); do kill -0 "$driver_pid" 2>/dev/null || { exited=1; break; }; sleep 0.1; done
if [ "$exited" -eq 1 ]; then
  wait "$driver_pid"; RC=$?
else
  kill -KILL "$driver_pid" 2>/dev/null; wait "$driver_pid" 2>/dev/null; RC=137
fi
if [ -n "$opt_pid" ] && [ "$exited" -eq 1 ] && [ "$RC" -eq 143 ]; then
  ok "metrics sigterm run: exits 143 within 10s of SIGTERM"
else
  bad "metrics sigterm run: OPTIMIZE started=${opt_pid:-no} exited_promptly=$exited rc=$RC (want 143)"
  sed 's/^/       /' "$LOG"
fi
if [ -n "$opt_pid" ] && ! kill -0 "$opt_pid" 2>/dev/null; then
  ok "metrics sigterm run: in-flight clickhouse-client was stopped"
else
  bad "metrics sigterm run: clickhouse-client pid ${opt_pid:-?} outlived the driver"
  [ -n "$opt_pid" ] && kill -KILL "$opt_pid" 2>/dev/null
fi
if grep -qE '^stellarindex_lake_dedup_last_exit_ok\{table="operations"\} 0$' "$PROM_FILE" 2>/dev/null; then
  ok "metrics sigterm run: last_exit_ok=0"
else
  bad "metrics sigterm run: last_exit_ok not 0 after SIGTERM"
  sed 's/^/       /' "$PROM_FILE" 2>/dev/null
fi
if grep -qE '^stellarindex_lake_dedup_running\{table="operations"\} 0$' "$PROM_FILE" 2>/dev/null; then
  ok "metrics sigterm run: running=0"
else
  bad "metrics sigterm run: running gauge missing or not 0 after SIGTERM"
  sed 's/^/       /' "$PROM_FILE" 2>/dev/null
fi

echo "----"
echo "lake-dedup-driver-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
