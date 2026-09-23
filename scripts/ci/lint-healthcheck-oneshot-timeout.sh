#!/usr/bin/env bash
# lint-healthcheck-oneshot-timeout — every Type=oneshot unit in
# configs/healthchecks/ must set TimeoutStartSec= and RuntimeMaxSec=.
#
# THE BUG CLASS (T592). These units are re-armed by a .timer's
# OnUnitActiveSec, and systemd will not start a new instance while the
# previous one is still running. A oneshot ExecStart with no start
# timeout and no runtime cap can hang forever on a wedged probe/socket,
# and every future scheduled run is silently swallowed — the check goes
# dark with no error anywhere. TimeoutStartSec bounds the start job;
# RuntimeMaxSec bounds the unit's total lifetime regardless of type.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
dir="${1:-configs/healthchecks}"

checked=0
fail=0
while IFS= read -r f; do
  [ -f "$f" ] || continue
  checked=$((checked + 1))
  grep -q '^Type=oneshot' "$f" || continue
  missing=""
  grep -q '^TimeoutStartSec=' "$f" || missing="${missing}TimeoutStartSec= "
  grep -q '^RuntimeMaxSec=' "$f" || missing="${missing}RuntimeMaxSec= "
  if [ -n "$missing" ]; then
    printf 'lint-healthcheck-oneshot-timeout: %s is Type=oneshot but missing %s\n' "$f" "$missing"
    fail=$((fail + 1))
  fi
done < <(find "$dir" -maxdepth 1 -type f -name '*.service*' 2>/dev/null | sort)

if [ "$checked" -eq 0 ]; then
  echo "lint-healthcheck-oneshot-timeout: FAIL — no unit files found under $dir; the gate would be vacuous" >&2
  exit 1
fi
if [ "$fail" -gt 0 ]; then
  echo "lint-healthcheck-oneshot-timeout: FAIL — $fail oneshot unit(s) without a start/runtime bound" >&2
  exit 1
fi
echo "lint-healthcheck-oneshot-timeout: OK — $checked unit file(s) checked"
