#!/usr/bin/env bash
#
# smoke.sh — wrapper around scripts/dev/r1-smoke.sh that reports
# the result to Healthchecks.io and to node_exporter's textfile
# collector. Distinct from heartbeat.sh: that probes the metrics
# port (process up); this one verifies the public API surface
# (schema + data integrity).
#
# Catches regressions the per-binary heartbeats can't see — e.g.
# /v1/price returning a 200 with malformed JSON, /v1/coins missing
# the `data` field, an OpenAPI-spec change that breaks downstream
# clients.
#
# Two sinks, one optional and one not:
#
#   - Healthchecks.io, from HEALTHCHECKS_URL_SMOKE in
#     /etc/default/stellarindex-healthchecks. An empty URL skips the
#     ping — and that variable has been empty on r1 since install.
#   - the node_exporter textfile at $TEXTFILE_DIR/api_smoke.prom,
#     which stellarindex_api_smoke_{failing,stale} evaluate against
#     (configs/prometheus/rules.r1/api-smoke.yml). Written on every
#     run, unconditionally.
#
# The textfile is the leg that is not optional. With only the
# Healthchecks leg, an unset URL left a 5-minute timer running 34 jq
# shape assertions — the one check that can see a 200 carrying
# structurally wrong JSON — reporting to nothing at all: no ping, no
# series, no alert rule. "Journal-only coverage" is not coverage;
# nothing reads a journal until someone already suspects a problem.

set -uo pipefail

SMOKE_SCRIPT="${SMOKE_SCRIPT:-/opt/stellarindex/healthchecks/r1-smoke.sh}"
URL="${HEALTHCHECKS_URL_SMOKE:-}"
# Same directory + atomic-write convention as the pgbackrest and
# restore-drill emitters. The archival-node role provisions the
# directory 0775 group stellarindex; stellarindex-smoke.service puts
# its DynamicUser in that group and lists the path in ReadWritePaths,
# which ProtectSystem=strict would otherwise refuse with EROFS.
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
METRIC_OUT="$TEXTFILE_DIR/api_smoke.prom"

# emit_metric <failed-check-count>
#
# Rewritten on EVERY run, pass or fail, so `failures` is this run's
# verdict rather than a survivor of an older one, and `last_run_unix`
# always advances. The two alerts split on exactly that: `_failing`
# reads the count, `_stale` reads the timestamp — and its
# absent_over_time branch is what covers the run that never happened
# (dead timer, fresh host, unwritable directory), which a plain
# `time() - last_run` comparison can never see, because an absent
# series is an empty vector.
emit_metric() {
  local failures="$1" tmp
  [ "$TEXTFILE_DIR" != "/dev/null" ] || return 0
  if mkdir -p "$TEXTFILE_DIR" 2>/dev/null && tmp="$(mktemp "$METRIC_OUT.tmp.XXXXXX" 2>/dev/null)"; then
    {
      echo "# HELP stellarindex_api_smoke_failures Failed checks in the most recent API smoke run (0 = the whole surface passed)."
      echo "# TYPE stellarindex_api_smoke_failures gauge"
      echo "stellarindex_api_smoke_failures $failures"
      echo "# HELP stellarindex_api_smoke_last_run_unix Unix time the most recent API smoke run finished, pass or fail."
      echo "# TYPE stellarindex_api_smoke_last_run_unix gauge"
      echo "stellarindex_api_smoke_last_run_unix $(date +%s)"
    } > "$tmp" && chmod 644 "$tmp" && mv "$tmp" "$METRIC_OUT" ||
      echo "smoke: WARN could not write $METRIC_OUT — this run's verdict is not observable" >&2
  else
    echo "smoke: WARN $TEXTFILE_DIR not writable — smoke metrics not emitted (stellarindex_api_smoke_stale fires on the absent series)" >&2
  fi
}

# F-1302 (codex audit-2026-05-13): a missing or non-executable
# smoke script is itself a failure — fan out to Healthchecks/fail
# so the 5-min API-surface check goes red, otherwise a broken
# install silently disables the check without anyone noticing.
if [ ! -x "$SMOKE_SCRIPT" ]; then
  MSG="smoke: $SMOKE_SCRIPT not found or not executable"
  echo "$MSG" >&2
  # Count it as one failed check rather than emitting nothing: an
  # absent write would leave the PREVIOUS run's `failures 0` in place
  # for node_exporter to keep serving on behalf of a check that can no
  # longer run — the frozen-textfile trap the data-freshness watchdog
  # hit (#319).
  emit_metric 1
  if [ -n "$URL" ]; then
    curl -fsS --max-time 10 -o /dev/null --retry 2 \
      --data-binary "$MSG" \
      "${URL}/fail" || true
  fi
  exit 0
fi

# Run the smoke script. Captures its exit code (= number of failed
# checks per the script's contract) and the full output for the
# Healthchecks.io ping body — operators reading the dashboard see
# exactly which checks tripped without leaving the page.
OUT="$(bash "$SMOKE_SCRIPT" 2>&1)"
RC=$?

# Emitted before the ping: the metric is the always-wired sink, and it
# must land even when curl spends its whole retry budget hanging.
emit_metric "$RC"

if [ -n "$URL" ]; then
  if [ "$RC" -eq 0 ]; then
    curl -fsS --max-time 10 -o /dev/null --retry 2 \
      --data-binary "$OUT" \
      "$URL" || true
  else
    curl -fsS --max-time 10 -o /dev/null --retry 2 \
      --data-binary "$OUT" \
      "${URL}/fail" || true
  fi
fi

# Always exit 0 from the timer's perspective — same contract as
# heartbeat.sh. Failures route via the textfile metric the alert rules
# evaluate, the /fail webhook, and journalctl.
exit 0
