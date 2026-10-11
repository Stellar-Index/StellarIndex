#!/usr/bin/env bash
#
# smoke.sh - wrapper around scripts/dev/r1-smoke.sh that reports the result to
# Healthchecks.io and node_exporter's textfile collector. Unlike heartbeat.sh
# (metrics port up), this verifies the public API surface (schema + data
# integrity): a 200 with malformed JSON, /v1/coins missing `data`, an OpenAPI
# change that breaks clients.
#
# Two sinks:
#   - Healthchecks.io, from HEALTHCHECKS_URL_SMOKE in
#     /etc/default/stellarindex-healthchecks. An empty URL skips the ping.
#   - the textfile $TEXTFILE_DIR/api_smoke.prom, which
#     stellarindex_api_smoke_{failing,stale} evaluate against
#     (configs/prometheus/rules.r1/api-smoke.yml). Written on EVERY run.
#
# The textfile is the non-optional leg: with only the Healthchecks leg, an unset
# URL leaves the one check that sees structurally wrong JSON reporting to
# nothing (no ping, no series, no alert). Journal-only is not coverage.
#
# A failing run pings /fail only after HC_FAIL_AFTER (default 2) consecutive
# failures, so one run overlapping a deploy restart costs no down + up email
# pair; api_smoke.prom still carries every run's verdict immediately.

set -uo pipefail

# hc_ping records whether the ping actually reached Healthchecks.io,
# and shares TEXTFILE_DIR with emit_metric below.
# shellcheck source=configs/healthchecks/hc-ping.sh
. "$(dirname "${BASH_SOURCE[0]}")/hc-ping.sh"

SMOKE_SCRIPT="${SMOKE_SCRIPT:-/opt/stellarindex/healthchecks/r1-smoke.sh}"
URL="${HEALTHCHECKS_URL_SMOKE:-}"
# Same directory + atomic-write convention as the pgbackrest and restore-drill
# emitters. The archival-node role provisions it 0775 group stellarindex;
# stellarindex-smoke.service's DynamicUser joins that group and lists the path
# in ReadWritePaths, or ProtectSystem=strict refuses with EROFS.
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
METRIC_OUT="$TEXTFILE_DIR/api_smoke.prom"

# emit_metric <failed-check-count>
#
# Rewritten on EVERY run so `failures` is this run's verdict and `last_run_unix`
# always advances. `_failing` reads the count; `_stale` reads the timestamp, and
# its absent_over_time branch covers a run that never happened (dead timer,
# fresh host, unwritable dir), which `time() - last_run` cannot see (an absent
# series is an empty vector).
# The tmp is removed on every exit path (failed chmod/mv, SIGTERM from a systemd
# timeout); its name never ends in .prom, so the collector cannot read a partial.
SMOKE_TMP=""
trap '[ -z "$SMOKE_TMP" ] || rm -f "$SMOKE_TMP"' EXIT
trap 'exit 143' TERM INT HUP

emit_metric() {
  local failures="$1" tmp
  [ "$TEXTFILE_DIR" != "/dev/null" ] || return 0
  if mkdir -p "$TEXTFILE_DIR" 2>/dev/null && tmp="$(mktemp "$METRIC_OUT.tmp.XXXXXX" 2>/dev/null)"; then
    SMOKE_TMP="$tmp"
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

# A missing or non-executable smoke script is itself a failure: fan out to
# Healthchecks/fail so a broken install cannot silently disable the check.
if [ ! -x "$SMOKE_SCRIPT" ]; then
  MSG="smoke: $SMOKE_SCRIPT not found or not executable"
  echo "$MSG" >&2
  # Count it as one failed check: an absent write would leave the PREVIOUS run's
  # `failures 0` served for a check that can no longer run (frozen-textfile trap).
  emit_metric 1
  hc_ping smoke "${URL:+${URL}/fail}" --data-binary "$MSG"
  exit 0
fi

# Run the smoke script. Captures its exit code (= number of failed checks) and
# the full output for the Healthchecks.io ping body.
OUT="$(bash "$SMOKE_SCRIPT" 2>&1)"
RC=$?

# On failure the reason must reach the JOURNAL, not only the ping body:
# `journalctl -u stellarindex-smoke` otherwise shows only Starting and Finished,
# and diagnosing an alert would need the dashboard.
if [ "$RC" -ne 0 ]; then
  echo "smoke: FAILED — $RC check(s) tripped:" >&2
  printf '%s\n' "$OUT" | grep -iE '^[[:space:]]*(FAIL|not ok)|✗' >&2 || printf '%s\n' "$OUT" | tail -20 >&2
fi

# Emitted before the ping: the metric is the always-wired sink and must land
# even when curl spends its whole retry budget hanging.
emit_metric "$RC"

hc_ping_verdict smoke "$URL" "$RC" "${HC_FAIL_AFTER:-2}" --data-binary "$OUT"

# Always exit 0 from the timer's perspective — same contract as
# heartbeat.sh. Failures route via the textfile metric the alert rules
# evaluate, the /fail webhook, and journalctl.
exit 0
