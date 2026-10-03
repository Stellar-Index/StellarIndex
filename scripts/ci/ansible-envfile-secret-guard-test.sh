#!/usr/bin/env bash
# ansible-envfile-secret-guard-test.sh — the archival-node preflight refuses
# shell-metacharacter secrets only for names listed in env_file_secrets, so
# every vault secret rendered unquoted into an /etc/default file must be
# listed there. Structural only (grep over the task file), no hosts.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
PREFLIGHT="${PREFLIGHT:-configs/ansible/roles/archival-node/tasks/01-preflight.yml}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$PREFLIGHT" ]; then
  echo "ansible-envfile-secret-guard-test: FAIL — $PREFLIGHT not found" >&2
  exit 1
fi

for name in minio_root_user minio_root_password; do
  if grep -qE "^[[:space:]]+${name}:[[:space:]]*\"\{\{[[:space:]]*${name}[[:space:]]" "$PREFLIGHT"; then
    ok "$name is covered by the metacharacter guard"
  else
    bad "$name is rendered into /etc/default/node-healthcheck and minio but is not in env_file_secrets"
  fi
done

echo "ansible-envfile-secret-guard-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
