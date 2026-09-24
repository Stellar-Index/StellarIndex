#!/bin/bash
# Phase D1 single-range archive-walk (parameterized, so ranges run concurrently).
# args: FROM TO STATE_FILE [PARALLEL]
# A window that fails PHASED_MAX_ATTEMPTS times in a row stops the run non-zero;
# re-running resumes from it (the state file records successes only).
set -uo pipefail
RFROM=$1; RTO=$2; STATE=$3; PAR=${4:-4}
LOG="${PHASED_LOG:-/var/log/phaseD-backfill.log}"
MAX_ATTEMPTS="${PHASED_MAX_ATTEMPTS:-3}"
case "$MAX_ATTEMPTS" in ''|*[!0-9]*|0) echo "phaseD-range: PHASED_MAX_ATTEMPTS='$MAX_ATTEMPTS' must be a positive integer" >&2; exit 2 ;; esac
FLOOR_KB=524288000   # 500 GiB floor
mkdir -p "$(dirname "$STATE")"; touch "$STATE"
# Read a systemd EnvironmentFile VERBATIM — never `.`/source it. Its
# values are unquoted (that is what systemd wants), so the shell would
# expand `$`, split on `;`/`&`/`|`/whitespace and eat quotes inside a
# secret: the services keep working while this path gets a mangled DSN
# (deploy-ansible-secrets-5). Same reader as run-heavy-job.sh.
# usage: load_env_file FILE [export]
load_env_file() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      [A-Za-z_]*=*)
        if [ "${2:-}" = export ]; then
          export "${line?}"
        else
          printf -v "${line%%=*}" '%s' "${line#*=}"
        fi
        ;;
    esac
  done < "$1"
}
for f in /etc/default/stellarindex-ops /etc/default/stellarindex; do
  [ -r "$f" ] && load_env_file "$f" export
done
OPS="${PHASED_OPS:-/usr/local/bin/stellarindex-ops}"
echo "$(date -u +%FT%TZ) RANGE_START [$RFROM,$RTO] par=$PAR state=$STATE" >> "$LOG"
w=$RFROM
attempts=0
while [ "$w" -le "$RTO" ]; do
  wto=$((w + 999999)); [ "$wto" -gt "$RTO" ] && wto=$RTO
  if grep -qx "$w" "$STATE"; then w=$((wto + 1)); continue; fi
  avail=$(df --output=avail -k /var/lib/clickhouse | tail -1 | tr -d ' ')
  case "$avail" in ''|*[!0-9]*) sleep 60; continue ;; esac
  if [ "$avail" -lt "$FLOOR_KB" ]; then echo "$(date -u +%FT%TZ) [$RFROM] PAUSE <500G ($avail) before $w" >> "$LOG"; sleep 300; continue; fi
  echo "$(date -u +%FT%TZ) [$RFROM] window $w-$wto START avail=${avail}KiB" >> "$LOG"
  if "$OPS" ch-backfill -write -config /etc/stellarindex.toml -bucket galexie-archive -parallel "$PAR" -flush-every 200 -from "$w" -to "$wto" >> "$LOG" 2>&1; then
    echo "$w" >> "$STATE"; echo "$(date -u +%FT%TZ) [$RFROM] window $w-$wto DONE (avail $(df --output=avail -k /var/lib/clickhouse | tail -1 | tr -d ' ')KiB)" >> "$LOG"
  else
    rc=$?; attempts=$((attempts + 1))
    if [ "$attempts" -ge "$MAX_ATTEMPTS" ]; then
      echo "$(date -u +%FT%TZ) [$RFROM] window $w-$wto FAILED (exit $rc) attempt $attempts/$MAX_ATTEMPTS — stopping; re-run to resume from $w" >> "$LOG"
      exit "$rc"
    fi
    echo "$(date -u +%FT%TZ) [$RFROM] window $w-$wto FAILED (exit $rc) attempt $attempts/$MAX_ATTEMPTS — retrying in 30s" >> "$LOG"; sleep 30; continue
  fi
  attempts=0
  w=$((wto + 1))
done
echo "$(date -u +%FT%TZ) RANGE [$RFROM,$RTO] COMPLETE" >> "$LOG"
