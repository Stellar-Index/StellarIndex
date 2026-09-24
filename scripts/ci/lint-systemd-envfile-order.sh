#!/usr/bin/env bash
# lint-systemd-envfile-order — in a unit's [Service] section, every
# Environment= default must appear BEFORE its EnvironmentFile=
# directives.
#
# THE BUG CLASS (SL07). systemd.exec(5): Environment= and
# EnvironmentFile= are applied in the order they appear in the unit
# file, and a later directive wins over an earlier one for the same
# key. A default declared via Environment= AFTER an EnvironmentFile=
# therefore always overrides whatever the operator set in the file —
# silently, since systemd emits no warning for a "successful" merge.
#
# Usage: lint-systemd-envfile-order.sh [ROOT] [FILE...]
# FILE defaults to the one unit SL07 was raised against; the
# self-test passes an explicit fixture path.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROOT="${1:-$PWD}"
shift || true
if [ "$#" -gt 0 ]; then
  FILES=("$@")
else
  FILES=("configs/ansible/roles/archival-node/templates/systemd/supply-snapshot.service.j2")
fi

fail=0
checked=0
for rel in "${FILES[@]}"; do
  path="$ROOT/$rel"
  if [ ! -f "$path" ]; then
    echo "lint-systemd-envfile-order: FAIL — $rel not found under $ROOT" >&2
    fail=1
    continue
  fi
  checked=$((checked + 1))
  # last line number of Environment= and first line number of
  # EnvironmentFile=, within [Service] (this repo never splits
  # [Service] across a unit file).
  env_lines=$(grep -n '^Environment=' "$path" | cut -d: -f1)
  envfile_lines=$(grep -n '^EnvironmentFile=' "$path" | cut -d: -f1)
  last_env=$(printf '%s\n' "$env_lines" | tail -1)
  first_envfile=$(printf '%s\n' "$envfile_lines" | sed -n '1p')
  if [ -z "$last_env" ] || [ -z "$first_envfile" ]; then
    continue
  fi
  if [ "$last_env" -gt "$first_envfile" ]; then
    echo "lint-systemd-envfile-order: $rel: Environment= at line $last_env" \
         "comes after EnvironmentFile= at line $first_envfile — the" \
         "hardcoded default always overrides the operator's file" >&2
    fail=1
  fi
done

if [ "$checked" -eq 0 ]; then
  echo "lint-systemd-envfile-order: FAIL — no unit checked; the gate would be vacuous" >&2
  exit 1
fi
if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "lint-systemd-envfile-order: OK — $checked unit(s) declare Environment=" \
     "defaults before EnvironmentFile="
