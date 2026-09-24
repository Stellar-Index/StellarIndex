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

# patroni_postgres_password is used for both the bootstrap admin user and
# the postgresql superuser block; isolate the admin one by context so a
# still-quoted superuser line can't mask an unquoted admin line.
admin_line="$(grep -A1 '^\s*admin:' "$TPL" | grep 'password:')"
if grep -Eq "'\{\{[^}]*patroni_postgres_password[^}]*\}\}'" <<<"$admin_line"; then
  echo "ok: bootstrap.users.admin.password is single-quoted"
  pass=$((pass + 1))
else
  echo "FAIL: bootstrap.users.admin.password is unquoted: $admin_line" >&2
  fail=$((fail + 1))
fi

echo
echo "lint-patroni-basic-auth-quoting-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
