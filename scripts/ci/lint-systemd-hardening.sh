#!/usr/bin/env bash
# lint-systemd-hardening.sh — every unit the archival-node role renders names
# the account it runs as and carries the baseline sandbox, and every
# wall-clock timer pins its zone. Past outliers (a unit with no sandbox, one
# running as root by omission, a timer at host-local 02:00) shared one cause:
# nothing noticed a directive that was simply absent.
#
# Rules, over $ROLE/templates/systemd and every deploy/systemd unit a role
# task installs verbatim (same rules, minus the .j2 suffix):
#   *.service.j2  — User= or DynamicUser=yes (running as root must be
#                   written down, not inherited), and each of
#                   NoNewPrivileges PrivateTmp ProtectKernelTunables
#                   ProtectKernelModules ProtectControlGroups set and not
#                   disabled. DynamicUser=yes implies the first two.
#   *.timer.j2    — an OnCalendar= with a fixed hour ends in " UTC".
#   *.service.j2 with a *.timer.j2 beside it — no Wants=, Requires=,
#                   BindsTo= or Requisite= on ClickHouse, MinIO, Postgres
#                   or Redis: every tick would start a datastore an
#                   operator stopped for maintenance. After= orders
#                   without starting.
#
# A unit that cannot take a directive is declared in
# $ROLE/templates/systemd/HARDENING-EXCEPTIONS as <unit>:<directive> with
# the reason. A declared exception the unit no longer needs fails too, so
# the list only shrinks.
#
# Run: bash scripts/ci/lint-systemd-hardening.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

DEFAULT_ROLE=configs/ansible/roles/archival-node
ROLE="${SYSTEMD_HARDENING_ROLE:-$DEFAULT_ROLE}"
DEPLOY_DIR="${SYSTEMD_HARDENING_DEPLOY_DIR:-deploy/systemd}"
DIR="$ROLE/templates/systemd"
EXCEPTIONS="$DIR/HARDENING-EXCEPTIONS"
BASELINE="NoNewPrivileges PrivateTmp ProtectKernelTunables ProtectKernelModules ProtectControlGroups"
fail=0
units=0
timers=0

[ -d "$DIR" ] || { echo "lint-systemd-hardening: $DIR missing"; exit 2; }

declared=""
# POSIX classes, not \s: BSD sed reads `\s` as a literal `s` and would
# strip the trailing s off NoNewPrivileges.
[ -f "$EXCEPTIONS" ] && declared=$(grep -vE '^[[:space:]]*(#|$)' "$EXCEPTIONS" | sed -E 's/[[:space:]]*#.*//; s/[[:space:]]+$//')

# value_of <file> <directive> — the last assignment's value, empty if unset.
value_of() {
  grep -E "^[[:space:]]*$2[[:space:]]*=" "$1" | tail -1 | sed -E 's/^[^=]*=[[:space:]]*//; s/[[:space:]]+$//'
}

# enabled <value> — set to something other than a systemd false.
enabled() {
  [ -n "$1" ] && ! grep -qixE 'no|false|off|0' <<<"$1"
}

# check_service <file> <unit> <timer-file>
check_service() {
  local f="$1" unit="$2" timer="$3" dynamic missing d dep
  units=$((units + 1))
  dynamic=$(value_of "$f" DynamicUser)
  missing=""
  if [ -z "$(value_of "$f" User)" ] && ! enabled "$dynamic"; then
    missing="User"
  fi
  for d in $BASELINE; do
    if enabled "$dynamic" && { [ "$d" = NoNewPrivileges ] || [ "$d" = PrivateTmp ]; }; then
      continue
    fi
    enabled "$(value_of "$f" "$d")" || missing="$missing $d"
  done
  if [ -f "$timer" ]; then
    while IFS= read -r dep; do
      echo "  STARTS-DATASTORE: $f is timer-driven and has '$dep' — each tick starts a stopped datastore; keep After= only"
      fail=$((fail + 1))
    done < <(grep -E '^[[:space:]]*(Wants|Requires|BindsTo|Requisite)[[:space:]]*=.*(clickhouse-server|minio|postgresql|redis)' "$f")
  fi
  for d in $missing; do
    grep -qxF "$unit:$d" <<<"$declared" && continue
    echo "  UNHARDENED: $f does not set $d, and $unit:$d is not declared in $EXCEPTIONS"
    fail=$((fail + 1))
  done
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    if ! grep -qwF "$d" <<<"$missing"; then
      echo "  STALE: $EXCEPTIONS declares $unit:$d but $f no longer needs it — delete the line"
      fail=$((fail + 1))
    fi
  done < <(awk -F: -v u="$unit" '$1 == u { print $2 }' <<<"$declared")
}

# check_timer <file>
check_timer() {
  local f="$1" cal
  timers=$((timers + 1))
  while IFS= read -r cal; do
    case "$cal" in *'{{'*) continue ;; esac
    grep -qE '(^|[[:space:]])[0-9]{1,2}(,[0-9]{1,2})*:' <<<"$cal" || continue
    grep -qE '[[:space:]]UTC$' <<<"$cal" && continue
    echo "  UNPINNED: $f OnCalendar=$cal fixes an hour but not a zone — append UTC"
    fail=$((fail + 1))
  done < <(grep -E '^[[:space:]]*OnCalendar[[:space:]]*=' "$f" | sed -E 's/^[^=]*=[[:space:]]*//; s/[[:space:]]+$//')
}

for f in "$DIR"/*.service.j2; do
  [ -f "$f" ] || continue
  check_service "$f" "$(basename "$f" .j2)" "${f%.service.j2}.timer.j2"
done
for f in "$DIR"/*.timer.j2; do
  [ -f "$f" ] || continue
  check_timer "$f"
done

# Units the role installs verbatim from deploy/systemd run as written there,
# so they carry the same baseline (exceptions in the same file).
verbatim=0
if [ -d "$ROLE/tasks" ]; then
  installed=$(python3 "$(dirname "$0")/deploy-systemd-authoritative.py" "$ROLE") || exit 2
  while IFS= read -r b; do
    [ -n "$b" ] || continue
    f="$DEPLOY_DIR/$b"
    if [ ! -f "$f" ]; then
      echo "  MISSING: the role installs $b from $DEPLOY_DIR, which has no such file"
      fail=$((fail + 1))
      continue
    fi
    verbatim=$((verbatim + 1))
    case "$b" in
      *.service) check_service "$f" "$b" "${f%.service}.timer" ;;
      *.timer) check_timer "$f" ;;
    esac
  done <<<"$installed"
fi
if [ "$ROLE" = "$DEFAULT_ROLE" ] && [ "$verbatim" -eq 0 ]; then
  echo "lint-systemd-hardening: FAIL — no unit installed from $DEPLOY_DIR was found; the scan is broken"
  exit 2
fi

# Both copies of the API unit run as their own uid (a shared uid reads the ingest
# daemons' /proc/<pid>/environ and root), see no other process, hide these paths
# and grant no write path. Its own env file is hidden too: systemd reads it before
# the sandbox, so only a same-uid child (wabt on uploaded WASM) could reach it.
# Not pinned here: the Postgres, ClickHouse and Redis credentials the API holds
# stay shared-strength until hosts set least-privilege ones.
API_HIDDEN="/run/lock /etc/default/stellarindex /etc/default/stellarindex-api /etc/default/galexie /etc/default/galexie-backfill /etc/default/stellarindex-ops /etc/clickhouse-client"
api_user_default=$(sed -nE 's/^stellarindex_api_os_user:[[:space:]]*"?([^"[:space:]]*)"?.*/\1/p' "$ROLE/defaults/main.yml" 2>/dev/null)
daemon_users=" "
for d in stellarindex-indexer stellarindex-aggregator; do
  for g in "$DIR/$d.service.j2" "$DEPLOY_DIR/$d.service"; do
    [ -f "$g" ] && daemon_users+="$(value_of "$g" User) "
  done
done
for f in "$DIR/stellarindex-api.service.j2" "$DEPLOY_DIR/stellarindex-api.service"; do
  [ -f "$f" ] || continue
  u=$(value_of "$f" User)
  u=${u//"{{ stellarindex_api_os_user }}"/$api_user_default}
  case "$u" in
    "" | root | *"{{"* | *" "*)
      echo "  API-SHARED-UID: $f User='$u' is not a resolvable dedicated user"
      fail=$((fail + 1)) ;;
  esac
  case "$daemon_users" in *" $u "*)
    echo "  API-SHARED-UID: $f runs as '$u', the indexer's/aggregator's user"
    fail=$((fail + 1)) ;;
  esac
  if [ -n "$(value_of "$f" SupplementaryGroups)" ]; then
    echo "  API-SHARED-UID: $f sets SupplementaryGroups=; the API needs no other group's files"
    fail=$((fail + 1))
  fi
  if grep -qE '^[[:space:]]*EnvironmentFile[[:space:]]*=[[:space:]]*-?/etc/default/stellarindex[[:space:]]*$' "$f"; then
    echo "  API-SHARED-ENV: $f reads /etc/default/stellarindex; it carries the ingest daemons' secrets"
    fail=$((fail + 1))
  fi
  if [ "$(value_of "$f" ProtectProc)" != invisible ]; then
    echo "  API-PROC: $f does not set ProtectProc=invisible"
    fail=$((fail + 1))
  fi
  if [ -n "$(value_of "$f" ReadWritePaths)" ]; then
    echo "  API-WRITABLE: $f sets ReadWritePaths=; the API writes only its private /tmp"
    fail=$((fail + 1))
  fi
  hidden=" $(grep -E '^[[:space:]]*InaccessiblePaths[[:space:]]*=' "$f" | sed -E 's/^[^=]*=//; s/(^|[[:space:]])-/\1/g' | tr '\n' ' ') "
  for p in $API_HIDDEN; do
    case "$hidden" in *" $p "*) ;; *)
      echo "  API-EXPOSED: $f does not list $p in InaccessiblePaths="
      fail=$((fail + 1)) ;;
    esac
  done
  for d in SystemCallFilter TasksMax MemoryMax; do
    [ -n "$(value_of "$f" "$d")" ] && continue
    echo "  API-UNBOUNDED: $f does not set $d (it runs wabt on uploaded WASM)"
    fail=$((fail + 1))
  done
done

# The API's env file carries only the names the API reads.
API_ENV="$ROLE/templates/stellarindex-api.env.j2"
API_ENV_ALLOW=" STELLARINDEX_POSTGRES_DSN STELLARINDEX_REDIS_PASSWORD STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD STELLARINDEX_CLICKHOUSE_LIVE_USER STELLARINDEX_CLICKHOUSE_LIVE_PASSWORD MASSIVE_API_KEY OPENEXCHANGERATES_APP_ID STELLARINDEX_RESEND_API_KEY STELLARINDEX_DASHBOARD_CODE_SECRET STELLARINDEX_WEBHOOK_SEAL_KEY "
if [ -f "$API_ENV" ]; then
  names=$(sed -nE 's/^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=.*/\1/p' "$API_ENV" | sort -u)
  if [ -z "$names" ]; then
    echo "  API-ENV: $API_ENV defines no variable; the scan is broken"
    fail=$((fail + 1))
  fi
  for n in $names; do
    case "$n" in *S3* | *AWS* | *COINGECKO* | *TIINGO* | *EXCHANGERATESAPI* | *CLICKHOUSE_OPS*)
      echo "  API-ENV-FORBIDDEN: $API_ENV sets $n, a credential the API never uses"
      fail=$((fail + 1))
      continue ;;
    esac
    case "$API_ENV_ALLOW" in *" $n "*) ;; *)
      echo "  API-ENV-UNLISTED: $API_ENV sets $n, outside the API allow-list in $0"
      fail=$((fail + 1)) ;;
    esac
  done
elif [ -f "$DIR/stellarindex-api.service.j2" ]; then
  echo "  API-ENV: $API_ENV is missing but the role installs the API unit"
  fail=$((fail + 1))
fi

if [ "$units" -eq 0 ] || [ "$timers" -eq 0 ]; then
  echo "lint-systemd-hardening: FAIL — found $units service and $timers timer template(s); the scan is broken"
  exit 2
fi
if [ "$fail" -gt 0 ]; then
  echo "lint-systemd-hardening: $fail finding(s) over $units service and $timers timer template(s)"
  exit 1
fi
echo "lint-systemd-hardening: OK — $units service and $timers timer template(s), every gap declared in $EXCEPTIONS"
