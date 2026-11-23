#!/usr/bin/env bash
#
# edge-probe.sh - off-host availability probe of a deployment's PUBLIC API
# edge, reported to Healthchecks.io. Meant to run on a host in a different data
# centre from the deployment it probes: the on-host probes (sla-probe, smoke)
# hit localhost and cannot see a dead proxy, DNS, certificate or host.
#
# Each run, against EDGE_PROBE_BASE_URL (https only, certificate verified,
# redirects never followed):
#   1. GET /v1/readyz must be 200;
#   2. GET EDGE_PROBE_DATA_PATH must be 200 with a non-empty "data" array, so a
#      live proxy in front of a dead backend does not pass;
#   3. the served certificate must be valid for at least EDGE_PROBE_CERT_MIN_DAYS.
# Steps 1-2 are retried once after EDGE_PROBE_RETRY_DELAY seconds: one blip must
# not page. A pass pings the check; a failure pings <url>/fail with a one-line
# reason (status code or curl exit meaning; never a response body or header).
# Silence is also an alert, through the check's grace period.
#
# EDGE_PROBE_PING_URL is the check's secret. It is fed to curl on stdin
# (`-K -`), never as an argument, and is never printed. curl's own stderr is
# discarded because a transport error can echo the URL it was given.

set -uo pipefail

NAME="${EDGE_PROBE_NAME:-edge}"
BASE="${EDGE_PROBE_BASE_URL:-}"
BASE="${BASE%/}"
PING_URL="${EDGE_PROBE_PING_URL:-}"
DATA_PATH="${EDGE_PROBE_DATA_PATH:-/v1/assets?limit=1}"
CERT_MIN_DAYS="${EDGE_PROBE_CERT_MIN_DAYS:-7}"
RETRY_DELAY="${EDGE_PROBE_RETRY_DELAY:-5}"

if [ -z "$PING_URL" ]; then
  echo "edge-probe[$NAME]: EDGE_PROBE_PING_URL is not set; refusing to probe with nowhere to report" >&2
  exit 2
fi
case "$CERT_MIN_DAYS" in '' | *[!0-9]*) CERT_MIN_DAYS=7 ;; esac

BODY="$(mktemp)" || exit 2
trap 'rm -f "$BODY"' EXIT

# send_ping <path-suffix> <reason>. The reason is built here from fixed text,
# a status code or a curl exit code, so it needs no escaping inside the config.
send_ping() {
  local rc=0
  printf 'url = "%s%s"\ndata = "%s"\n' "$PING_URL" "$1" "$2" |
    curl -K - -fsS -o /dev/null --max-time 10 --retry 2 2>/dev/null || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "edge-probe[$NAME]: ping NOT DELIVERED (curl rc=$rc)" >&2
    return 1
  fi
}

curl_meaning() {
  case "$1" in
    6) echo "dns" ;;
    7) echo "connect refused" ;;
    28) echo "timeout" ;;
    35 | 51 | 58 | 59 | 60 | 77 | 80 | 82 | 83 | 90 | 91) echo "tls" ;;
    *) echo "transport" ;;
  esac
}

finish() { # <ok|fail> <reason>
  if [ "$1" = ok ]; then
    echo "edge-probe[$NAME]: ok"
    send_ping "" "$NAME ok" || exit 1
  else
    echo "edge-probe[$NAME]: FAIL $2" >&2
    send_ping "/fail" "$NAME: $2" || exit 1
  fi
  exit 0
}

case "$BASE" in
  https://?*) ;;
  *) finish fail "config: EDGE_PROBE_BASE_URL must be an https:// URL" ;;
esac

# fetch <path>: sets CODE and REASON; returns 0 only for HTTP 200.
fetch() {
  local rc=0
  : >"$BODY"
  CODE="$(curl -sS -o "$BODY" -w '%{http_code}' --proto '=https' --tlsv1.2 \
    --connect-timeout 5 --max-time 15 --max-filesize 2000000 \
    -A 'stellarindex-edge-probe/1' "$BASE$1" 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    REASON="$1 curl rc=$rc ($(curl_meaning "$rc"))"
    return 1
  fi
  if [ "$CODE" != 200 ]; then
    REASON="$1 status $CODE"
    return 1
  fi
}

attempt() {
  fetch /v1/readyz || return 1
  fetch "$DATA_PATH" || return 1
  if ! grep -Eq '"data"[[:space:]]*:[[:space:]]*\[[[:space:]]*\{' "$BODY"; then
    REASON="$DATA_PATH 200 but no data rows"
    return 1
  fi
}

REASON=""
if ! attempt; then
  sleep "$RETRY_DELAY"
  attempt || finish fail "$REASON"
fi

# Certificate lifetime. Chain/hostname validity was already enforced by curl
# above (verification is never disabled); this reads only the expiry.
HOSTPORT="${BASE#https://}"
HOSTPORT="${HOSTPORT%%/*}"
HOST="${HOSTPORT%%:*}"
case "$HOSTPORT" in *:*) PORT="${HOSTPORT##*:}" ;; *) PORT=443 ;; esac
TO=()
command -v timeout >/dev/null 2>&1 && TO=(timeout 20)
PEM="$(: | ${TO[@]+"${TO[@]}"} openssl s_client -connect "$HOST:$PORT" -servername "$HOST" 2>/dev/null |
  openssl x509 2>/dev/null)"
if [ -z "$PEM" ]; then
  finish fail "cert: could not read the served certificate"
fi
if ! printf '%s\n' "$PEM" | openssl x509 -noout -checkend $((CERT_MIN_DAYS * 86400)) >/dev/null 2>&1; then
  finish fail "cert: expires within $CERT_MIN_DAYS days"
fi

finish ok ""
