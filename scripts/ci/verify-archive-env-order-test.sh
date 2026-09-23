#!/usr/bin/env bash
# verify-archive-env-order-test.sh — EnvironmentFile= must come AFTER the
# Environment=VERIFY_ARCHIVE_* defaults in the verify-archive-tier-{a,b}
# systemd unit templates (SL08).
#
# systemd evaluates Environment=/EnvironmentFile= directives in FILE
# ORDER: whichever sets a key LAST wins. Both templates carry a comment
# claiming "operators override via /etc/default/stellarindex-ops", but
# the EnvironmentFile= used to be declared before the Environment=
# defaults, so every VERIFY_ARCHIVE_* key in the operator's env file was
# silently clobbered back to the template's hardcoded default on every
# run.
#
# Run: bash scripts/ci/verify-archive-env-order-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

TEMPLATES=(
  "configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-a.service.j2"
  "configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-b.service.j2"
)

for f in "${TEMPLATES[@]}"; do
  if [[ ! -r "$f" ]]; then
    bad "missing template $f"
    continue
  fi

  envfile_lines="$(grep -n '^EnvironmentFile=' "$f")"
  envfile_line="${envfile_lines%%$'\n'*}"
  envfile_line="${envfile_line%%:*}"
  default_lines="$(grep -n '^Environment=VERIFY_ARCHIVE_' "$f")"
  last_default_line="${default_lines##*$'\n'}"
  last_default_line="${last_default_line%%:*}"

  if [[ -z "$envfile_line" ]]; then
    bad "$f: no EnvironmentFile= directive found"
  elif [[ -z "$last_default_line" ]]; then
    bad "$f: no Environment=VERIFY_ARCHIVE_* default found"
  elif (( envfile_line > last_default_line )); then
    ok "$f: EnvironmentFile= (line $envfile_line) comes after the last VERIFY_ARCHIVE_* default (line $last_default_line)"
  else
    bad "$f: EnvironmentFile= (line $envfile_line) precedes VERIFY_ARCHIVE_* defaults (last at line $last_default_line) — operator overrides in /etc/default/stellarindex-ops are silently clobbered"
  fi
done

echo "---"
echo "pass=$pass fail=$fail"
[[ "$fail" -eq 0 ]]
