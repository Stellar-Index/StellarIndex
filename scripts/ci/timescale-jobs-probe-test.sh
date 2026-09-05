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
#      it is world-readable, and no temp file survives.
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
case "$kind" in
  cagg)        printf 'prices_1m|1788598634|60\noracle_prices_1m|1788598600|30\n' ;;
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

# run <fail-queries> <empty-queries> — one probe run; sets $RC.
run() {
  rm -f "$PROM"
  FAIL_QUERIES="$1" EMPTY_QUERIES="$2" bash "$PROBE"
  RC=$?
}

# metric <exact-name-with-labels> — the value, or the empty string.
metric() {
  awk -v want="$1" '$1 == want { print $2 }' "$PROM"
}

has_family() { grep -qE "^$1\{" "$PROM"; }

# ─── 1. clean run ───────────────────────────────────────────────────
run "" ""
[[ "$RC" -eq 0 ]] && ok "clean run exits 0" || bad "clean run exits 0 (got $RC)"
for f in stellarindex_cagg_last_refresh_unix \
         stellarindex_timescale_chunks_overdue_compression \
         stellarindex_timescale_job_failures_total; do
  has_family "$f" && ok "clean run emits $f" || bad "clean run emits $f"
done
for qname in cagg_refresh compression job_stats; do
  got=$(metric "stellarindex_timescale_probe_query_ok{query=\"$qname\"}")
  [[ "$got" == "1" ]] && ok "clean run: query_ok $qname = 1" \
                      || bad "clean run: query_ok $qname = 1 (got '${got:-<absent>}')"
  got=$(metric "stellarindex_timescale_probe_rows{query=\"$qname\"}")
  [[ "$got" == "2" ]] && ok "clean run: rows $qname = 2" \
                      || bad "clean run: rows $qname = 2 (got '${got:-<absent>}')"
done
stamp=$(metric stellarindex_timescale_probe_last_run_unix)
[[ "$stamp" =~ ^1[0-9]{9}$ ]] && ok "clean run stamps last_run_unix" \
                              || bad "clean run stamps last_run_unix (got '${stamp:-<absent>}')"

# 0644, atomic: node_exporter runs unprivileged and SILENTLY skips a
# file it cannot read, and a surviving temp file would be scraped as a
# duplicate of every series in it.
mode=$(ls -l "$PROM" | cut -c1-10)
[[ "$mode" == "-rw-r--r--" ]] && ok "textfile is 0644" || bad "textfile is 0644 (got $mode)"
leftovers=$(find "$TEXTFILE_DIR" -type f ! -name 'timescale_jobs.prom' | wc -l | tr -d ' ')
[[ "$leftovers" == "0" ]] && ok "no temp file survives" || bad "no temp file survives ($leftovers left)"

# ─── 2. ONE failing query ───────────────────────────────────────────
#
# The degrade is deliberate: dying here would fire the EXIT trap, skip
# the `mv`, and leave the PREVIOUS file on disk to be re-scraped with a
# fresh sample timestamp forever. So the other two families must still
# land — and the failure must still be visible.
run "compression" ""
[[ "$RC" -eq 0 ]] && ok "one failing query still writes the file" \
                  || bad "one failing query still writes the file (rc $RC)"
has_family stellarindex_cagg_last_refresh_unix \
  && ok "failing compression query does not cost the cagg family" \
  || bad "failing compression query does not cost the cagg family"
has_family stellarindex_timescale_job_failures_total \
  && ok "failing compression query does not cost the job-failure family" \
  || bad "failing compression query does not cost the job-failure family"
got=$(metric 'stellarindex_timescale_probe_query_ok{query="compression"}')
[[ "$got" == "0" ]] && ok "failing query reports query_ok 0" \
                    || bad "failing query reports query_ok 0 (got '${got:-<absent>}')"
got=$(metric 'stellarindex_timescale_probe_query_ok{query="cagg_refresh"}')
[[ "$got" == "1" ]] && ok "the queries that worked still report 1" \
                    || bad "the queries that worked still report 1 (got '${got:-<absent>}')"
got=$(metric 'stellarindex_timescale_probe_rows{query="compression"}')
[[ "$got" == "0" ]] && ok "failing query reports rows 0" \
                    || bad "failing query reports rows 0 (got '${got:-<absent>}')"

# ─── 3. a query that succeeds and returns nothing ───────────────────
run "" "compression"
got=$(metric 'stellarindex_timescale_probe_query_ok{query="compression"}')
[[ "$got" == "1" ]] && ok "empty result keeps query_ok 1" \
                    || bad "empty result keeps query_ok 1 (got '${got:-<absent>}')"
got=$(metric 'stellarindex_timescale_probe_rows{query="compression"}')
[[ "$got" == "0" ]] && ok "empty result reports rows 0" \
                    || bad "empty result reports rows 0 (got '${got:-<absent>}')"
has_family stellarindex_timescale_chunks_overdue_compression \
  && bad "an empty result emits no compression series" \
  || ok "an empty result emits no compression series"

# ─── 4. every query failing still stamps the run ────────────────────
run "cagg compression jobs" ""
stamp=$(metric stellarindex_timescale_probe_last_run_unix)
[[ "$stamp" =~ ^1[0-9]{9}$ ]] && ok "a fully-failing run still stamps last_run_unix" \
                              || bad "a fully-failing run still stamps last_run_unix (got '${stamp:-<absent>}')"
zeros=$(grep -c '^stellarindex_timescale_probe_query_ok{.*} 0$' "$PROM")
[[ "$zeros" == "3" ]] && ok "a fully-failing run reports all three queries as 0" \
                      || bad "a fully-failing run reports all three queries as 0 (got $zeros)"

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
  echo "  note promtool not installed — skipping the text-format assertions" >&2
fi

printf 'timescale-jobs-probe-test: %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
