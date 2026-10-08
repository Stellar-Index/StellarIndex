#!/usr/bin/env bash
# lake-dedup-driver-test.sh — fixture tests for the two fail-open guards
# in deploy/clickhouse/lake-dedup-driver.sh.
#
# clickhouse-client and df are STUBBED (fake-ch / fake-df, dropped in a
# throwaway PATH dir), so this runs anywhere in under a second and never
# touches a real lake. The stub records every statement handed to it and
# answers from fixture env vars, which is what makes the interesting
# assertion possible: not "the script printed something" but "OPTIMIZE
# was never issued" / "the run exits non-zero and says why".
#
# What is pinned, and why each is the actual defect (not an invented one):
#
#   1. the partition-enumeration query FAILING (auth, OOM, server down)
#      left PARTS empty, so total=0, the for-loop never ran, and
#      the driver logged "=== done: 0 partitions processed ===" and
#      exited 0 — a failed enumeration was indistinguishable from a
#      legitimate quiet run. It must now ABORT (exit 1) before that line.
#   2. a legitimate quiet run (the query succeeds, finds nothing) must
#      still exit 0 and log the zero — the fix must not turn "nothing to
#      do" into a false failure.
#   3. the disk-space guard failing OPEN on unparseable df output: `df`
#      printing something non-numeric made `[ "$free_bytes" -lt … ]`
#      error (exit 2), which `if` reads as false, skipping the ABORT and
#      falling straight through to an unguarded OPTIMIZE. The guard must
#      now validate free_bytes as digits BEFORE the comparison.
#   4. OPTIMIZE's own exit code was captured (`rc=$?`) but never acted
#      on — a failed merge was logged inline as just another partition
#      and the run still reported success. The driver must now abort on
#      a nonzero rc.
#   5. the ordinary success path (no failures injected) still completes,
#      processes every candidate partition, and reports the right count.
#   6. the <table> argument is spliced into SQL and the log path, so any
#      name outside the lake tables the driver is for is refused before
#      a single statement is issued.
#   7. the run's progress reaches monitoring as textfile-collector gauges
#      (stellarindex_lake_dedup_*), and the terminal state is written.
#   8. a run that ABORTs on the enumeration query, before the candidate
#      count is known, still overwrites the previous run's
#      last_exit_ok=1 with 0, so a failed run never reads as healthy.
#   9. a run stopped by SIGTERM mid-OPTIMIZE exits non-zero promptly,
#      takes its in-flight clickhouse-client with it, and records
#      last_exit_ok=0 / running=0 rather than a clean exit.
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
# Answers the driver's three query shapes from env fixtures, and
# appends every statement (whitespace squashed to one line) to $CH_LOG
# so a test can assert a statement was NEVER issued, not just that the
# run exited a certain way.
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
  *"sorting_key FROM system.tables"*)
    if [ -n "${SORTKEY_FAIL:-}" ]; then
      echo "Code: 60. DB::Exception: Table stellar.x does not exist." >&2
      exit 1
    fi
    printf '%s\n' "${SORTKEY_ANSWER:-ledger_seq}"
    ;;
  *"dup_rows > 0"*)
    if [ -n "${ENUM_FAIL:-}" ]; then
      echo "Code: 210. DB::NetException: Connection refused." >&2
      exit 1
    fi
    printf '%b' "${ENUM_PARTS:-}"
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
# Ignores its arguments (the driver always targets the same mount) and
# prints a header + one avail-bytes line, matching what
# `df --output=avail -B1 <dir> | tail -1` hands the driver.
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
  env -u ENUM_FAIL -u STATS_FAIL -u OPT_FAIL -u AFTER_FAIL -u DF_GARBAGE -u SORTKEY_FAIL \
      PATH="$TMP/bin:$PATH" CH="$TMP/bin/fake-ch" CH_LOG="$LOG_STMT" \
      OUT="$LOG" STOP="$TMP/stop.$name" TEXTFILE_DIR=/dev/null \
      STATS_BEFORE="$STATS_BEFORE_DEFAULT" ROWS_AFTER=50 DF_AVAIL=1000000000000 \
      ENUM_PARTS='' SORTKEY_ANSWER='ledger_seq' \
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
if grep -q 'dup-candidate partitions: 0' "$LOG" && grep -q '=== done: 0 partitions processed ===' "$LOG"; then
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

# ── 5b. sorting-key lookup FAILS ⇒ abort before the candidate query ──
run sortkey_fail SORTKEY_FAIL=1 -- transactions
if [ "$RC" -ne 0 ]; then
  ok "sorting-key lookup failure ⇒ non-zero exit"
else
  bad "sorting-key lookup failure ⇒ exited 0"
  sed 's/^/       /' "$LOG"
fi
if grep -q 'dup_rows > 0' "$LOG_STMT"; then
  bad "sorting-key lookup failure ⇒ candidate query was issued anyway"
  sed 's/^/       /' "$LOG_STMT"
else
  ok "sorting-key lookup failure ⇒ candidate query never issued"
fi

# ── 5c. the dup-candidate probe uses the table's real ORDER BY key, ──
# not a toYYYYMM(ingested_at) proxy — a partition spanning two ingest
# months with zero actual duplicate rows must not be flagged, and one
# with duplicates inside a single ingest month must be. Both are the
# same defect: a months-based proxy gets each case wrong.
run real_dup_probe SORTKEY_ANSWER='ledger_seq, tx_index' ENUM_PARTS=$'0000000003\n' -- transactions
if grep -q 'uniqExact((ledger_seq, tx_index))' "$LOG_STMT"; then
  ok "candidate query measures real duplicates via the table's own ORDER BY key"
else
  bad "candidate query does not use the table's sorting key"
  sed 's/^/       /' "$LOG_STMT"
fi
if grep -q 'toYYYYMM' "$LOG_STMT"; then
  bad "candidate query still uses the toYYYYMM(ingested_at) proxy heuristic"
  sed 's/^/       /' "$LOG_STMT"
else
  ok "candidate query no longer relies on the ingest-month proxy"
fi

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

# ── 7. textfile-collector metrics reach monitoring (F127) ────────────
# A multi-day dedup run must be observable beyond tailing $OUT.
# Point TEXTFILE_DIR at a real directory and assert the terminal .prom
# state reflects the run: running=0 (exited), partitions_processed=2
# (both candidates finished), last_exit_ok=1 (clean exit).
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
# The prior run's .prom (from case 7) says last_exit_ok=1, running=0.
# A later run that aborts before the candidate count is known must
# replace that with last_exit_ok=0, otherwise the failure is invisible.
run metrics_enum_fail ENUM_FAIL=1 TEXTFILE_DIR="$PROMDIR" -- operations
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

# ── 9. SIGTERM mid-OPTIMIZE is a failed run, not a clean one ─────────
# The EXIT trap alone sees the last command's status on a signal, so a
# SIGTERM'd run recorded last_exit_ok=1. The fake OPTIMIZE blocks for
# 30s; the driver must exit within 10s of SIGTERM, non-zero, with the
# client killed and the terminal gauges reporting the failure.
LOG="$TMP/log.metrics_sigterm"; : > "$LOG"
OPT_STARTED="$TMP/opt-started"; rm -f "$OPT_STARTED"
env -u ENUM_FAIL -u STATS_FAIL -u OPT_FAIL -u AFTER_FAIL -u DF_GARBAGE -u SORTKEY_FAIL \
    PATH="$TMP/bin:$PATH" CH="$TMP/bin/fake-ch" CH_LOG="$TMP/stmt.metrics_sigterm.log" \
    OUT="$LOG" STOP="$TMP/stop.metrics_sigterm" TEXTFILE_DIR="$PROMDIR" \
    STATS_BEFORE="$STATS_BEFORE_DEFAULT" ROWS_AFTER=50 DF_AVAIL=1000000000000 \
    ENUM_PARTS=$'0000000001\n' SORTKEY_ANSWER='ledger_seq' \
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
