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

# The internet-facing API shares its uid with the writers, so both copies of
# its unit hide what that uid could otherwise reach and grant no write path.
API_HIDDEN="/run/lock /etc/default/galexie /etc/default/galexie-backfill /etc/default/stellarindex-ops /etc/clickhouse-client"
for f in "$DIR/stellarindex-api.service.j2" "$DEPLOY_DIR/stellarindex-api.service"; do
  [ -f "$f" ] || continue
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

if [ "$units" -eq 0 ] || [ "$timers" -eq 0 ]; then
  echo "lint-systemd-hardening: FAIL — found $units service and $timers timer template(s); the scan is broken"
  exit 2
fi
if [ "$fail" -gt 0 ]; then
  echo "lint-systemd-hardening: $fail finding(s) over $units service and $timers timer template(s)"
  exit 1
fi
echo "lint-systemd-hardening: OK — $units service and $timers timer template(s), every gap declared in $EXCEPTIONS"
