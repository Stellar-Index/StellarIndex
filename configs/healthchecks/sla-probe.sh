#!/usr/bin/env bash
#
# sla-probe.sh - wrapper around `stellarindex-sla-probe` that reports pass/fail
# to Healthchecks.io. Unlike smoke.sh (API surface), this drives load and asserts
# the latency, freshness and availability SLAs (p95 <= 200 ms, p99 <= 500 ms,
# freshness ≤ 30 s, availability ≥ 99.9 %, as published at stellarindex.io/sla).
#
# Binary exit code is 0 (pass) or 1 (an SLA violated). Both branches POST the
# full JSON report to Healthchecks.io so the dashboard shows which percentile or
# endpoint tripped.
#
# URL comes from /etc/default/stellarindex-healthchecks
# (HEALTHCHECKS_URL_SLA_PROBE). An empty URL runs the probe for journal-only
# coverage.

set -uo pipefail

# hc_ping records whether the ping actually reached Healthchecks.io.
# shellcheck source=configs/healthchecks/hc-ping.sh
. "$(dirname "${BASH_SOURCE[0]}")/hc-ping.sh"

PROBE_BIN="${PROBE_BIN:-/usr/local/bin/stellarindex-sla-probe}"
BASE_URL="${SLA_PROBE_BASE_URL:-http://localhost:3000/v1}"
# 30s default keeps the probe cheap, but on a memory-pressured single-instance
# host a ~800-request window is too sensitive to timeout jitter (one straggler
# at 99.9% = unit_failed=1). Such hosts should set SLA_PROBE_DURATION=120s or
# longer in /etc/default/stellarindex-healthchecks.
DURATION="${SLA_PROBE_DURATION:-30s}"
# Default concurrency=1, not 2: at 2 the probe drives ~2.5k req/s, which tripped
# the operator-tier rate limit (429s read as fake availability failure) and
# saturated the API request path (p95 ~3.8s, a false positive). At 1, ~800 req
# over 30s still stresses the closed-bucket lookup enough to surface real
# regressions. Multi-instance HA can override via inventory.
CONCURRENCY="${SLA_PROBE_CONCURRENCY:-1}"
PAIR="${SLA_PROBE_PAIR:-native,fiat:USD}"
URL="${HEALTHCHECKS_URL_SLA_PROBE:-}"
# TEXTFILE_OUTPUT - path under node_exporter's textfile_collector dir where the
# probe writes per-endpoint p50/p95/p99 + freshness + verdict metrics. Without
# it the alerts in deploy/monitoring/rules/sla-probe.yml have no series. The
# archival-node role provisions the dir and the --collector.textfile flag, so
# this defaults on. Set empty to disable.
TEXTFILE_OUTPUT="${SLA_PROBE_TEXTFILE_OUTPUT:-/var/lib/node_exporter/textfile_collector/sla_probe.prom}"

# A missing or non-executable probe binary is itself a failure: fan out to
# Healthchecks/fail so a broken deploy cannot silently disable the check.
if [ ! -x "$PROBE_BIN" ]; then
  MSG="sla-probe: $PROBE_BIN not found or not executable"
  echo "$MSG" >&2
  hc_ping sla-probe "${URL:+${URL}/fail}" --data-binary "$MSG"
  exit 0
fi

# Omit -textfile-output entirely when TEXTFILE_OUTPUT is blank: the binary's
# flag parser rejects an empty value.
TEXTFILE_FLAG=()
if [ -n "$TEXTFILE_OUTPUT" ]; then
  TEXTFILE_FLAG=(-textfile-output "$TEXTFILE_OUTPUT")
fi

# Run the probe. JSON report on stdout; pass=0, fail=1 on exit.
#
# The probe's auth variable is deliberately NOT passed as a flag: argv is
# world-readable (`ps`, /proc/<pid>/cmdline) and would leak it. The unit's
# EnvironmentFile already exports STELLARINDEX_PROBE_API_KEY and the binary falls
# back to it when `-api-key` is empty (resolveAPIKey, cmd/stellarindex-sla-probe/main.go).
OUT="$(
  "$PROBE_BIN" \
    -base-url "$BASE_URL" \
    -duration "$DURATION" \
    -concurrency "$CONCURRENCY" \
    -pair "$PAIR" \
    -report-format json \
    "${TEXTFILE_FLAG[@]}" 2>&1
)"
RC=$?

# An unauthenticated probe measures our rate limiter, not our SLA. Say so loudly
# rather than pinging /fail and sending an operator after a phantom outage.
if [ -z "${STELLARINDEX_PROBE_API_KEY:-}" ]; then
  echo "sla-probe: WARNING no STELLARINDEX_PROBE_API_KEY — this run measured the anonymous rate limit, not the SLA" >&2
fi

# A failing run pings /fail only after HC_FAIL_AFTER (default 2) consecutive
# failures; /fail bypasses the grace period, so one run caught by a restart
# would otherwise email twice.
hc_ping_verdict sla-probe "$URL" "$RC" "${HC_FAIL_AFTER:-2}" --data-binary "$OUT"

# Always exit 0 from the timer's perspective; failures route via the /fail
# webhook + journalctl, as with the other Healthchecks-driven timers.
exit 0
