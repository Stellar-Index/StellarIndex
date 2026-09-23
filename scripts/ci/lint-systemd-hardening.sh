#!/usr/bin/env bash
# lint-systemd-hardening.sh — every unit the archival-node role renders
# names the account it runs as and carries the baseline sandbox, and every
# wall-clock timer pins its zone.
#
# The role's units are unusually consistent: nearly every one sets User=,
# NoNewPrivileges, PrivateTmp and the Protect-Kernel/CGroup trio, and
# every fixed-hour OnCalendar ends in UTC. The outliers issue #617 found
# (pgbackrest-backup with no sandbox at all, node-healthcheck running as
# root by omission, a timer firing at 02:00 host-local) all had one thing
# in common: nothing noticed a directive that was simply absent.
#
# Rules, over $ROLE/templates/systemd:
#   *.service.j2  — User= or DynamicUser=yes (running as root must be
#                   written down, not inherited), and each of
#                   NoNewPrivileges PrivateTmp ProtectKernelTunables
#                   ProtectKernelModules ProtectControlGroups set and not
#                   disabled. DynamicUser=yes implies the first two.
#   *.timer.j2    — an OnCalendar= with a fixed hour ends in " UTC".
#
# A unit that genuinely cannot take a directive is declared in
# $ROLE/templates/systemd/HARDENING-EXCEPTIONS as <unit>:<directive> with
# the reason. A declared exception the unit no longer needs fails too, so
# the list only ever shrinks by deleting lines.
#
# Run: bash scripts/ci/lint-systemd-hardening.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

ROLE="${SYSTEMD_HARDENING_ROLE:-configs/ansible/roles/archival-node}"
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

for f in "$DIR"/*.service.j2; do
  [ -f "$f" ] || continue
  unit=$(basename "$f" .j2)
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
done

for f in "$DIR"/*.timer.j2; do
  [ -f "$f" ] || continue
  timers=$((timers + 1))
  while IFS= read -r cal; do
    case "$cal" in *'{{'*) continue ;; esac
    grep -qE '(^|[[:space:]])[0-9]{1,2}(,[0-9]{1,2})*:' <<<"$cal" || continue
    grep -qE '[[:space:]]UTC$' <<<"$cal" && continue
    echo "  UNPINNED: $f OnCalendar=$cal fixes an hour but not a zone — append UTC"
    fail=$((fail + 1))
  done < <(grep -E '^[[:space:]]*OnCalendar[[:space:]]*=' "$f" | sed -E 's/^[^=]*=[[:space:]]*//; s/[[:space:]]+$//')
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
