#!/bin/bash
#
# node-healthcheck.sh: periodic health probe for archival-node services and
# network state. Pings a Healthchecks.io-compatible URL on success and, with a
# diagnostic body, on failure.
#
# Checks (all must pass):
#   1.   All 9 systemd services are `active`
#   1b.  stellarindex-api's /v1/healthz returns 200
#   4.5. galexie's most recent MinIO upload isn't stale (we track network head)
#   5.   ZFS data pool state == ONLINE
#   6.   galexie/stellar-rpc captive-core dirs are < 90% full
#
# HEALTHCHECK_PING_URL comes from /etc/default/node-healthcheck, rendered by
# Ansible from a vault variable: the URL is a shared secret (anyone with it can
# forge 'healthy' pings) and must not sit in git in cleartext.
#
# Exit 0 always, so systemd never marks the timer failed; failures are reported
# to healthchecks.io by the script itself.

set -uo pipefail

# --- Load config ------------------------------------------------
# Under the unit this file is root-only and systemd has already loaded it
# via EnvironmentFile=; sourcing is for a by-hand run as root.
if [ -r /etc/default/node-healthcheck ]; then
  # shellcheck disable=SC1091
  . /etc/default/node-healthcheck
fi

PING_URL="${HEALTHCHECK_PING_URL:-}"
MAX_LEDGER_AGE_SEC="${MAX_LEDGER_AGE_SEC:-90}"
POOL_NAME="${POOL_NAME:-data}"

# No ping URL: exit 0 silently so unit install is idempotent before the secret
# is populated.
if [ -z "$PING_URL" ]; then
  echo "node-healthcheck: HEALTHCHECK_PING_URL unset — skipping" >&2
  exit 0
fi

# --- Accumulator -----------------------------------------------
FAILS=()
add_fail() { FAILS+=("$1"); }

# --- Check 1: systemd service liveness ---
# Storage and ingestion plumbing, then the application services (watched so a
# crashed indexer pings failure). Deliberately absent: primary stellar-core,
# stellar-core-prometheus-exporter (scraped primary), and stellar-rpc (redundant;
# the indexer consumes galexie's MinIO output directly).
#
# A unit may be absent on a non-application-tier node (e.g. a read-only mirror):
# skip it when `inactive` and no unit file exists; only `failed` is a real fault.
SERVICES=(
  postgresql@15-main
  redis-server
  galexie
  minio
  # prometheus-node-exporter, NOT node_exporter: the cutover to the packaged unit
  # deliberately leaves the old hand-rolled node_exporter.service in place, stopped,
  # as a rollback path. That defeats the `systemctl cat` guard below ("unit file
  # exists, so this host should run it"), so checking the old name faulted on
  # every run and pinged /fail every 5 minutes while metrics were fine on :9100.
  prometheus-node-exporter
  stellarindex-indexer
  stellarindex-aggregator
  stellarindex-api
  # cap67-movements: the account-movements FOLLOW DAEMON. An always-on writer of
  # the money trail must page when dead or crash-looped (StartLimit -> failed); a
  # wedged daemon does not self-heal like the timer+oneshot it replaced.
  cap67-movements
)
for s in "${SERVICES[@]}"; do
  state=$(systemctl is-active "$s" 2>&1)
  case "$state" in
    active)
      ;;
    inactive)
      # Fault for inactive only when the unit file exists: `systemctl is-active`
      # says 'inactive' both for stopped and for not loaded, so a node that doesn't
      # run a service must not false-positive.
      if systemctl cat "$s" >/dev/null 2>&1; then
        add_fail "service $s is inactive (expected active)"
      fi
      ;;
    *)
      add_fail "service $s is $state (expected active)"
      ;;
  esac
done

# --- Check 1b: API /v1/healthz returning 200 ---
# A running process only means systemd thinks it's alive; the listener may be
# wedged on a deadlock. /v1/healthz is the wire-honest "API is serving" signal.
# Skipped if the api unit isn't installed (as in Check 1).
if systemctl cat stellarindex-api >/dev/null 2>&1; then
  api_status=$(curl -sS -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:3000/v1/healthz 2>&1 || echo CURL_FAILED)
  if [ "$api_status" != "200" ]; then
    add_fail "stellarindex-api /v1/healthz returned $api_status (expected 200)"
  fi
fi

# No stellar-core /info or stellar-rpc getHealth probe: "is the network being
# followed?" is exclusively Check 4.5 below.

# --- Check 4.5: galexie upload freshness ---
# Galexie's metrics endpoint (admin_port 6061) can HANG for minutes when
# captive-core is stuck, so it is no liveness signal. Instead check the mtime of
# the newest object in the galexie-live bucket (a new one lands ~every 5s, one
# per closed ledger): fail if > GALEXIE_MAX_LAG_SEC old and galexie has run >
# GALEXIE_WARMUP_SEC.
#
# The unit runs DynamicUser+ProtectHome=true, so a persisted `mc alias set` under
# /root/.mc is namespace-hidden. Auth comes from the MC_HOST_local env var (set in
# /etc/default/node-healthcheck, root:root 0600, no_log). An empty last_iso means
# either "mc broken" or "bucket empty", indistinguishable from the listing, so we
# track mc's own exit code.
GALEXIE_MAX_LAG_SEC="${GALEXIE_MAX_LAG_SEC:-600}"
GALEXIE_WARMUP_SEC="${GALEXIE_WARMUP_SEC:-1800}"
g_enter_iso=$(systemctl show -p ActiveEnterTimestamp --value galexie 2>/dev/null)
g_enter_epoch=$(date -d "$g_enter_iso" +%s 2>/dev/null || echo 0)
g_age=$(( $(date +%s) - g_enter_epoch ))
if [ "$g_age" -gt "$GALEXIE_WARMUP_SEC" ]; then
  # mc --json gives a machine-readable listing; sort by lastModified. `sort` writes
  # to a file and `head` reads it back: with pipefail, `sort -r | head -1` makes
  # sort die on EPIPE once the listing exceeds the pipe buffer, logging "Broken
  # pipe" every run. pipefail still lets us read mc's exit code below.
  mc ls --json --recursive local/galexie-live/ 2>/dev/null \
    | jq -r 'select(.key | test("\\.xdr\\.zst$")) | .lastModified' 2>/dev/null \
    | sort -r > /tmp/galexie-live-mtimes.txt
  mc_rc=$?
  last_iso=$(head -1 /tmp/galexie-live-mtimes.txt)
  if [ -n "$last_iso" ]; then
    last_epoch=$(date -d "$last_iso" +%s 2>/dev/null || echo 0)
    lag=$(( $(date +%s) - last_epoch ))
    if [ "$lag" -gt "$GALEXIE_MAX_LAG_SEC" ]; then
      add_fail "galexie last upload was ${lag}s ago (threshold ${GALEXIE_MAX_LAG_SEC}s) — captive-core likely stuck"
    fi
  elif [ "$mc_rc" -ne 0 ]; then
    add_fail "mc ls local/galexie-live/ failed (rc=$mc_rc) — cannot verify galexie upload freshness"
  fi
  # last_iso empty AND mc_rc == 0: bucket has no matching objects yet; left to
  # check 1 (minio service) and manual inspection.
fi

# --- Check 5: ZFS pool state -----------------------------------
pool_state=$(zpool list -H -o health "$POOL_NAME" 2>&1 || echo "MISSING")
if [ "$pool_state" != "ONLINE" ]; then
  add_fail "zpool $POOL_NAME state=$pool_state (expected ONLINE)"
fi

# --- Check 6: disk free on the captive-core bucket dirs --------
# galexie's captive-core is the primary producer; stellar-rpc's is secondary.
for d in /var/lib/galexie /var/lib/stellar-rpc; do
  [ -d "$d" ] || continue
  disk_pct_used=$(df --output=pcent "$d" | tail -1 | tr -d ' %')
  if [ "${disk_pct_used:-0}" -gt 90 ]; then
    add_fail "$d is ${disk_pct_used}% full"
  fi
done

# --- Report ----------------------------------------------------
if [ ${#FAILS[@]} -eq 0 ]; then
  curl -sfm 10 -o /dev/null "$PING_URL" || true
  exit 0
fi

body="$(printf '%s\n' "${FAILS[@]}")"
echo "node-healthcheck: ${#FAILS[@]} failure(s) — reporting to healthchecks.io" >&2
printf '%s\n' "$body" >&2
curl -sfm 10 -o /dev/null --data-raw "$body" "$PING_URL/fail" || true
exit 0
