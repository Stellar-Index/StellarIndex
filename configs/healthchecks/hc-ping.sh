#!/usr/bin/env bash
#
# hc-ping.sh - deliver one Healthchecks.io ping and make the delivery itself
# observable. Sourced by heartbeat.sh, smoke.sh and sla-probe.sh; not executable.
#
# Pings here end in `|| true`: a monitoring wrapper must never fail the unit it
# monitors. But that discards the one fact an operator needs when Healthchecks.io
# reports a healthy host down: the ping never left the box. A dropped ping and a
# dead service produce the SAME "down" email, and without evidence the alert
# reads as noise.
#
# hc_ping keeps the exit-code contract and records what happened where it
# outlives the run:
#   - the journal, at WARN, naming the check and curl's exit code;
#   - node_exporter's textfile collector, as a per-check failure counter and a
#     last-success timestamp, which `stellarindex_healthcheck_ping_undelivered`
#     evaluates. When the counter moves, the next email is about our egress,
#     not the service.
#
# hc_ping_verdict debounces /fail: an explicit /fail bypasses the check's grace
# period, so one failed probe during a routine restart cost a down + up email
# pair. It sends /fail only after <fail-after> consecutive failures, tracked in
# the same textfile.

# TEXTFILE_DIR is shared with smoke.sh's emit_metric. Callers may set it to
# /dev/null to opt out (the test harness does); an unwritable dir still pings
# and logs.
HC_PING_TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"

# hc_ping_counter_read <file> <metric>
#
# Reads a counter back from a textfile written on an earlier run; the textfile
# collector is the only state these oneshot units have. An unreadable or absent
# file reads 0: a restarted counter shows as a reset to `increase()`, where an
# invented number would not.
hc_ping_counter_read() {
  local file="$1" metric="$2"
  [ -r "$file" ] || { echo 0; return 0; }
  awk -v m="$metric" '$0 ~ "^" m "[{ ]" { v=$NF } END { print (v == "" ? 0 : v) }' \
    "$file" 2>/dev/null || echo 0
}

# hc_ping_emit <check> <failures-total> <last-success-unix> <consecutive-failures>
#
# Returns non-zero when nothing was written, so a caller using the file as state
# knows it did not persist.
hc_ping_emit() {
  local check="$1" failures="$2" last_ok="$3" streak="$4" out tmp
  [ "$HC_PING_TEXTFILE_DIR" != "/dev/null" ] || return 1
  out="$HC_PING_TEXTFILE_DIR/hc_ping_${check}.prom"
  mkdir -p "$HC_PING_TEXTFILE_DIR" 2>/dev/null || return 1
  tmp="$(mktemp "$out.tmp.XXXXXX" 2>/dev/null)" || return 1
  # Written to a sibling temp file and renamed so node_exporter never reads a
  # half-written exposition; any failure keeps the previous file and drops the temp.
  if {
    echo "# HELP stellarindex_healthcheck_ping_failures_total Pings to Healthchecks.io this host could not deliver."
    echo "# TYPE stellarindex_healthcheck_ping_failures_total counter"
    echo "stellarindex_healthcheck_ping_failures_total{check=\"$check\"} $failures"
    echo "# HELP stellarindex_healthcheck_ping_last_success_unix Unix time the last ping for this check was accepted."
    echo "# TYPE stellarindex_healthcheck_ping_last_success_unix gauge"
    echo "stellarindex_healthcheck_ping_last_success_unix{check=\"$check\"} $last_ok"
    echo "# HELP stellarindex_healthcheck_consecutive_failures Consecutive failed probes/runs behind this check (0 = the last one passed)."
    echo "# TYPE stellarindex_healthcheck_consecutive_failures gauge"
    echo "stellarindex_healthcheck_consecutive_failures{check=\"$check\"} $streak"
  } > "$tmp" && chmod 644 "$tmp"; then
    mv "$tmp" "$out" || { rm -f "$tmp"; return 1; }
  else
    rm -f "$tmp"
    return 1
  fi
}

# hc_ping <check> <url> [curl-arg...]
#
# <check> names the check for the metric label and journal line ("api", "smoke",
# "sla-probe"). <url> is the full ping endpoint, /fail included where a failure
# should be recorded. Remaining arguments go to curl (typically --data-binary).
#
# An empty URL is not an error: units install before the operator pastes the
# URLs in, and the textfile leg is never optional.
hc_ping() {
  local check="$1" url="$2"
  shift 2 || true
  [ -n "$url" ] || return 0

  local file failures last_ok streak rc=0
  file="$HC_PING_TEXTFILE_DIR/hc_ping_${check}.prom"
  failures="$(hc_ping_counter_read "$file" stellarindex_healthcheck_ping_failures_total)"
  last_ok="$(hc_ping_counter_read "$file" stellarindex_healthcheck_ping_last_success_unix)"
  streak="$(hc_ping_counter_read "$file" stellarindex_healthcheck_consecutive_failures)"

  curl -fsS --max-time 10 -o /dev/null --retry 2 "$@" "$url" || rc=$?

  if [ "$rc" -eq 0 ]; then
    last_ok="$(date +%s)"
  else
    failures=$((failures + 1))
    # The URL carries the check's secret: log only the check name and curl's exit code.
    echo "hc-ping: WARN $check ping NOT DELIVERED (curl rc=$rc) — a Healthchecks.io 'down' notice for this check may be about egress, not the service" >&2
  fi
  hc_ping_emit "$check" "$failures" "$last_ok" "$streak"
  return 0
}

# hc_ping_verdict <check> <url> <probe-rc> <fail-after> [curl-arg...]
#
# Reports one probe/run verdict. A pass resets the streak and pings <url>;
# a failure pings <url>/fail only once the streak reaches <fail-after>.
# A non-numeric <fail-after> means 1, so a typo never silences /fail.
hc_ping_verdict() {
  local check="$1" url="$2" probe_rc="$3" after="$4"
  shift 4 || true
  [ -n "$url" ] || return 0
  case "$after" in '' | *[!0-9]*) after=1 ;; esac

  local file streak
  file="$HC_PING_TEXTFILE_DIR/hc_ping_${check}.prom"
  streak=0
  if [ "$probe_rc" -ne 0 ]; then
    streak=$(($(hc_ping_counter_read "$file" stellarindex_healthcheck_consecutive_failures) + 1))
  fi
  # A streak that cannot persist restarts at 1 every run and holds /fail back
  # forever; without the file, report every failure at once.
  hc_ping_emit "$check" \
    "$(hc_ping_counter_read "$file" stellarindex_healthcheck_ping_failures_total)" \
    "$(hc_ping_counter_read "$file" stellarindex_healthcheck_ping_last_success_unix)" \
    "$streak" || after=1

  if [ "$probe_rc" -eq 0 ]; then
    hc_ping "$check" "$url" "$@"
  elif [ "$streak" -ge "$after" ]; then
    hc_ping "$check" "$url/fail" "$@"
  else
    echo "hc-ping: $check failure $streak of $after before /fail — ping held back" >&2
  fi
  return 0
}
