#!/usr/bin/env bash
# galexie-catchup-probe-test.sh — fixture tests for the galexie
# catchup-refusal probe the archival-node role installs as
# /usr/local/sbin/galexie-catchup-probe.sh.
#
# THE DEFECT THESE PIN. The probe counted journal lines with
#   N=$(journalctl -u galexie … 2>/dev/null | grep -c "Skipping catchup" || true)
# `grep -c` prints 0 and exits 1 when nothing matches — the HEALTHY
# outcome — so the `|| true` was load-bearing, and it swallowed the
# journal read's failure with it. Measured on r1 2026-09-05, a healthy
# read, a unit name that does not exist, a journal that could not be
# opened and a missing journalctl binary all produced N=0 and exit 0.
# The consumer, stellarindex_galexie_catchup_refused, is severity
# `page` with no absent arm and no staleness arm, so a broken read left
# it frozen at zero, structurally unable to fire, with nothing anywhere
# reporting the blindness.
#
# What must hold:
#   1. a healthy read emits the refusal count AND reports read_ok 1
#      with the journal_lines it actually read, plus a run stamp;
#   2. a healthy read with no refusals emits 0 — that is a real
#      measurement and must stay distinguishable from a failure;
#   3. a journal read that FAILED emits no refusal count at all (a
#      fabricated 0 is the assertion "no refusals") and reports
#      read_ok 0, while still writing the file and the stamp — the
#      degrade is kept, the silence is not;
#   4. a read that exits 0 and returns NOTHING (renamed unit, journal
#      filtered to nothing) is caught by journal_lines 0, because it
#      raises no exit status and produces the healthy value;
#   5. the run is stamped even when the read failed, so "producing bad
#      data" and "not running" stay different signals;
#   6. the output is valid Prometheus text (a malformed textfile makes
#      node_exporter drop the WHOLE file), it is world-readable, and no
#      temp file survives.
#
# The SHIPPED bytes are executed, not a hand-copied twin: the script is
# an inline `content:` block in the role, so it is extracted from the
# task YAML the way ansible would render it (same idiom as
# scripts/ci/timescale-jobs-probe-test.sh). `journalctl` is stubbed on
# PATH and TEXTFILE_DIR is redirected into a temp dir, so there is no
# systemd, no journal and no write outside $TMPDIR.
#
# Run: bash scripts/ci/galexie-catchup-probe-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SRC="$PWD/configs/ansible/roles/archival-node/tasks/10-observability.yml"
[[ -r "$SRC" ]] || { echo "galexie-catchup-probe-test: missing $SRC" >&2; exit 2; }

command -v python3 >/dev/null 2>&1 || {
  echo "galexie-catchup-probe-test: python3 is required to render the task's inline script" >&2
  exit 2
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

PROBE="$TMP/galexie-catchup-probe.sh"
TASK_NAME="galexie catchup-refusal probe (script + timer)"
if ! TASK_NAME="$TASK_NAME" SRC="$SRC" DEST="$PROBE" python3 - <<'PY'
import io, os, sys
try:
    import yaml
except ImportError:
    print("galexie-catchup-probe-test: FAIL — PyYAML is required (pip install pyyaml); "
          "refusing to pass vacuously", file=sys.stderr)
    sys.exit(2)
doc = yaml.safe_load(io.open(os.environ["SRC"], encoding="utf-8"))
want = os.environ["TASK_NAME"]
for task in doc or []:
    if isinstance(task, dict) and task.get("name") == want:
        io.open(os.environ["DEST"], "w", encoding="utf-8").write(
            task["ansible.builtin.copy"]["content"])
        sys.exit(0)
print(f"galexie-catchup-probe-test: FAIL — no task named {want!r} in {os.environ['SRC']}",
      file=sys.stderr)
sys.exit(2)
PY
then
  exit 2
fi
chmod +x "$PROBE"

# ─── stubs ──────────────────────────────────────────────────────────
#
# journalctl stub, driven by $SCENARIO. The line counts mirror what r1
# actually produces: twelve consecutive 5-minute buckets on 2026-09-05
# returned 364-409 lines, so `healthy` emits 364.
mkdir -p "$TMP/bin" "$TMP/textfile"
cat > "$TMP/bin/journalctl" <<'SH'
#!/usr/bin/env bash
# `journalctl -u galexie --since -5min --no-pager`
case "${SCENARIO:-healthy}" in
  healthy)
    for i in $(seq 1 364); do echo "Sep 05 09:30:00 r1 galexie[1]: exported ledger $i"; done
    exit 0 ;;
  refusing)
    for i in $(seq 1 360); do echo "Sep 05 09:30:00 r1 galexie[1]: exported ledger $i"; done
    for i in $(seq 1 4); do echo "Sep 05 09:30:00 r1 galexie[1]: History: Skipping catchup"; done
    exit 0 ;;
  read_failed)
    # A journal that cannot be opened. The real command writes its
    # diagnostic to stderr, which the probe redirects away, and exits 1.
    echo "Failed to open files: No such file or directory" >&2
    exit 1 ;;
  truncated_read)
    # The read dies partway: rows already written stay on stdout and the
    # exit status is non-zero. Those rows contain no refusal, so the
    # pre-fix probe reported a clean zero from a failed read.
    for i in $(seq 1 42); do echo "Sep 05 09:30:00 r1 galexie[1]: exported ledger $i"; done
    echo "Journal file corrupted, ignoring file" >&2
    exit 1 ;;
  renamed_unit)
    # `journalctl -u <unit that does not exist>` exits 0 and prints
    # nothing. Confirmed on r1 2026-09-05.
    exit 0 ;;
  no_binary)
    # Models journalctl missing from PATH: bash's own 127.
    exit 127 ;;
esac
echo "journalctl stub: unknown SCENARIO ${SCENARIO:-}" >&2
exit 9
SH
chmod +x "$TMP/bin/journalctl"
export PATH="$TMP/bin:$PATH"
export TEXTFILE_DIR="$TMP/textfile"
PROM="$TEXTFILE_DIR/galexie_catchup.prom"

# run <scenario> — one probe run; sets $RC.
run() {
  rm -f "$PROM"
  SCENARIO="$1" bash "$PROBE"
  RC=$?
}

# metric <exact-name> — the value, or the empty string when absent.
metric() {
  awk -v want="$1" '$1 == want { print $2 }' "$PROM"
}

# expect <metric> <wanted> <label>
expect() {
  local got
  got=$(metric "$1")
  if [[ "$got" == "$2" ]]; then
    ok "$3"
  else
    bad "$3 (want '$2', got '${got:-<absent>}')"
  fi
}

stamped() { # stamped <label>
  local s
  s=$(metric stellarindex_galexie_catchup_probe_last_run_unix)
  if [[ "$s" =~ ^1[0-9]{9}$ ]]; then
    ok "$1"
  else
    bad "$1 (got '${s:-<absent>}')"
  fi
}

# ─── 1. a healthy read that found refusals ──────────────────────────
run refusing
[[ "$RC" -eq 0 ]] && ok "a refusing run exits 0" || bad "a refusing run exits 0 (got $RC)"
expect stellarindex_galexie_catchup_refusals_5m 4 "a refusing run counts every refusal"
expect stellarindex_galexie_catchup_probe_read_ok 1 "a refusing run reports read_ok 1"
expect stellarindex_galexie_catchup_probe_journal_lines 364 "a refusing run reports the lines it read"
stamped "a refusing run stamps last_run_unix"

# 0644, atomic: node_exporter runs unprivileged and SILENTLY skips a
# file it cannot read, and a surviving temp file would be scraped as a
# duplicate of every series in it.
mode=$(ls -l "$PROM" | cut -c1-10)
[[ "$mode" == "-rw-r--r--" ]] && ok "textfile is 0644" || bad "textfile is 0644 (got $mode)"
leftovers=$(find "$TEXTFILE_DIR" -type f ! -name 'galexie_catchup.prom' | wc -l | tr -d ' ')
[[ "$leftovers" == "0" ]] && ok "no temp file survives" || bad "no temp file survives ($leftovers left)"

# ─── 2. a healthy read that found nothing ───────────────────────────
#
# A real measurement of zero. It must still be published, and must stay
# distinguishable from the failures below.
run healthy
expect stellarindex_galexie_catchup_refusals_5m 0 "a clean quiet run publishes the zero it measured"
expect stellarindex_galexie_catchup_probe_read_ok 1 "a clean quiet run reports read_ok 1"
expect stellarindex_galexie_catchup_probe_journal_lines 364 "a clean quiet run reports the lines it read"

# ─── 3. the read failed outright ────────────────────────────────────
#
# The degrade is deliberate and is KEPT: the file is still written, so
# node_exporter is not left re-serving the previous one forever. What
# must not survive is the fabricated count.
run read_failed
[[ "$RC" -eq 0 ]] && ok "a failed read still writes the file" \
                  || bad "a failed read still writes the file (rc $RC)"
got=$(metric stellarindex_galexie_catchup_refusals_5m)
[[ -z "$got" ]] && ok "a failed read publishes NO refusal count" \
                || bad "a failed read publishes NO refusal count (got '$got')"
expect stellarindex_galexie_catchup_probe_read_ok 0 "a failed read reports read_ok 0"
stamped "a failed read still stamps last_run_unix"

# The shape that made this indistinguishable from health: the read dies
# partway, the rows it did return carry no refusal, and the count is a
# clean zero taken from a failed read.
run truncated_read
got=$(metric stellarindex_galexie_catchup_refusals_5m)
[[ -z "$got" ]] && ok "a truncated read publishes NO refusal count" \
                || bad "a truncated read publishes NO refusal count (got '$got')"
expect stellarindex_galexie_catchup_probe_read_ok 0 "a truncated read reports read_ok 0"

# ─── 4. the read succeeded and returned nothing ─────────────────────
#
# The door an exit-status check alone leaves open. `journalctl -u <unit
# that does not exist>` exits 0 with no output (confirmed on r1), so the
# count is a genuine, correctly-derived zero over an empty input — and
# the refusal page is blind. Only the line count sees it.
run renamed_unit
expect stellarindex_galexie_catchup_probe_read_ok 1 "an empty read keeps read_ok 1"
expect stellarindex_galexie_catchup_probe_journal_lines 0 "an empty read reports journal_lines 0"
expect stellarindex_galexie_catchup_refusals_5m 0 "an empty read still yields a zero count"

# ─── 5. journalctl missing from PATH ────────────────────────────────
run no_binary
expect stellarindex_galexie_catchup_probe_read_ok 0 "a missing journalctl reports read_ok 0"
got=$(metric stellarindex_galexie_catchup_refusals_5m)
[[ -z "$got" ]] && ok "a missing journalctl publishes NO refusal count" \
                || bad "a missing journalctl publishes NO refusal count (got '$got')"
stamped "a missing journalctl still stamps last_run_unix"

# ─── 6. the file parses as Prometheus text ──────────────────────────
#
# One malformed line makes node_exporter reject the WHOLE file, taking
# every family with it — the blindness these health metrics exist to
# report, arriving by a different door. The failed-read run is included
# because it emits a HELP/TYPE header with no sample under it.
if command -v promtool >/dev/null 2>&1; then
  for sc in refusing read_failed renamed_unit; do
    run "$sc"
    if promtool check metrics < "$PROM" >/dev/null 2>&1; then
      ok "output parses as Prometheus text (scenario=$sc)"
    else
      bad "output parses as Prometheus text (scenario=$sc)"
    fi
  done
else
  echo "  note promtool not installed — skipping the text-format assertions" >&2
fi

printf 'galexie-catchup-probe-test: %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
