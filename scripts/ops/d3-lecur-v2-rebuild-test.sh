#!/usr/bin/env bash
# d3-lecur-v2-rebuild-test.sh — fixture tests for the two IRREVERSIBLE
# decisions in scripts/ops/d3-lecur-v2-rebuild.sh (audit RLT-399).
#
# clickhouse-client is STUBBED, so this runs anywhere in about a second and
# never reaches a lake. The stub RECORDS every statement it is handed and
# answers the read-only aggregates from fixture values, which is what makes
# the interesting assertion possible: not "the script printed a warning"
# but "the DROP / RENAME / INSERT never left the script".
#
# What is pinned, and why each one is a data loss if it regresses:
#
#   1. cutover REFUSES when v2's min(ledger_seq) is above v1's. That is the
#      migration artifact's own rule (Step 2 of
#      deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql: the
#      current min "preserves today's coverage floor"), and the runbook's
#      `reproject 38000000 <tip>` raises the floor on any v1 reaching below
#      38,000,000. The RENAME is where rollback-precutover stops applying.
#   2. cutover REFUSES on an empty v2, on a v2 whose max lags v1's (a v2 MV
#      that is not capturing live ingest), and on a coverage query that did
#      not come back at all — r1's state today, where the 2026-07-29 cutover
#      completed and there is no longer a ledger_entries_current_v2. Unfixed,
#      that case DROPPED the live ledger_entries_current_mv before failing.
#   3. a refusal issues NO DROP, NO RENAME and NO INSERT — v1 keeps serving.
#   4. D3_FORCE_CUTOVER=yes, and only that, overrides — the same explicit
#      acknowledgement finalize / rollback-precutover already require.
#   5. a v2 that covers v1 still cuts over, and the coverage reads happen
#      BEFORE the first DROP (a gate that runs after the DDL is not a gate).
#   6. reproject progress is keyed by the window: a mark left by
#      [54000000,63050000] must not be adopted by a later [2,38000000].
#      Unkeyed, that run inserted NOTHING and logged "complete" — and that
#      ordering is the normal one (phaseD-backfill.sh walks the high range
#      first). The pre-fix shape is reproduced here by planting the legacy
#      unkeyed state file.
#   7. a same-window mark IS still honoured (resume must keep working), a
#      mark below its own window start is refused as corrupt, and an empty
#      window is refused outright.
#
# Run: bash scripts/ops/d3-lecur-v2-rebuild-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
D3="$PWD/scripts/ops/d3-lecur-v2-rebuild.sh"
[ -r "$D3" ] || { echo "d3-lecur-v2-rebuild-test: missing $D3" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# ─── the fake clickhouse-client ─────────────────────────────────────
#
# Answers only the three read-only shapes the script asks for, from
# environment fixtures, and appends every statement (whitespace squashed to
# one line) to $CH_LOG. Everything else — DROP, RENAME, CREATE, INSERT —
# succeeds silently and is judged by its presence in the log.
#
# V1_COV / V2_COV are "count<TAB>min<TAB>max" as `FORMAT TSV` would return
# them; TIP answers max(ledger_seq) over the append-log.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/fake-ch" <<'STUB'
#!/usr/bin/env bash
sql=""
while [ $# -gt 0 ]; do
  case "$1" in
    -q) sql="${2-}"; shift 2 ;;
    *)  shift ;;
  esac
done
# One line per statement: squash the embedded newlines/tabs, but keep the
# record's OWN terminator — the ordering assertion counts log lines.
printf '%s\n' "$(printf '%s' "$sql" | tr '\n\t' '  ' | tr -s ' ')" >> "$CH_LOG"
case "$sql" in
  *"count()"*ledger_entries_current_v2*)
    # CH_FAIL_V2 models a table that does not exist: clickhouse-client
    # prints its error on stderr and exits non-zero, so the caller's
    # capture is EMPTY.
    [ -n "${CH_FAIL_V2:-}" ] && { echo "Code: 60. DB::Exception: Table stellar.ledger_entries_current_v2 does not exist." >&2; exit 1; }
    printf '%b\n' "$V2_COV" ;;
  *"count()"*ledger_entries_current*)    printf '%b\n' "$V1_COV" ;;
  *"max(ledger_seq) FROM stellar.ledger_entry_changes"*) printf '%s\n' "$TIP" ;;
esac
exit 0
STUB
chmod +x "$TMP/bin/fake-ch"

# d3 <case-name> [ENV=VAL …] -- <phase args…>
# Leaves the statement log in $CH_LOG, stdout+stderr in $OUT, status in $RC.
d3() {
  local name="$1"; shift
  local -a envs=()
  while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do envs+=("$1"); shift; done
  shift   # the --
  CH_LOG="$TMP/ch.$name.log"; : > "$CH_LOG"
  OUT="$TMP/out.$name"
  env -u D3_FORCE_CUTOVER -u CH_FAIL_V2 \
      CH_LOG="$CH_LOG" CH="$TMP/bin/fake-ch" \
      D3_STATE="$TMP/state.$name" CH_FLAGS_DIR="$TMP/flags" \
      V1_COV="0\t0\t0" V2_COV="0\t0\t0" TIP=63700000 \
      ${envs[@]+"${envs[@]}"} bash "$D3" "$@" > "$OUT" 2>&1
  RC=$?
}

# no_ddl <case-name> <label> — the destructive statements must be absent.
no_ddl() {
  local log="$TMP/ch.$1.log" label="$2" hit
  hit="$(grep -Eic 'DROP TABLE|RENAME TABLE|INSERT INTO' "$log")"
  if [ "$hit" -eq 0 ]; then
    ok "$label: no DROP / RENAME / INSERT reached ClickHouse"
  else
    bad "$label: $hit destructive statement(s) issued despite the refusal"
    sed 's/^/       /' "$log"
  fi
}

echo "d3-lecur-v2-rebuild-test: cutover coverage gate + per-window reproject state"

# ─── 1. cutover refuses a raised coverage floor ─────────────────────
#
# r1's shape: v1 reaches down to ledger 2 (the genesis-onward re-derive),
# v2 was reprojected from the runbook's 38000000 only. Same row order of
# magnitude, same tip — ONLY the floor differs, which is exactly the case
# a count-only or tip-only glance would wave through.
d3 floor V1_COV='900000000\t2\t63700000' V2_COV='900000001\t38000000\t63700000' -- cutover
if [ "$RC" -ne 0 ]; then
  ok "raised floor (v2 min 38000000 > v1 min 2) ⇒ non-zero exit"
else
  bad "raised floor ⇒ exited 0; an incomplete v2 was swapped over a complete v1"
  sed 's/^/       /' "$OUT"
fi
if grep -q 'coverage floor would REGRESS: v2 min(ledger_seq)=38000000 is above v1'"'"'s 2' "$OUT"; then
  ok "raised floor ⇒ names both floors in the refusal"
else
  bad "raised floor ⇒ refusal does not state v2 min 38000000 vs v1 min 2"
  sed 's/^/       /' "$OUT"
fi
no_ddl floor "raised floor"

# ─── 2. cutover refuses an empty v2, and a v2 behind the tip ────────
d3 empty V1_COV='900000000\t2\t63700000' V2_COV='0\t0\t0' -- cutover
if [ "$RC" -ne 0 ] && grep -q 'v2 is EMPTY' "$OUT"; then
  ok "empty v2 ⇒ refused, and says so"
else
  bad "empty v2 ⇒ expected a non-zero exit naming an EMPTY v2 (rc=$RC)"
  sed 's/^/       /' "$OUT"
fi
no_ddl empty "empty v2"

d3 lag V1_COV='900000000\t2\t63700000' V2_COV='900000000\t2\t63000000' -- cutover
if [ "$RC" -ne 0 ] && grep -q 'v2 lags the tip: v2 max(ledger_seq)=63000000' "$OUT"; then
  ok "v2 behind v1's tip ⇒ refused, and says so"
else
  bad "v2 behind v1's tip ⇒ expected a non-zero exit naming the lag (rc=$RC)"
  sed 's/^/       /' "$OUT"
fi
no_ddl lag "v2 behind the tip"

# r1's state TODAY (measured 2026-09-19): the 2026-07-29 cutover completed and
# finalize dropped _old, so ledger_entries_current is already the
# ReplacingMergeTree(version) shape and there is NO ledger_entries_current_v2.
# A cutover re-run therefore gets a FAILED query, not a number — and a failed
# clickhouse-client query yields an empty capture, which must refuse rather
# than be compared or interpolated. CH_FAIL_V2 makes the stub exit non-zero on
# the v2 aggregate, exactly as a missing table does.
d3 missing CH_FAIL_V2=1 V1_COV='900000000\t2\t63700000' -- cutover
if [ "$RC" -ne 0 ] && grep -q 'not a non-negative integer' "$OUT"; then
  ok "a v2 aggregate that did not come back ⇒ refused, not compared"
else
  bad "a failed v2 coverage query did not refuse (rc=$RC)"
  sed 's/^/       /' "$OUT"
fi
no_ddl missing "missing v2 table"

# ─── 3. only the explicit acknowledgement overrides ─────────────────
d3 force-wrong D3_FORCE_CUTOVER=1 V1_COV='900000000\t2\t63700000' V2_COV='900000001\t38000000\t63700000' -- cutover
if [ "$RC" -ne 0 ]; then
  ok "D3_FORCE_CUTOVER=1 (not 'yes') ⇒ still refused"
else
  bad "D3_FORCE_CUTOVER=1 was accepted as an acknowledgement"
fi
no_ddl force-wrong "D3_FORCE_CUTOVER=1"

d3 force-yes D3_FORCE_CUTOVER=yes V1_COV='900000000\t2\t63700000' V2_COV='900000001\t38000000\t63700000' -- cutover
if [ "$RC" -eq 0 ] && grep -q 'RENAME TABLE' "$TMP/ch.force-yes.log"; then
  ok "D3_FORCE_CUTOVER=yes ⇒ proceeds over the acknowledged violation"
else
  bad "D3_FORCE_CUTOVER=yes ⇒ expected the cutover to run (rc=$RC)"
  sed 's/^/       /' "$OUT"
fi

# ─── 4. a covering v2 still cuts over, and the gate runs FIRST ──────
d3 good V1_COV='900000000\t2\t63700000' V2_COV='920000000\t2\t63700000' -- cutover
if [ "$RC" -eq 0 ] && grep -q 'RENAME TABLE' "$TMP/ch.good.log"; then
  ok "v2 covering v1 ⇒ cutover proceeds (the gate is not simply always-refuse)"
else
  bad "v2 covering v1 ⇒ cutover was refused; the gate is too strict (rc=$RC)"
  sed 's/^/       /' "$OUT"
fi
# Land each match list in a variable before slicing it: piping into head
# under `set -o pipefail` is the SIGPIPE trap lint-shell-sigpipe rejects.
ddl_lines="$(grep -n -Ei 'DROP TABLE|RENAME TABLE' "$TMP/ch.good.log")"
gate_lines="$(grep -n 'count(), min(ledger_seq)' "$TMP/ch.good.log")"
first_ddl="${ddl_lines%%:*}"            # first match's line number
last_gate="${gate_lines##*$'\n'}"; last_gate="${last_gate%%:*}"
if [ -n "$first_ddl" ] && [ -n "$last_gate" ] && [ "$last_gate" -lt "$first_ddl" ]; then
  ok "both coverage reads precede the first DROP/RENAME"
else
  bad "coverage reads did not precede the DDL (gate line $last_gate, first DDL line $first_ddl)"
  sed 's/^/       /' "$TMP/ch.good.log"
fi

# ─── 5. reproject state is per-window ───────────────────────────────
#
# The pre-fix shape, planted: a run over the HIGH range left an unkeyed
# mark at 63050000. The next range is the LOW one — phaseD-backfill.sh's
# real order — and it must insert, not report itself complete.
mkdir -p "$TMP/state.lowwindow"
printf '63050000\n' > "$TMP/state.lowwindow/reproject-progress"
d3 lowwindow D3_CHUNK=10000000 -- reproject 2 38000000
inserts="$(grep -c 'INSERT INTO stellar.ledger_entries_current_v2' "$TMP/ch.lowwindow.log")"
if [ "$RC" -eq 0 ] && [ "$inserts" -gt 0 ]; then
  ok "[2,38000000) after a [54000000,63050000) mark ⇒ $inserts INSERT(s) issued"
else
  bad "[2,38000000) after a higher window's mark ⇒ issued $inserts INSERTs (rc=$RC): the window was skipped"
  sed 's/^/       /' "$OUT"
fi
if grep -q 'WHERE ledger_seq >= 2 AND ledger_seq < 10000002' "$TMP/ch.lowwindow.log"; then
  ok "the low window starts at its own FROM (2), not at the other window's mark"
else
  bad "the first INSERT does not start at ledger 2"
  sed 's/^/       /' "$TMP/ch.lowwindow.log"
fi
if grep -q 'reproject \[2,38000000) complete' "$OUT"; then
  ok "completion is reported for the window that was actually requested"
else
  bad "completion line does not name [2,38000000)"
  sed 's/^/       /' "$OUT"
fi

# ─── 6. resume within the SAME window still works ───────────────────
mkdir -p "$TMP/state.resume"
printf '38500000\n' > "$TMP/state.resume/reproject-progress.from-38000000"
d3 resume D3_CHUNK=100000 -- reproject 38000000 38700000
ins_bounds="$(grep -o 'ledger_seq >= [0-9]* AND ledger_seq < [0-9]*' "$TMP/ch.resume.log")"
first_ins="${ins_bounds%%$'\n'*}"
if [ "$first_ins" = 'ledger_seq >= 38500000 AND ledger_seq < 38600000' ]; then
  ok "a mark for this window is still honoured (first INSERT resumes at 38500000)"
else
  bad "same-window resume broke — first INSERT was '$first_ins', expected it at 38500000"
  sed 's/^/       /' "$TMP/ch.resume.log"
fi
if grep -q 'WHERE ledger_seq >= 38000000 ' "$TMP/ch.resume.log"; then
  bad "resume re-inserted a chunk below the mark"
else
  ok "resume did not redo the covered part of its own window"
fi

# ─── 7. corrupt / impossible windows are refused, not guessed ───────
mkdir -p "$TMP/state.corrupt"
printf '100\n' > "$TMP/state.corrupt/reproject-progress.from-38000000"
d3 corrupt -- reproject 38000000 38700000
if [ "$RC" -ne 0 ] && grep -q 'is below that file' "$OUT"; then
  ok "a mark below its own window start ⇒ refused as corrupt"
else
  bad "a mark of 100 in the from-38000000 file was accepted (rc=$RC)"
  sed 's/^/       /' "$OUT"
fi
no_ddl corrupt "corrupt mark"

d3 emptywin -- reproject 38000000 38000000
if [ "$RC" -ne 0 ] && grep -q 'empty window' "$OUT"; then
  ok "an empty window [38000000,38000000) ⇒ refused"
else
  bad "an empty window was accepted (rc=$RC)"
  sed 's/^/       /' "$OUT"
fi
no_ddl emptywin "empty window"

d3 nonnum -- reproject 38000000 '38000000; DROP'
if [ "$RC" -ne 0 ] && grep -q 'not a non-negative integer' "$OUT"; then
  ok "a non-numeric to-ledger ⇒ refused before any statement is built"
else
  bad "a non-numeric to-ledger was interpolated (rc=$RC)"
  sed 's/^/       /' "$OUT"
fi
no_ddl nonnum "non-numeric to-ledger"

echo "d3-lecur-v2-rebuild-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
