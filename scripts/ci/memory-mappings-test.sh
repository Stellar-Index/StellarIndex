#!/usr/bin/env bash
# memory-mappings-test.sh — fixture tests for the per-process
# mapping-headroom probe
# (configs/ansible/roles/archival-node/files/memory-mappings.sh).
#
# Drives the SHIPPED bytes against a FAKE /proc: CI has no host, macOS
# has no /proc at all, and the one thing this probe must get right —
# which of two processes off the same binary it reports — cannot be
# exercised any other way.
#
# WHAT IT PINS, and why each case exists:
#
#   1. It reports the HIGHEST mapping count among the matches. r1 runs
#      two clickhouse processes: a watchdog parent (~97 mappings) and the
#      real server (~47,000). max_map_count is enforced per process, so
#      the one that can hit the wall is the one with the most mappings.
#      The fixture's watchdog sorts FIRST in the /proc glob, so a
#      first-match selector picks it and the gauge sits permanently at
#      ~0.01 % of the limit — green through the next crash.
#   2. It matches on the EXECUTABLE, not the command line, and keeps
#      matching after the binary has been replaced under the running
#      process (the kernel then spells /proc/<pid>/exe "<path>
#      (deleted)"). An in-place package upgrade is the state a
#      long-lived server is most likely to be found in.
#   3. A process it cannot see is published as procs=0 with the mapping
#      gauges WITHHELD — never as 0 mappings, which is the value that
#      reads as maximum headroom.
#   4. A matched-but-unreadable map, and an unreadable vm.max_map_count,
#      are fail-closed (unreadable=1, ratio withheld).
#   5. An operator-supplied watch name cannot inject into a label value.
#   6. The rendered bytes parse as Prometheus exposition, checked with
#      the repo's own parser (lint_textfile_exposition.py --check-file)
#      rather than a second copy of the grammar written here.
#
# PROVEN-RED (re-run after any edit to the selector):
#   * change `[ "$count" -gt "$best" ]` to `[ "$count" -lt "$best" ]` in
#     memory-mappings.sh and case 1 reports 97 instead of 47000 — the
#     watchdog, i.e. the permanently-green failure this probe exists to
#     avoid;
#   * delete the `case "$want" in *[!A-Za-z0-9._-]*)` guard and case 5
#     renders `{process="click"house"}` into the published file.
#
# Run: bash scripts/ci/memory-mappings-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/configs/ansible/roles/archival-node/files/memory-mappings.sh"
CHECKER="$PWD/scripts/ci/lint_textfile_exposition.py"
[ -r "$SCRIPT" ] || { echo "memory-mappings-test: missing $SCRIPT" >&2; exit 2; }
[ -r "$CHECKER" ] || { echo "memory-mappings-test: missing $CHECKER" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }
# exited0 <case> <rc> — spelled out rather than `A && ok || bad`, which
# also runs `bad` whenever `ok` itself returns non-zero.
exited0() {
  if [ "$2" = "0" ]; then
    ok "$1: exits 0"
  else
    bad "$1: exited $2 (stderr: $(tail -2 "$TMP/$1.stderr"))"
  fi
}

# ─── the fake /proc ──────────────────────────────────────────────────
#
# 101 = watchdog parent, 97 mappings. Sorts FIRST in the shell glob, so
#       any "first match wins" selector picks it.
# 202 = the real server, 47000 mappings (r1's measured steady state).
# 303 = postgres — a process that must NOT be matched.
#
# The exe links point at real files so `readlink -f` behaves the same on
# every implementation; the deleted-binary form is exercised separately
# in case 2.
FAKE="$TMP/proc"
mkdir -p "$FAKE/sys/vm" "$TMP/bin"
: > "$TMP/bin/clickhouse"
: > "$TMP/bin/postgres"
echo 1048576 > "$FAKE/sys/vm/max_map_count"

make_proc() {  # make_proc <pid> <exe-target> <maps-line-count>
  mkdir -p "$FAKE/$1"
  ln -sf "$2" "$FAKE/$1/exe"
  if [ "$3" != "none" ]; then
    seq 1 "$3" > "$FAKE/$1/maps"
  fi
}
make_proc 101 "$TMP/bin/clickhouse" 97
make_proc 202 "$TMP/bin/clickhouse" 47000
make_proc 303 "$TMP/bin/postgres" 500

# run_probe <case> [env assignments…] — one isolated invocation.
run_probe() {
  local name="$1"; shift
  rm -rf "${TMP:?}/$name"
  mkdir -p "$TMP/$name"
  env PROC_ROOT="$FAKE" \
      MAX_MAP_COUNT_FILE="$FAKE/sys/vm/max_map_count" \
      TEXTFILE_DIR="$TMP/$name" \
      "$@" \
      bash "$SCRIPT" > "$TMP/$name.stdout" 2> "$TMP/$name.stderr"
  echo "$?"
}
prom() { cat "$TMP/$1/memory_mappings.prom" 2>/dev/null; }

# has <case> <exact line> — the published file contains this line verbatim.
has() {
  if grep -qxF "$2" "$TMP/$1/memory_mappings.prom" 2>/dev/null; then
    ok "$1: $2"
  else
    bad "$1: expected line '$2'; got:
$(prom "$1" | grep -v '^#' | sed 's/^/      /')"
  fi
}
hasnt() {  # hasnt <case> <grep pattern> <why it must be absent>
  if grep -q "$2" "$TMP/$1/memory_mappings.prom" 2>/dev/null; then
    bad "$1: '$2' is present but $3"
  else
    ok "$1: no '$2' — $3"
  fi
}

# ─── 1. the healthy shape: the SERVER, not the watchdog ──────────────
rc="$(run_probe healthy)"
exited0 healthy "$rc"

has healthy 'stellarindex_process_memory_mappings{process="clickhouse"} 47000'
has healthy 'stellarindex_process_memory_mappings_procs{process="clickhouse"} 2'
has healthy 'stellarindex_process_memory_mappings_unreadable{process="clickhouse"} 0'
has healthy 'stellarindex_process_memory_mappings_limit 1048576'
# 47000 / 1048576 = 0.04482269… — r1's measured steady state, ~4.5 % of
# the limit, against the 1.0 the 2026-09-10 crash reached.
has healthy 'stellarindex_process_memory_mappings_ratio{process="clickhouse"} 0.044823'

if grep -q '^stellarindex_process_memory_mappings_updated_unix [0-9]\{10\}' \
     "$TMP/healthy/memory_mappings.prom" 2>/dev/null; then
  ok "healthy: stamps updated_unix (the only series that can see a stopped timer)"
else
  bad "healthy: no updated_unix stamp — a frozen textfile would read as live"
fi

# Every family carries HELP + TYPE. node_exporter tolerates a missing
# header, but an undeclared family means the operator reading a scrape
# has nowhere to learn what the number is.
for fam in stellarindex_process_memory_mappings \
           stellarindex_process_memory_mappings_limit \
           stellarindex_process_memory_mappings_ratio \
           stellarindex_process_memory_mappings_procs \
           stellarindex_process_memory_mappings_unreadable \
           stellarindex_process_memory_mappings_updated_unix; do
  if grep -q "^# HELP $fam " "$TMP/healthy/memory_mappings.prom" \
     && grep -q "^# TYPE $fam gauge$" "$TMP/healthy/memory_mappings.prom"; then
    ok "healthy: $fam declares HELP + TYPE"
  else
    bad "healthy: $fam has no HELP/TYPE of its own"
  fi
done

# 6. The rendered bytes, judged by the repo's parser — not by a second
#    copy of the exposition grammar written inside this test, which
#    would only prove the copy agrees with itself.
if python3 "$CHECKER" --check-file "$TMP/healthy/memory_mappings.prom" >/dev/null 2>"$TMP/checkfile.err"; then
  ok "healthy: published bytes parse as Prometheus exposition"
else
  bad "healthy: published bytes do not parse: $(cat "$TMP/checkfile.err")"
fi

# ─── 2. the binary was replaced under the running process ────────────
#
# The kernel spells the exe link "<path> (deleted)" after an in-place
# package upgrade. Left unhandled, the basename becomes "clickhouse
# (deleted)", nothing matches, and the probe silently watches nothing on
# exactly the host that has been running longest.
ln -sf "$TMP/bin/clickhouse (deleted)" "$FAKE/202/exe"
rc="$(run_probe deleted)"
exited0 deleted "$rc"
has deleted 'stellarindex_process_memory_mappings{process="clickhouse"} 47000'
has deleted 'stellarindex_process_memory_mappings_procs{process="clickhouse"} 2'
ln -sf "$TMP/bin/clickhouse" "$FAKE/202/exe"

# ─── 3. nothing matched: procs=0, gauges WITHHELD ────────────────────
#
# Publishing 0 mappings here is the dangerous direction: 0 is the value
# that means "maximum headroom", so a renamed or relocated binary would
# leave the alerts green forever. Absence is honest; 0 is a lie.
rc="$(run_probe absent MEMORY_MAPPINGS_WATCH=nosuchbinary)"
exited0 absent "$rc"
has absent 'stellarindex_process_memory_mappings_procs{process="nosuchbinary"} 0'
hasnt absent '^stellarindex_process_memory_mappings{' \
  "0 mappings would read as maximum headroom and never alert"
hasnt absent '^stellarindex_process_memory_mappings_ratio{' \
  "a ratio of 0 for a process nobody can see is a false all-clear"

# ─── 4a. matched, but the map could not be read ──────────────────────
#
# Privilege (the probe de-privileged, or ptrace_scope tightened), or the
# pid exiting between the readlink and the read. Fail closed.
make_proc 404 "$TMP/bin/redis-server" none
rc="$(run_probe unreadable MEMORY_MAPPINGS_WATCH=redis-server)"
exited0 unreadable "$rc"
has unreadable 'stellarindex_process_memory_mappings_procs{process="redis-server"} 1'
has unreadable 'stellarindex_process_memory_mappings_unreadable{process="redis-server"} 1'
hasnt unreadable '^stellarindex_process_memory_mappings{process="redis-server"}' \
  "a map that could not be read must not be published as a count"

# ─── 4b. vm.max_map_count itself unreadable ──────────────────────────
#
# The count is still true and still published; the RATIO is not
# computable, so it is withheld and the process is flagged unreadable
# rather than being given an invented denominator.
rc="$(run_probe nolimit MAX_MAP_COUNT_FILE="$TMP/does-not-exist")"
exited0 nolimit "$rc"
has nolimit 'stellarindex_process_memory_mappings{process="clickhouse"} 47000'
has nolimit 'stellarindex_process_memory_mappings_unreadable{process="clickhouse"} 1'
hasnt nolimit '^stellarindex_process_memory_mappings_ratio' \
  "a ratio needs a denominator; inventing one is worse than having none"
hasnt nolimit '^stellarindex_process_memory_mappings_limit ' \
  "an unread limit must not be published as a number"
if python3 "$CHECKER" --check-file "$TMP/nolimit/memory_mappings.prom" >/dev/null 2>&1; then
  ok "no-limit: the degraded file still parses"
else
  bad "no-limit: the degraded file does not parse"
fi

# ─── 5. the watch list cannot inject into a label value ──────────────
#
# MEMORY_MAPPINGS_WATCH arrives from the unit's Environment=. A quote in
# it renders `{process="click"house"}`, which node_exporter rejects — and
# it rejects the WHOLE file, taking every family above down with it.
rc="$(run_probe inject MEMORY_MAPPINGS_WATCH='click"house')"
exited0 inject "$rc"
hasnt inject 'click"house' "an unquotable watch name must never reach a label value"
if grep -q 'ignoring watch entry' "$TMP/inject.stderr"; then
  ok "label-injection: the rejected entry is named on stderr, not dropped silently"
else
  bad "label-injection: nothing on stderr — a silently ignored watch list is a blind probe"
fi
if python3 "$CHECKER" --check-file "$TMP/inject/memory_mappings.prom" >/dev/null 2>&1; then
  ok "label-injection: the published file still parses"
else
  bad "label-injection: the published file does not parse"
fi

# ─── 6. a second watched process needs no rename ─────────────────────
#
# The `process` label is what makes the metric extensible: adding a name
# to the watch list produces a new series the existing alerts already
# cover. Pinned so a future refactor cannot quietly make the family
# single-process again.
rc="$(run_probe two MEMORY_MAPPINGS_WATCH=clickhouse,postgres)"
exited0 two "$rc"
has two 'stellarindex_process_memory_mappings{process="clickhouse"} 47000'
has two 'stellarindex_process_memory_mappings{process="postgres"} 500'
has two 'stellarindex_process_memory_mappings_procs{process="postgres"} 1'

echo "memory-mappings-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
