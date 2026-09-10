#!/usr/bin/env bash
# timescale-jobs-probe-test.sh — fixture tests for the TimescaleDB
# job/CAGG probe the archival-node role installs as
# /usr/local/sbin/timescale-jobs-probe.sh.
#
# THE DEFECT THESE PIN. The probe's psql helper ended in
# `2>/dev/null || true`, so a failing query returned an empty result AND
# exit 0. The file was rewritten without that query's families,
# node_exporter served it, and stellarindex_timescale_cagg_stale,
# _compression_lag and _job_failures_climbing evaluated over absent
# series — `>` and `time() - x` over an empty vector are empty, so none
# of them could fire on exactly the runs they exist for. A probe that
# cannot report its own failure reads exactly like a healthy one.
#
# What must hold:
#   1. a clean run emits every data family AND, per query, an
#      _probe_query_ok of 1 with a non-zero _probe_rows, plus one
#      _probe_last_run_unix stamp;
#   2. ONE failing query costs only its own family — the other two are
#      still written (a partial file beats leaving the previous one on
#      disk forever) — and that query reports _probe_query_ok 0;
#   3. a query that exits 0 and returns NO rows is distinguishable from
#      one that succeeded: ok 1, rows 0. That is the other way a family
#      disappears (renamed view, filter that stopped matching) and it
#      never raises an exit status;
#   4. the stamp is written even when every query fails, so "producing
#      bad data" and "not running" stay different signals;
#   5. the output is valid Prometheus text (a malformed textfile makes
#      node_exporter drop the WHOLE file, i.e. all families at once),
#      it is world-readable, and no temp file survives;
#   6. the PG lock-convoy gauges (2026-09-10) carry their values
#      intact, EMIT an explicit zero on a quiet database, and emit
#      nothing at all when their query failed. They live in this probe
#      rather than in postgres_exporter because the exporter was
#      itself queued in the convoy they exist to report — three of its
#      scrapes blocked 917 s while a decompress_chunk's pending
#      AccessExclusiveLock held the queue. A gauge that vanished at
#      zero would leave the alert unable to distinguish "no convoy"
#      from "no producer", which is the same blindness again.
#
# The SHIPPED bytes are executed, not a hand-copied twin: the script is
# an inline `content:` block in the role, so it is extracted from the
# task YAML the way ansible would render it (same idiom as
# pgbackrest-backup-test.sh, which runs its role template directly).
# `runuser` is stubbed on PATH and TEXTFILE_DIR is redirected into a
# temp dir, so there is no Postgres, no systemd and no write outside
# $TMPDIR.
#
# Run: bash scripts/ci/timescale-jobs-probe-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SRC="$PWD/configs/ansible/roles/archival-node/tasks/10-observability.yml"
[[ -r "$SRC" ]] || { echo "timescale-jobs-probe-test: missing $SRC" >&2; exit 2; }

command -v python3 >/dev/null 2>&1 || {
  echo "timescale-jobs-probe-test: python3 is required to render the task's inline script" >&2
  exit 2
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

PROBE="$TMP/timescale-jobs-probe.sh"
TASK_NAME="TimescaleDB job/CAGG health probe (script)"
if ! TASK_NAME="$TASK_NAME" SRC="$SRC" DEST="$PROBE" python3 - <<'PY'
import io, os, sys
try:
    import yaml
except ImportError:
    print("timescale-jobs-probe-test: FAIL — PyYAML is required (pip install pyyaml); "
          "refusing to pass vacuously", file=sys.stderr)
    sys.exit(2)
doc = yaml.safe_load(io.open(os.environ["SRC"], encoding="utf-8"))
want = os.environ["TASK_NAME"]
for task in doc or []:
    if isinstance(task, dict) and task.get("name") == want:
        io.open(os.environ["DEST"], "w", encoding="utf-8").write(
            task["ansible.builtin.copy"]["content"])
        sys.exit(0)
print(f"timescale-jobs-probe-test: FAIL — no task named {want!r} in {os.environ['SRC']}",
      file=sys.stderr)
sys.exit(2)
PY
then
  exit 2
fi
chmod +x "$PROBE"

# ─── stubs ──────────────────────────────────────────────────────────
#
# runuser stub: answers the three queries by matching their distinctive
# SQL, driven by $SCENARIO. Fails or empties one query at a time so the
# blast radius of a single failure is observable.
mkdir -p "$TMP/bin" "$TMP/textfile"
cat > "$TMP/bin/runuser" <<'SH'
#!/usr/bin/env bash
# `runuser -u postgres -- psql -d stellarindex -At -F'|' -c "<sql>"`
sql=""; prev=""
for a in "$@"; do
  [[ "$prev" == "-c" ]] && sql="$a"
  prev="$a"
done
kind=other
case "$sql" in
  *pg_blocking_pids*)                    kind=lock_convoy ;;
  *policy_refresh_continuous_aggregate*) kind=cagg ;;
  *policy_compression*)                  kind=compression ;;
  *job_stats\ js*)                       kind=jobs ;;
esac
for f in ${FAIL_QUERIES:-}; do
  [[ "$kind" == "$f" ]] && { echo 'ERROR:  permission denied' >&2; exit 1; }
done
for e in ${EMPTY_QUERIES:-}; do
  [[ "$kind" == "$e" ]] && exit 0
done

# ── FIDELITY: psql narrates every statement, not only the last ──────
#
# THIS IS THE POINT OF THE STUB. The previous cut answered whatever SQL
# it was handed with well-formed rows and nothing else — a psql that
# cannot exist. `-At` suppresses the column header and the row
# separator; it does NOT suppress the per-statement command tag. Handed
#
#     SET statement_timeout = '10s'; SELECT count(*), …
#
# the real client writes
#
#     SET
#     5|1164|7
#
# and the probe's `IFS='|' read -r convoyed worst blocked` takes `SET`
# as its first field. That is r1 2026-09-10, and this file passed 34/34
# with it live: a test that supplies its own well-formed input cannot
# see a malformed-input bug. So the narration is now DERIVED from the
# SQL — every statement before the last one puts a line on stdout ahead
# of the rows, exactly as psql does. Re-introduce a multi-statement
# command string and this stub reproduces the incident.
rest="$sql"
while [[ "$rest" == *';'* ]]; do
  stmt="${rest%%;*}"
  rest="${rest#*;}"
  # A trailing `;` terminates the final statement rather than
  # introducing another one.
  [[ -z "$(printf '%s' "$rest" | tr -d '[:space:]')" ]] && break
  word=$(printf '%s' "$stmt" | tr '\n\t' '  ' | sed -E 's/^ +//; s/ .*$//' | tr '[:lower:]' '[:upper:]')
  case "$word" in
    '')                            ;;
    SELECT|WITH|VALUES|TABLE|SHOW) echo 0 ;;      # a non-final query prints its ROWS
    *)                             echo "$word" ;; # everything else prints its command tag
  esac
done

case "$kind" in
  # The 2026-09-10 shape: five backends convoyed behind a decompress
  # that is ITSELF blocked, worst wait 1,164 s, seven blocked in all.
  lock_convoy) printf "${LOCK_CONVOY_ROW:-5|1164|7}\n" ;;
  cagg)        printf "${CAGG_ROWS:-prices_1m|1788598634|60
oracle_prices_1m|1788598600|30}\n" ;;
  compression) printf 'trades|3\nfx_quotes|0\n' ;;
  jobs)        printf 'policy_compression|trades|1001|0\npolicy_retention|-|1002|2\n' ;;
  *)           echo "runuser stub: unmatched sql" >&2; exit 9 ;;
esac
exit 0
SH
chmod +x "$TMP/bin/runuser"
export PATH="$TMP/bin:$PATH"
export TEXTFILE_DIR="$TMP/textfile"
PROM="$TEXTFILE_DIR/timescale_jobs.prom"

# run <fail-queries> <empty-queries> [convoy-row] [cagg-rows] — one probe
# run; sets $RC and leaves the probe's stderr in $ERR.
run() {
  rm -f "$PROM"
  ERR="$(FAIL_QUERIES="$1" EMPTY_QUERIES="$2" LOCK_CONVOY_ROW="${3:-}" CAGG_ROWS="${4:-}" \
    bash "$PROBE" 2>&1 >/dev/null)"
  RC=$?
}

# metric <exact-name-with-labels> — the value, or the empty string.
metric() {
  awk -v want="$1" '$1 == want { print $2 }' "$PROM"
}

has_family() { grep -qE "^$1\{" "$PROM"; }

# ─── assertion helpers ──────────────────────────────────────────────
#
# `if/then/else`, not `A && ok "…" || bad "…"`. The third arm of that
# idiom runs whenever the second one fails (shellcheck SC2015), and
# CI's changed-script shellcheck gate is clean-or-nothing — so a file
# carrying that pattern cannot be edited without turning the gate red
# for whoever touches it next. Same shape as data-freshness-test.sh.

# eq <want> <got> <label> — assert an exact value.
eq() {
  if [[ "$2" == "$1" ]]; then ok "$3"; else bad "$3 (got '${2:-<absent>}', want '$1')"; fi
}

# matches <regex> <got> <label> — assert a pattern. The regex is
# deliberately unquoted inside [[ =~ ]]; quoting it would match it as a
# literal.
matches() {
  if [[ "$2" =~ $1 ]]; then ok "$3"; else bad "$3 (got '${2:-<absent>}')"; fi
}

# holds <label> <cmd...> — assert the command succeeds.
holds() { local label="$1"; shift; if "$@"; then ok "$label"; else bad "$label"; fi; }

# refutes <label> <cmd...> — assert the command FAILS.
refutes() { local label="$1"; shift; if "$@"; then bad "$label"; else ok "$label"; fi; }

# ─── 1. clean run ───────────────────────────────────────────────────
run "" ""
eq 0 "$RC" "clean run exits 0"
for f in stellarindex_cagg_last_refresh_unix \
         stellarindex_timescale_chunks_overdue_compression \
         stellarindex_timescale_job_failures_total; do
  holds "clean run emits $f" has_family "$f"
done
for qname in cagg_refresh compression job_stats; do
  eq 1 "$(metric "stellarindex_timescale_probe_query_ok{query=\"$qname\"}")" "clean run: query_ok $qname = 1"
  eq 2 "$(metric "stellarindex_timescale_probe_rows{query=\"$qname\"}")" "clean run: rows $qname = 2"
done
matches '^1[0-9]{9}$' "$(metric stellarindex_timescale_probe_last_run_unix)" "clean run stamps last_run_unix"

# 0644, atomic: node_exporter runs unprivileged and SILENTLY skips a
# file it cannot read, and a surviving temp file would be scraped as a
# duplicate of every series in it.
#
# shellcheck disable=SC2012  # $PROM is a fixed literal name this script created in $TMPDIR; the find-instead-of-ls advice is about filenames we control here
mode=$(ls -l "$PROM" | cut -c1-10)
eq "-rw-r--r--" "$mode" "textfile is 0644"
leftovers=$(find "$TEXTFILE_DIR" -type f ! -name 'timescale_jobs.prom' | wc -l | tr -d ' ')
eq 0 "$leftovers" "no temp file survives"

# ─── 1b. the lock-convoy gauges (2026-09-10) ────────────────────────
#
# The alerting layer went cascade-blind during the incident because
# postgres_exporter was itself queued in the convoy. These gauges are
# the independent carrier, so what they have to prove here is that the
# numbers survive the trip intact and that a value of zero is EMITTED
# rather than omitted — an absent series is how an alert quietly stops
# being able to fire.
#
# The convoy gauges carry no labels (they are facts about the whole
# cluster, not about one hypertable), which `metric` already handles:
# it matches the whole first field.
run "" ""
eq 5 "$(metric stellarindex_pg_lock_convoy_backends)" "convoy backends carried through"
eq 1164 "$(metric stellarindex_pg_lock_convoy_wait_seconds_max)" "worst convoyed wait carried through"
eq 7 "$(metric stellarindex_pg_lock_blocked_backends)" "total blocked backends carried through"
eq 1 "$(metric "stellarindex_timescale_probe_query_ok{query=\"lock_convoy\"}")" "clean run: query_ok lock_convoy = 1"

# A quiet database is the common case and it must still EMIT. An alert
# built on a series that disappears when the value is zero cannot
# distinguish "no convoy" from "no producer".
run "" "" "0|0|0"
eq 0 "$(metric stellarindex_pg_lock_convoy_wait_seconds_max)" "a quiet database emits an explicit 0, not an absent series"

# A failing convoy query must not cost the other three families, and
# must report itself — the probe-degraded alert is what carries that.
run "lock_convoy" ""
eq 0 "$(metric "stellarindex_timescale_probe_query_ok{query=\"lock_convoy\"}")" "a failing convoy query reports query_ok 0"
holds "a failing convoy query does not cost the cagg family" \
  has_family stellarindex_cagg_last_refresh_unix
eq "" "$(metric stellarindex_pg_lock_convoy_backends)" \
  "a failing convoy query emits no convoy value (absent, never a fabricated 0)"
run "" ""

# ─── 2. ONE failing query ───────────────────────────────────────────
#
# The degrade is deliberate: dying here would fire the EXIT trap, skip
# the `mv`, and leave the PREVIOUS file on disk to be re-scraped with a
# fresh sample timestamp forever. So the other two families must still
# land — and the failure must still be visible.
run "compression" ""
eq 0 "$RC" "one failing query still writes the file"
holds "failing compression query does not cost the cagg family" \
  has_family stellarindex_cagg_last_refresh_unix
holds "failing compression query does not cost the job-failure family" \
  has_family stellarindex_timescale_job_failures_total
eq 0 "$(metric 'stellarindex_timescale_probe_query_ok{query="compression"}')" "failing query reports query_ok 0"
eq 1 "$(metric 'stellarindex_timescale_probe_query_ok{query="cagg_refresh"}')" "the queries that worked still report 1"
eq 0 "$(metric 'stellarindex_timescale_probe_rows{query="compression"}')" "failing query reports rows 0"

# ─── 3. a query that succeeds and returns nothing ───────────────────
run "" "compression"
eq 1 "$(metric 'stellarindex_timescale_probe_query_ok{query="compression"}')" "empty result keeps query_ok 1"
eq 0 "$(metric 'stellarindex_timescale_probe_rows{query="compression"}')" "empty result reports rows 0"
refutes "an empty result emits no compression series" \
  has_family stellarindex_timescale_chunks_overdue_compression

# ─── 4. every query failing still stamps the run ────────────────────
run "cagg compression jobs lock_convoy" ""
matches '^1[0-9]{9}$' "$(metric stellarindex_timescale_probe_last_run_unix)" "a fully-failing run still stamps last_run_unix"
zeros=$(grep -c '^stellarindex_timescale_probe_query_ok{.*} 0$' "$PROM")
eq 4 "$zeros" "a fully-failing run reports all four queries as 0"

# ─── 5. the file parses as Prometheus text ──────────────────────────
#
# One malformed line makes node_exporter reject the WHOLE file, taking
# every family with it — the blindness this probe's health metrics
# exist to report, arriving by a different door.
if command -v promtool >/dev/null 2>&1; then
  for sc in "" "compression"; do
    run "$sc" ""
    if promtool check metrics < "$PROM" >/dev/null 2>&1; then
      ok "output parses as Prometheus text (fail-queries='${sc:-none}')"
    else
      bad "output parses as Prometheus text (fail-queries='${sc:-none}')"
    fi
  done
else
  echo "  note promtool not installed — skipping the promtool text-format assertions" >&2
fi

# parses <label> — assert $PROM parses, using the repo's own exposition
# parser rather than promtool (not installed on every runner) and rather
# than a copy of the probe's awk (which would only prove the copy agrees
# with itself).
parses() {
  if python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM" >/dev/null 2>&1; then
    ok "$1"
  else
    bad "$1
$(python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM" 2>&1 | sed 's/^/        /')"
  fi
}

# ─── 6. the reply is an EXTERNAL input (2026-09-10) ──────────────────
#
# psql narrates: a command tag ahead of the rows, a NULL rendered as an
# empty field, a short row leaving later fields unset. `${x:-0}` cannot
# tell a number from a word, so on 2026-09-10 the word `SET` was written
# as a metric value and node_exporter rejected the WHOLE file — the three
# convoy gauges and, as collateral, 127 pre-existing
# stellarindex_timescale_* series that merely share timescale_jobs.prom.
#
# These drive the SHIPPED probe with the shapes a real client produces.
# The destructive branch (a row dropped) is exercised WITH its trigger,
# and the untriggered rows in the same reply are asserted to survive
# with their exact values — a guard tested only on input it never fires
# on certifies the one shape that was never at risk.

# 6a. The stub's fidelity is itself under test. If it stops narrating,
#     every assertion here silently becomes an assertion about a psql
#     that cannot exist — which is exactly how 2026-09-10 shipped green.
tag_reply="$(runuser -u postgres -- psql -d stellarindex -At -F'|' \
  -c "SET statement_timeout = '10s'; SELECT count(*) FROM pg_stat_activity a WHERE pg_blocking_pids(a.pid) <> '{}'")"
# Sliced with parameter expansion, not `| head -1`: head exits after the
# first line and under pipefail that SIGPIPEs the writer
# (scripts/ci/lint-shell-sigpipe.sh).
tag_first="${tag_reply%%$'\n'*}"
tag_second="${tag_reply#*$'\n'}"
tag_second="${tag_second%%$'\n'*}"
eq "SET" "$tag_first" \
  "the psql stub prints a command tag for a leading SET, as -At does not suppress it"
eq "5|1164|7" "$tag_second" \
  "…and still returns the row after it"

# 6b. A command tag reaching the convoy read loop. The tag row is
#     dropped, the REAL row survives byte-for-byte, and the query says
#     it is not healthy.
run "" "" "SET
5|1164|7"
eq 0 "$RC" "a command-tag reply still completes the run"
parses "a command-tag reply still produces a parseable file"
eq 5 "$(metric stellarindex_pg_lock_convoy_backends)" \
  "the real convoy row survives a command tag ahead of it, with its exact value"
eq 1164 "$(metric stellarindex_pg_lock_convoy_wait_seconds_max)" \
  "…and so does the worst wait"
refutes "the command tag never reaches the file as a value" grep -q 'SET' "$PROM"
eq 0 "$(metric 'stellarindex_timescale_probe_query_ok{query="lock_convoy"}')" \
  "a reply the loop could not parse reports query_ok 0 (the degraded alert already reads this)"
eq 1 "$(metric "stellarindex_timescale_probe_rows{query=\"lock_convoy\"}")" \
  "…and rows counts what was actually emitted, not what was read"
holds "a command tag in ONE query does not cost the cagg family" \
  has_family stellarindex_cagg_last_refresh_unix
eq 1 "$(metric 'stellarindex_timescale_probe_query_ok{query="cagg_refresh"}')" \
  "…and the queries that were fine still report 1"

# 6c. A NULL column. `-At` renders NULL as an EMPTY field, and
#     schedule_interval is the one value in this probe with no COALESCE
#     around it — an empty value field is a metric name with nothing
#     after it, which costs the file exactly as hard as `SET` did.
run "" "" "" "prices_1m|1788598634|60
oracle_prices_1m|1788598600|"
parses "a NULL interval still produces a parseable file"
eq 1788598634 "$(metric 'stellarindex_cagg_last_refresh_unix{cagg="prices_1m"}')" \
  "the healthy cagg row is untouched by its neighbour's NULL"
eq "" "$(metric 'stellarindex_cagg_refresh_interval_seconds{cagg="oracle_prices_1m"}')" \
  "the NULL row emits nothing rather than a metric name with no value"
eq 0 "$(metric 'stellarindex_timescale_probe_query_ok{query="cagg_refresh"}')" \
  "a NULL in a value column reports query_ok 0"

# 6d. Ordinary input is untouched. The guard is lossy, so the case that
#     matters beside the one above is that nothing else moves: a clean
#     run emits the same rows, values and count it always did.
run "" ""
parses "a clean run parses"
eq 5 "$(metric stellarindex_pg_lock_convoy_backends)" "clean run: convoy value unchanged by the guard"
eq 2 "$(metric "stellarindex_timescale_probe_rows{query=\"cagg_refresh\"}")" "clean run: both cagg rows still emitted"
eq 1 "$(metric 'stellarindex_timescale_probe_query_ok{query="cagg_refresh"}')" "clean run: cagg still reports healthy"
eq 60 "$(metric 'stellarindex_cagg_refresh_interval_seconds{cagg="prices_1m"}')" "clean run: interval carried through intact"

# 6e. The last line of defence. Values cannot now be non-numeric, but the
#     cost of being wrong about that is every family in the file, so the
#     probe parses its own rendered bytes before the atomic `mv`. A label
#     carrying a brace is the shape that gets past a value guard and is
#     still fatal to the file; the probe must REFUSE to publish it and
#     leave the previous file alone, because a published malformed file
#     is silent while a frozen one ages into the degraded alert.
printf 'stellarindex_sentinel 1\n' > "$PROM"
sentinel_before="$(cat "$PROM")"
ERR="$(FAIL_QUERIES='' EMPTY_QUERIES='' LOCK_CONVOY_ROW='' CAGG_ROWS='pri}ces|1|2' \
  bash "$PROBE" 2>&1 >/dev/null)"; RC=$?
if [[ "$RC" -ne 0 ]]; then ok "an unparseable render exits non-zero (the oneshot unit fails)"; else bad "an unparseable render exits non-zero (rc=$RC)"; fi
eq "$sentinel_before" "$(cat "$PROM")" "…and the previously-published file is left untouched"
matches 'refusing to publish' "$ERR" "…and says why, on stderr"
leftovers=$(find "$TEXTFILE_DIR" -type f ! -name 'timescale_jobs.prom' | wc -l | tr -d ' ')
eq 0 "$leftovers" "…and leaves no half-written temp file behind"

printf 'timescale-jobs-probe-test: %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
