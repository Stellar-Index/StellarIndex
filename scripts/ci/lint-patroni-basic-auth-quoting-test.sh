#!/usr/bin/env bash
# lint-patroni-basic-auth-quoting-test.sh — SL18: the patroni REST API
# authentication block (username/password) rendered its jinja vars
# unquoted while the replicator/superuser passwords in the same
# template are single-quoted. An unquoted scalar containing a `:`,
# `#`, or leading special char is either a YAML parse error or a
# silently different value than the credential the operator set.
#
# Asserts every credential-shaped value in patroni.yml.j2 is
# single-quoted, the convention the file already uses for the
# replicator/superuser passwords.
#
# Run: bash scripts/ci/lint-patroni-basic-auth-quoting-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TPL="configs/ansible/roles/patroni/templates/patroni.yml.j2"

pass=0
fail=0

check_quoted() {
  local label="$1" pattern="$2"
  local line
  line="$(grep -n "$pattern" "$TPL")"
  if [ -z "$line" ]; then
    echo "FAIL: $label — pattern not found in $TPL" >&2
    fail=$((fail + 1))
    return
  fi
  if grep -Eq "$pattern" <<<"$line" && grep -Eq "'\{\{[^}]*\}\}'" <<<"$line"; then
    echo "ok: $label is single-quoted"
    pass=$((pass + 1))
  else
    echo "FAIL: $label is unquoted: $line" >&2
    fail=$((fail + 1))
  fi
}

check_quoted "restapi.authentication.username" "username: .*patroni_rest_basic_auth_user"
check_quoted "restapi.authentication.password" "password: .*patroni_rest_basic_auth_password"

check_quoted "postgresql.authentication.replication.password" "password: .*patroni_replicator_password"
check_quoted "postgresql.authentication.superuser.password" "password: .*patroni_postgres_password"

# Every templated username/password line, so a credential added later is
# covered without a new check_quoted line.
while IFS= read -r line; do
  [ -z "$line" ] && continue
  if grep -Eq ":[[:space:]]*'\{\{[^}]*\}\}'[[:space:]]*$" <<<"$line"; then
    pass=$((pass + 1))
  else
    echo "FAIL: templated credential is not single-quoted: $line" >&2
    fail=$((fail + 1))
  fi
done < <(grep -nE '^[[:space:]]*(username|password):.*\{\{' "$TPL")

echo
echo "lint-patroni-basic-auth-quoting-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
