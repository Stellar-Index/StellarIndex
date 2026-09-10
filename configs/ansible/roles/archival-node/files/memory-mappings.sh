#!/usr/bin/env bash
# memory-mappings — per-process virtual-memory-mapping headroom.
#
# WHY THIS EXISTS (r1, 2026-09-10 13:53 CEST). ClickHouse exhausted the
# kernel's per-process mapping limit and said so itself:
#
#   Allocator: Cannot malloc 63.33 MiB: , errno: 12, strerror: Cannot allocate memory
#   It looks like that the process is near the limit on number of virtual memory mappings.
#   Current number of mappings (/proc/self/maps): 1048578.
#   Limit on number of mappings (/proc/sys/vm/max_map_count): 1048576.
#
# It then went into a std::bad_alloc storm and crashed; systemd restarted
# it, and holders-rollup.service failed as collateral — its first failure
# against 24 successful cycles in the preceding 48h.
#
# The limit is already raised far above the 65530 default and is codified
# (/etc/sysctl.d/10-map-count.conf, vm.max_map_count=1048576 = 2^20). What
# was missing is any signal at all: no metric, no rule, nothing anywhere in
# the tree referenced max_map_count. A SERVER CRASH WAS THE FIRST AND ONLY
# SIGNAL. Steady state measured right after the restart was ~47,000-50,000
# mappings — about 4.5 % of the limit — so the excursion was roughly 20x,
# and its driver is NOT established. This probe does not claim one; it
# makes the number observable so the next excursion is watched on the way
# up instead of reconstructed from a core dump.
#
# node_exporter publishes no per-process mapping count (procfs collector
# covers the whole system, not a named process), so this is a
# textfile-collector producer in the shape of its siblings.
#
# WHICH PROCESS, AND WHY BY EXECUTABLE. `pgrep -f clickhouse` matches this
# probe's OWN command line — the self-match class that has bitten this
# project repeatedly — so the walk below resolves /proc/<pid>/exe and
# compares the resolved basename. A command line is attacker- and
# operator-influenced text; the exe link is what the kernel executed.
#
# WHICH OF THE TWO. r1 runs TWO processes off the clickhouse binary: a
# watchdog parent (~97 mappings) and the real server (~47,000). This
# reports the HIGHEST mapping count among the matches, because
# max_map_count is enforced per mm — the process that can hit the wall is
# by definition the one with the most mappings. That selector is correct
# without knowing which pid is the watchdog: it needs no ppid, no argv, no
# ordering assumption, and stays correct if the process topology changes
# (an extra child, a renamed unit). Picking by RSS was rejected as an
# indirect proxy that can invert — many small mappings are cheap in
# resident bytes and expensive in mapping count, which is exactly the
# failure mode being watched. Taking the FIRST match would have selected
# the watchdog roughly half the time and left the gauge permanently green
# at ~0.01 % of the limit. `..._procs` publishes how many matched, so a
# topology change is visible rather than silently narrowing what is
# watched.
#
# Self-test: scripts/ci/memory-mappings-test.sh (drives these shipped
# bytes against a fake /proc; CI has no host and macOS has no /proc).
#
# Runtime: one readlink per pid plus one maps read per match. At steady
# state that is ~47k lines (~3.5 MB); during the incident state it is
# ~1M lines. There is no cheaper interface — the kernel exposes no
# mapping COUNT, only the map itself — so the cost is accepted and the
# timer is spaced at 5 minutes. systemd will not start a second run while
# one is in flight.

set -euo pipefail

# Numeric formatting must not follow the host locale. Under a
# comma-decimal LC_NUMERIC, awk's %.6f renders the ratio as "0,044823",
# which is not a Prometheus value: the validator at the end would
# correctly refuse to publish, and this probe would go dark on a host
# where nothing is actually wrong. Pin it rather than discover it.
export LC_ALL=C

# Every input is overridable so the self-test can drive the SHIPPED bytes
# hermetically. Same idiom as ch-schema-drift.sh / galexie-archive-tip-lag.sh.
PROC_ROOT="${PROC_ROOT:-/proc}"
MAX_MAP_COUNT_FILE="${MAX_MAP_COUNT_FILE:-$PROC_ROOT/sys/vm/max_map_count}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
OUT="$TEXTFILE_DIR/memory_mappings.prom"

# Watched executables, comma-separated basenames. The `process` label
# carries the basename verbatim, so adding a second process here needs no
# metric rename and no rule change — the existing alerts apply to it the
# moment it appears. (Candidates if this recurs elsewhere: postgres,
# stellarindex-indexer.)
WATCH_EXES="${MEMORY_MAPPINGS_WATCH:-clickhouse}"

# The scratch file is created BESIDE the destination, never in /tmp. The
# unit runs with PrivateTmp=true, so /tmp is on a different filesystem
# and `mv` across it degrades to copy-then-unlink — node_exporter would
# be free to scrape a half-written file, which is the whole-file rejection
# this producer is careful to avoid. Same-directory mktemp keeps the swap
# a rename(2). node_exporter's collector globs *.prom, so the `.XXXXXX`
# suffix keeps the scratch file out of the scrape.
mkdir -p "$TEXTFILE_DIR"
TMP="$(mktemp "${OUT}.XXXXXX")"
trap 'rm -f "$TMP"' EXIT

# ── The limit ────────────────────────────────────────────────────────
# Read once. It is a kernel-wide sysctl, not a per-process property, so
# it carries no `process` label — the per-process quantity is the count,
# and the ratio joins the two.
LIMIT=""
if [ -r "$MAX_MAP_COUNT_FILE" ]; then
  LIMIT="$(tr -dc '0-9' < "$MAX_MAP_COUNT_FILE" || true)"
fi
case "$LIMIT" in
  '' | *[!0-9]* | 0) LIMIT="" ;;
esac

# mapping_count_of PID — print the number of VMAs, or nothing when the
# map is unreadable. Reading another process's map needs privilege
# (root, or same-uid with ptrace access); the unit runs as root for this
# reason, and a read that fails must NOT be reported as zero mappings —
# zero is the value that reads as "maximum headroom" and would make this
# probe permanently, falsely green.
mapping_count_of() {
  local n
  n="$(wc -l < "$PROC_ROOT/$1/maps" 2>/dev/null)" || return 1
  n="${n//[[:space:]]/}"
  case "$n" in
    '' | *[!0-9]*) return 1 ;;
  esac
  printf '%s\n' "$n"
}

# exe_basename PID — the basename of the executable the kernel ran, or
# nothing.
#
# `readlink -f` first (canonical path through any symlinked install), the
# bare form as fallback: some readlink implementations FAIL on a dangling
# link, and /proc/<pid>/exe is dangling precisely when the binary has
# been replaced under a running process — an in-place package upgrade,
# which is the state a long-lived server is most likely to be found in.
# The kernel then spells the link "<path> (deleted)", so that suffix is
# stripped before the basename is taken; leaving it on would silently
# stop matching the very process most worth watching.
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

# One pass per watched executable. /proc is a directory listing plus one
# readlink per pid, so re-walking it per name is cheaper than carrying an
# associative array (which bash 3.2 does not have).
IFS=',' read -r -a WATCHED <<< "$WATCH_EXES"
for want in "${WATCHED[@]}"; do
  [ -n "$want" ] || continue

  # The watch list is operator-supplied (Environment= in the unit) and
  # lands verbatim in a LABEL VALUE. A quote or backslash in it would
  # render an unparseable sample and cost every family in this file its
  # scrape — and the byte-level validator below deliberately mirrors
  # data-freshness.sh's grammar, which does not inspect label quoting.
  # So the value is constrained where it enters, not where it leaves.
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

  # An unreadable limit means no ratio can be computed for anything that
  # matched — the same fail-closed verdict as an unreadable map.
  if [ -n "$best" ] && [ -z "$LIMIT" ]; then
    unreadable=1
  fi

  printf 'stellarindex_process_memory_mappings_procs{process="%s"} %s\n' "$want" "$procs" >> "$TMP"
  printf 'stellarindex_process_memory_mappings_unreadable{process="%s"} %s\n' "$want" "$unreadable" >> "$TMP"

  if [ -n "$best" ]; then
    printf 'stellarindex_process_memory_mappings{process="%s"} %s\n' "$want" "$best" >> "$TMP"
  fi
  if [ -n "$best" ] && [ -n "$LIMIT" ]; then
    # awk, not bash arithmetic: the ratio is fractional and bash has only
    # integers. %.6f over a 2^20 limit resolves single mappings.
    ratio="$(awk -v c="$best" -v l="$LIMIT" 'BEGIN { printf "%.6f", c / l }')"
    printf 'stellarindex_process_memory_mappings_ratio{process="%s"} %s\n' "$want" "$ratio" >> "$TMP"
  fi
done

if [ -n "$LIMIT" ]; then
  printf 'stellarindex_process_memory_mappings_limit %s\n' "$LIMIT" >> "$TMP"
fi
printf 'stellarindex_process_memory_mappings_updated_unix %s\n' "$(date +%s)" >> "$TMP"

# ── Validate the rendered bytes before publishing ────────────────────
#
# node_exporter does not skip an unparseable line, it rejects the WHOLE
# file — one bad value here takes every family above down together, and
# on 2026-09-10 that same shape cost 127 unrelated timescale series ~20
# minutes of darkness. Every value this producer writes is already
# guarded numerically at its source (digits-only from wc -l and from the
# sysctl, %.6f from awk, date +%s), so this is a last line of defence
# rather than the primary one; it is here because "publish what parses"
# is a property the file should hold whatever a future edit does to it.
#
# Same grammar and the same position in the sequence as
# data-freshness.sh's validator. The VERDICT differs, deliberately.
# data-freshness.sh publishes and withholds only the offending lines,
# because it emits no last-run gauge: a file it declined to publish
# would be re-served verbatim forever with NOTHING going absent, so its
# only meta-alert could never fire. This producer does emit
# ..._updated_unix, and stellarindex_process_mappings_probe_degraded
# alerts on its age — so refusing to publish AGES into a ticket, which
# is the louder and safer outcome. Refusing also keeps the last KNOWN-
# GOOD headroom reading in place rather than replacing it with a partial
# file whose missing families read as absence.
#
# Run as an `if` condition so a validator that cannot run is handled
# rather than killing the script under `set -e`.
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

# node_exporter runs unprivileged and mktemp defaults to 0600, so the
# file has to be world-readable before the atomic swap or the collector
# skips it.
chmod 0644 "$TMP"
mv "$TMP" "$OUT"
trap - EXIT
