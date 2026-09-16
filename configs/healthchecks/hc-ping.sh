#!/usr/bin/env bash
#
# hc-ping.sh — deliver one Healthchecks.io ping and make the delivery
# itself observable. Sourced by heartbeat.sh, smoke.sh and sla-probe.sh;
# not executable on its own.
#
# Every ping in this directory used to end in `|| true`. That is the
# right exit-code contract — a monitoring wrapper must never fail the
# unit it monitors — but it also threw away the one fact an operator
# needs when Healthchecks.io reports a check down on a host that was
# healthy throughout: the ping never left the box.
#
# A dropped ping and a dead service produce the SAME "down" email.
# Healthchecks.io cannot tell them apart — it only ever sees silence —
# and until now neither could the host, because curl's failure went to
# /dev/null along with its output. That is the shape of an alert that
# reads as noise: it fires on a healthy service, no evidence survives,
# and the next one gets trusted a little less.
#
# hc_ping keeps the exit-code contract and records what happened in two
# places that outlive the run:
#
#   - the journal, at WARN, naming the check and curl's exit code;
#   - node_exporter's textfile collector, as a per-check failure counter
#     and a last-success timestamp, which
#     `stellarindex_healthcheck_ping_undelivered` evaluates.
#
# The metric is what makes the distinction actionable: when the counter
# moves, the next Healthchecks.io email is about our egress, not about
# the service under the check.

# TEXTFILE_DIR is shared with smoke.sh's emit_metric. Callers may set it
# to /dev/null to opt out (the test harness does); a caller that cannot
# write there still pings, and still logs.
HC_PING_TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"

# hc_ping_counter_read <file> <metric>
#
# Reads a counter back out of a textfile we wrote on an earlier run.
# A counter has to survive the process that emits it, and the textfile
# collector is the only state these oneshot units have. An unreadable or
# absent file reads 0 — a restarted counter is visible to `increase()`
# as a reset, which is correct, where inventing a number would not be.
hc_ping_counter_read() {
  local file="$1" metric="$2"
  [ -r "$file" ] || { echo 0; return 0; }
  awk -v m="$metric" '$0 ~ "^" m "[{ ]" { v=$NF } END { print (v == "" ? 0 : v) }' \
    "$file" 2>/dev/null || echo 0
}

# hc_ping_emit <check> <failures-total> <last-success-unix>
hc_ping_emit() {
  local check="$1" failures="$2" last_ok="$3" out tmp
  [ "$HC_PING_TEXTFILE_DIR" != "/dev/null" ] || return 0
  out="$HC_PING_TEXTFILE_DIR/hc_ping_${check}.prom"
  mkdir -p "$HC_PING_TEXTFILE_DIR" 2>/dev/null || return 0
  tmp="$(mktemp "$out.tmp.XXXXXX" 2>/dev/null)" || return 0
  # Written whole to a sibling temp file and renamed, so node_exporter
  # never reads a half-written exposition. Any failure along the way
  # leaves the previous file in place and takes the temp with it.
  if {
    echo "# HELP stellarindex_healthcheck_ping_failures_total Pings to Healthchecks.io this host could not deliver."
    echo "# TYPE stellarindex_healthcheck_ping_failures_total counter"
    echo "stellarindex_healthcheck_ping_failures_total{check=\"$check\"} $failures"
    echo "# HELP stellarindex_healthcheck_ping_last_success_unix Unix time the last ping for this check was accepted."
    echo "# TYPE stellarindex_healthcheck_ping_last_success_unix gauge"
    echo "stellarindex_healthcheck_ping_last_success_unix{check=\"$check\"} $last_ok"
  } > "$tmp" && chmod 644 "$tmp"; then
    mv "$tmp" "$out" || rm -f "$tmp"
  else
    rm -f "$tmp"
  fi
}

# hc_ping <check> <url> [curl-arg...]
#
# <check> names the Healthchecks.io check for the metric label and the
# journal line — "api", "smoke", "sla-probe". <url> is the full ping
# endpoint, /fail suffix included where the caller wants a failure
# recorded. Remaining arguments are passed to curl (typically --data-binary).
#
# An empty URL is not an error: the units install before the operator
# has pasted the Healthchecks.io URLs in, and the textfile leg is the
# one that is never optional.
hc_ping() {
  local check="$1" url="$2"
  shift 2 || true
  [ -n "$url" ] || return 0

  local file failures last_ok rc=0
  file="$HC_PING_TEXTFILE_DIR/hc_ping_${check}.prom"
  failures="$(hc_ping_counter_read "$file" stellarindex_healthcheck_ping_failures_total)"
  last_ok="$(hc_ping_counter_read "$file" stellarindex_healthcheck_ping_last_success_unix)"

  curl -fsS --max-time 10 -o /dev/null --retry 2 "$@" "$url" || rc=$?

  if [ "$rc" -eq 0 ]; then
    last_ok="$(date +%s)"
  else
    failures=$((failures + 1))
    # The URL carries the check's secret, so the journal gets the check
    # name and curl's exit code and nothing else.
    echo "hc-ping: WARN $check ping NOT DELIVERED (curl rc=$rc) — a Healthchecks.io 'down' notice for this check may be about egress, not the service" >&2
  fi
  hc_ping_emit "$check" "$failures" "$last_ok"
  return 0
}
