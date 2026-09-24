#!/usr/bin/env bash
# lake-dedup-driver-test.sh — fixture tests for the two fail-open guards
# in deploy/clickhouse/lake-dedup-driver.sh (audit T429).
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
#      used to leave PARTS empty, so total=0, the for-loop never ran, and
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
      OUT="$LOG" STOP="$TMP/stop.$name" \
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
# same defect (T340/T357): a months-based proxy gets each case wrong.
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

echo "----"
echo "lake-dedup-driver-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
