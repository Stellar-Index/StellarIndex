#!/usr/bin/env bash
# memory-mappings: per-process virtual-memory-mapping headroom.
#
# WHY (r1): ClickHouse hit the kernel's per-process mapping limit, logged
# "Cannot malloc ... errno: 12 ... near the limit on number of virtual memory mappings"
# (1048578 of vm.max_map_count 1048576), then crashed in a std::bad_alloc storm. The limit
# is already raised (/etc/sysctl.d/10-map-count.conf) but nothing observed it: the crash was
# the first signal. Steady state is ~47-50k mappings (~4.5 % of the limit); the driver of the
# excursion is NOT established, so this probe only makes the number watchable.
# node_exporter has no per-process mapping count, hence a textfile producer like its siblings.
#
# Process selection is by EXECUTABLE: `pgrep -f clickhouse` self-matches this probe's own
# command line, so the walk resolves /proc/<pid>/exe and compares the basename (the exe link
# is what the kernel ran; a command line is operator-influenced text).
#
# r1 runs two processes off the clickhouse binary: a watchdog parent (~97 mappings) and the
# server (~47,000). Report the HIGHEST count among matches: max_map_count is enforced per mm,
# so the process that can hit the wall is the one with the most mappings. This needs no ppid,
# argv or ordering and survives topology changes. RSS was rejected (many small mappings are
# cheap in bytes, costly in count); the FIRST match would pick the watchdog ~half the time and
# leave the gauge falsely green. `..._procs` publishes the match count so a topology change
# is visible.
#
# Self-test: scripts/ci/memory-mappings-test.sh (drives these bytes against a fake /proc).
#
# Runtime: one readlink per pid plus one maps read per match (~47k lines steady, ~1M in the
# incident). The kernel exposes only the map, not a count, so the cost is accepted and the
# timer is spaced at 5 minutes; systemd will not start a second run while one is in flight.
# one is in flight.

set -euo pipefail

# Numeric formatting must not follow the host locale: under a comma-decimal LC_NUMERIC awk's
# %.6f renders "0,044823", the validator refuses it, and the probe goes dark on a healthy host.
export LC_ALL=C

# Every input is overridable so the self-test can drive the SHIPPED bytes hermetically
# (same idiom as ch-schema-drift.sh / galexie-archive-tip-lag.sh).
PROC_ROOT="${PROC_ROOT:-/proc}"
MAX_MAP_COUNT_FILE="${MAX_MAP_COUNT_FILE:-$PROC_ROOT/sys/vm/max_map_count}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
OUT="$TEXTFILE_DIR/memory_mappings.prom"

# Watched executables, comma-separated basenames. The `process` label carries the basename
# verbatim, so adding one needs no metric rename or rule change.
WATCH_EXES="${MEMORY_MAPPINGS_WATCH:-clickhouse}"

# The scratch file is created BESIDE the destination, never in /tmp: the unit runs with
# PrivateTmp=true, so a cross-filesystem `mv` degrades to copy-then-unlink and node_exporter
# could scrape a half-written file. Same-directory mktemp keeps the swap a rename(2); the
# `.XXXXXX` suffix keeps the scratch file out of the *.prom glob.
mkdir -p "$TEXTFILE_DIR"
TMP="$(mktemp "${OUT}.XXXXXX")"
trap 'rm -f "$TMP"' EXIT

# ── The limit ────────────────────────────────────────────────────────
# Read once. A kernel-wide sysctl, so no `process` label; the ratio joins it to the count.
LIMIT=""
if [ -r "$MAX_MAP_COUNT_FILE" ]; then
  LIMIT="$(tr -dc '0-9' < "$MAX_MAP_COUNT_FILE" || true)"
fi
case "$LIMIT" in
  '' | *[!0-9]* | 0) LIMIT="" ;;
esac

# mapping_count_of PID: print the number of VMAs, or nothing when the map is unreadable.
# Reading another process's map needs privilege (the unit runs as root). A failed read must
# NOT be reported as zero: zero reads as "maximum headroom" and would make the probe
# permanently, falsely green.
mapping_count_of() {
  local n
  n="$(wc -l < "$PROC_ROOT/$1/maps" 2>/dev/null)" || return 1
  n="${n//[[:space:]]/}"
  case "$n" in
    '' | *[!0-9]*) return 1 ;;
  esac
  printf '%s\n' "$n"
}

# exe_basename PID: basename of the executable the kernel ran, or nothing.
#
# `readlink -f` first (canonical path), bare form as fallback: some readlinks FAIL on a
# dangling link, and /proc/<pid>/exe dangles when the binary was replaced under a running
# process (in-place package upgrade). The kernel then spells it "<path> (deleted)"; strip
# that suffix or the long-lived server we most want to watch would stop matching.
exe_basename() {
  local p
  p="$(readlink -f "$PROC_ROOT/$1/exe" 2>/dev/null)" || p=""
  if [ -z "$p" ]; then
    p="$(readlink "$PROC_ROOT/$1/exe" 2>/dev/null)" || p=""
  fi
  [ -n "$p" ] || return 1
  p="${p% (deleted)}"
  printf '%s\n' "${p##*/}"
}

{
  echo '# HELP stellarindex_process_memory_mappings Virtual-memory mappings (VMAs, one per /proc/<pid>/maps line) held by the watched process — the quantity the kernel caps at vm.max_map_count. Reported for the HIGHEST-mapping process matching the executable, because the cap is enforced per process.'
  echo '# TYPE stellarindex_process_memory_mappings gauge'
  echo '# HELP stellarindex_process_memory_mappings_limit Kernel vm.max_map_count: the per-process mapping ceiling. Kernel-wide, so it carries no process label. 1048576 on r1 (/etc/sysctl.d/10-map-count.conf), against a 65530 default.'
  echo '# TYPE stellarindex_process_memory_mappings_limit gauge'
  echo '# HELP stellarindex_process_memory_mappings_ratio Mappings held as a fraction of vm.max_map_count (0-1). ~0.045 is r1 ClickHouse steady state; 1.0 is the 2026-09-10 crash. Published as a ratio so the alerts survive a change to the limit.'
  echo '# TYPE stellarindex_process_memory_mappings_ratio gauge'
  echo '# HELP stellarindex_process_memory_mappings_procs Processes whose executable basename matched. 0 means the probe is watching NOTHING (process stopped, or renamed/relocated binary) — an absence the mapping gauges cannot express, since withholding them and publishing 0 mappings are indistinguishable to an alert.'
  echo '# TYPE stellarindex_process_memory_mappings_procs gauge'
  echo '# HELP stellarindex_process_memory_mappings_unreadable 1 when a process matched but its headroom could not be computed — /proc/<pid>/maps unreadable (privilege) or vm.max_map_count unreadable. Fail-closed: the affected gauges are withheld rather than published as 0.'
  echo '# TYPE stellarindex_process_memory_mappings_unreadable gauge'
  echo '# HELP stellarindex_process_memory_mappings_updated_unix Unix time of the last successful run. node_exporter re-serves a stale textfile verbatim on every scrape, so a stopped timer FREEZES these gauges at their last value instead of making them absent — this is the only series that can see that.'
  echo '# TYPE stellarindex_process_memory_mappings_updated_unix gauge'
} > "$TMP"

# One pass per watched executable: re-walking /proc per name is cheaper than an associative
# array (bash 3.2 has none).
IFS=',' read -r -a WATCHED <<< "$WATCH_EXES"
for want in "${WATCHED[@]}"; do
  [ -n "$want" ] || continue

  # The watch list is operator-supplied and lands verbatim in a LABEL VALUE. A quote or
  # backslash would render an unparseable sample and cost every family its scrape, and the
  # validator below does not inspect label quoting, so constrain the value where it enters.
  case "$want" in
    *[!A-Za-z0-9._-]*)
      echo "memory-mappings: ignoring watch entry '$want' — an executable name may only contain A-Za-z0-9._- (it becomes a Prometheus label value)" >&2
      continue
      ;;
  esac

  procs=0
  unreadable=0
  best=""

  for entry in "$PROC_ROOT"/[0-9]*; do
    [ -d "$entry" ] || continue
    pid="${entry##*/}"

    base="$(exe_basename "$pid")" || continue
    [ "$base" = "$want" ] || continue

    procs=$((procs + 1))

    count="$(mapping_count_of "$pid")" || {
      # Matched but unreadable: say so, do not guess.
      unreadable=1
      continue
    }
    if [ -z "$best" ] || [ "$count" -gt "$best" ]; then
      best="$count"
    fi
  done

  # An unreadable limit means no ratio can be computed: same fail-closed verdict as an
  # unreadable map.
  if [ -n "$best" ] && [ -z "$LIMIT" ]; then
    unreadable=1
  fi

  printf 'stellarindex_process_memory_mappings_procs{process="%s"} %s\n' "$want" "$procs" >> "$TMP"
  printf 'stellarindex_process_memory_mappings_unreadable{process="%s"} %s\n' "$want" "$unreadable" >> "$TMP"

  if [ -n "$best" ]; then
    printf 'stellarindex_process_memory_mappings{process="%s"} %s\n' "$want" "$best" >> "$TMP"
  fi
  if [ -n "$best" ] && [ -n "$LIMIT" ]; then
    # awk, not bash arithmetic: the ratio is fractional. %.6f over a 2^20 limit resolves single mappings.
    ratio="$(awk -v c="$best" -v l="$LIMIT" 'BEGIN { printf "%.6f", c / l }')"
    printf 'stellarindex_process_memory_mappings_ratio{process="%s"} %s\n' "$want" "$ratio" >> "$TMP"
  fi
done

if [ -n "$LIMIT" ]; then
  printf 'stellarindex_process_memory_mappings_limit %s\n' "$LIMIT" >> "$TMP"
fi
printf 'stellarindex_process_memory_mappings_updated_unix %s\n' "$(date +%s)" >> "$TMP"

# ── Validate the rendered bytes before publishing ──
#
# node_exporter rejects the WHOLE file on one unparseable line. Every value here is already
# guarded at its source (digits-only wc -l and sysctl, %.6f, date +%s), so this is a last
# line of defence: "publish what parses" should hold whatever a future edit does.
#
# Same grammar and position as data-freshness.sh's validator, but the VERDICT differs on
# purpose. That script has no last-run gauge, so it publishes and withholds bad lines (an
# unpublished file would be re-served forever with nothing going absent). This producer emits
# ..._updated_unix and stellarindex_process_mappings_probe_degraded alerts on its age, so
# refusing to publish AGES into a ticket and keeps the last KNOWN-GOOD reading rather than a
# partial file whose missing families read as absence.
#
# Run as an `if` condition so a validator that cannot run is handled, not fatal under `set -e`.
if ! BAD="$(awk '
  /^[ \t]*$/ || /^#/ { next }
  {
    # Braces as [{] / [}]: in an ERE a bare brace opens an interval
    # expression and the backslash form is undefined by POSIX, so this
    # is what means the same thing under mawk, gawk and BWK awk alike.
    rest = $0
    sub(/^[a-zA-Z_:][a-zA-Z0-9_:]*([{][^}]*[}])?[ \t]+/, "", rest)
    if (rest != $0 && rest ~ /^[+-]?([0-9]+\.?[0-9]*([eE][+-]?[0-9]+)?|\.[0-9]+([eE][+-]?[0-9]+)?|Inf|NaN)([ \t]+[0-9]+)?[ \t]*$/) next
    printf "memory-mappings: unparseable exposition line: %s\n", $0 > "/dev/stderr"
    bad = bad + 1
  }
  END { print bad + 0 }
' "$TMP")"; then
  echo "memory-mappings: the exposition validator did not run — refusing to publish unvalidated bytes. Previous memory_mappings.prom left in place; its ..._updated_unix will age into stellarindex_process_mappings_probe_degraded." >&2
  exit 1
fi
case "$BAD" in
  '' | *[!0-9]*)
    echo "memory-mappings: exposition validator returned a non-numeric tally ('$BAD') — refusing to publish. Previous memory_mappings.prom left in place." >&2
    exit 1
    ;;
esac
if [ "$BAD" -gt 0 ]; then
  echo "memory-mappings: $BAD unparseable exposition line(s) rendered — refusing to publish, since node_exporter would reject the whole file. Previous memory_mappings.prom left in place." >&2
  exit 1
fi

# node_exporter runs unprivileged and mktemp defaults to 0600: make the file world-readable
# before the atomic swap or the collector skips it.
chmod 0644 "$TMP"
mv "$TMP" "$OUT"
trap - EXIT
