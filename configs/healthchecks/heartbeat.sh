#!/usr/bin/env bash
# Per-binary Healthchecks.io heartbeat.
#
# Hits the binary's metrics scrape endpoint to confirm it's alive,
# then pings the corresponding Healthchecks.io URL. The service
# name (indexer / aggregator / api) is the systemd template
# instance — passed via %i.
#
# URLs come from /etc/default/stellarindex-healthchecks (off-disk
# in git). Empty URL silently skips the ping — lets the timers
# install before the operator has provisioned the URLs.
#
# A failed probe pings /fail only after HC_FAIL_AFTER (default 3)
# consecutive failures: /fail bypasses the check's grace period, so
# one failed probe inside a ~20 s deploy restart cost a down + up
# email pair. The grace period still covers a heartbeat that stops.

set -uo pipefail

# hc_ping records whether the ping actually reached Healthchecks.io.
# Sourced from the script's own directory so a checkout and the
# installed copy under /opt/stellarindex/healthchecks both resolve it.
# shellcheck source=configs/healthchecks/hc-ping.sh
. "$(dirname "${BASH_SOURCE[0]}")/hc-ping.sh"

SERVICE="${1:-${SERVICE_NAME:-}}"
if [ -z "$SERVICE" ]; then
  echo "usage: $0 <indexer|aggregator|api>" >&2
  exit 64
fi

# Env vars (HEALTHCHECKS_URL_*) come from the systemd unit's
# EnvironmentFile= directive, which loads
# /etc/default/stellarindex-healthchecks as root before dropping
# privileges. Do NOT re-source it here — the unprivileged service
# user can't read the 0600 secret file.

# Port mapping (matches configs/prometheus/prometheus.r1.yml).
case "$SERVICE" in
  indexer)    PORT="${INDEXER_METRICS_PORT:-9464}";;
  aggregator) PORT="${AGGREGATOR_METRICS_PORT:-9465}";;
  api)        PORT="${API_METRICS_PORT:-3000}";;
  *)
    echo "heartbeat: unknown service $SERVICE" >&2
    exit 64
    ;;
esac

# URL var name: HEALTHCHECKS_URL_INDEXER, _AGGREGATOR, _API.
URL_VAR="HEALTHCHECKS_URL_$(echo "$SERVICE" | tr '[:lower:]' '[:upper:]')"
PING_URL="${!URL_VAR:-}"

# Probe the metrics endpoint with a 5 s budget. Curl exit codes
# distinguish "process not listening" (7) from "process up, slow"
# (28). Both indicate degraded — the difference matters for
# operators reading logs but not for the heartbeat decision.
PROBE_RC=0
curl -sSf --max-time 5 -o /dev/null "http://localhost:${PORT}/metrics" || PROBE_RC=$?

if [ "$PROBE_RC" -ne 0 ]; then
  echo "heartbeat: $SERVICE probe FAILED on :${PORT} (rc=$PROBE_RC)" >&2
  hc_ping_verdict "$SERVICE" "$PING_URL" "$PROBE_RC" "${HC_FAIL_AFTER:-3}"
  exit 0
fi

# Probe succeeded — ping the heartbeat URL. POST body carries a
# short health summary so the dashboard's "last ping" entry is
# useful at a glance.
hc_ping_verdict "$SERVICE" "$PING_URL" 0 "${HC_FAIL_AFTER:-3}" -d "stellarindex-${SERVICE} ok :${PORT}"

exit 0
