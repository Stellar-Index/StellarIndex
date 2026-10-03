#!/usr/bin/env bash
# lint-healthcheck-oneshot-timeout — every Type=oneshot unit in
# configs/healthchecks/ must set TimeoutStartSec=, and no Type=oneshot unit
# in the repo may set RuntimeMaxSec=.
#
# THE BUG CLASS (T592). These units are re-armed by a .timer's
# OnUnitActiveSec, and systemd will not start a new instance while the
# previous one is still running. A oneshot ExecStart with no start
# timeout can hang forever on a wedged probe/socket, and every future
# scheduled run is silently swallowed — the check goes dark with no error
# anywhere. For Type=oneshot the start job IS the whole run, so
# TimeoutStartSec is the bound. systemd ignores RuntimeMaxSec for oneshot
# ("has no effect in combination with Type=oneshot") and logs that on every
# start, so carrying it is journal noise that reads as a second bound.
#
# Usage: lint-healthcheck-oneshot-timeout.sh [HEALTHCHECK_DIR [UNIT_DIR...]]
# With no arguments the RuntimeMaxSec rule also covers deploy/systemd and
# the ansible role templates.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
hc_dir="${1:-configs/healthchecks}"
if [ "$#" -gt 0 ]; then
  shift
  unit_dirs=("$@")
else
  unit_dirs=(deploy/systemd configs/ansible/roles)
fi

checked=0
fail=0
# scan <dir> <require-timeout:0|1> [find depth args...]
scan() {
  local dir="$1" require="$2" f found=0
  shift 2
  while IFS= read -r f; do
    [ -f "$f" ] || continue
    found=$((found + 1))
    grep -q '^Type=oneshot' "$f" || continue
    if [ "$require" -eq 1 ] && ! grep -q '^TimeoutStartSec=' "$f"; then
      printf 'lint-healthcheck-oneshot-timeout: %s is Type=oneshot but missing TimeoutStartSec=\n' "$f"
      fail=$((fail + 1))
    fi
    if grep -q '^RuntimeMaxSec=' "$f"; then
      printf 'lint-healthcheck-oneshot-timeout: %s is Type=oneshot and sets RuntimeMaxSec=, which systemd ignores; bound it with TimeoutStartSec=\n' "$f"
      fail=$((fail + 1))
    fi
  done < <(find "$dir" "$@" -type f -name '*.service*' 2>/dev/null | sort)
  if [ "$found" -eq 0 ]; then
    echo "lint-healthcheck-oneshot-timeout: FAIL — no unit files found under $dir; the gate would be vacuous" >&2
    fail=$((fail + 1))
  fi
  checked=$((checked + found))
}

scan "$hc_dir" 1 -maxdepth 1
for d in ${unit_dirs[@]+"${unit_dirs[@]}"}; do
  scan "$d" 0
done

if [ "$fail" -gt 0 ]; then
  echo "lint-healthcheck-oneshot-timeout: FAIL — $fail oneshot timeout problem(s)" >&2
  exit 1
fi
echo "lint-healthcheck-oneshot-timeout: OK — $checked unit file(s) checked"
