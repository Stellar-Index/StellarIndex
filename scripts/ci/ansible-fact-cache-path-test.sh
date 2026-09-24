#!/usr/bin/env bash
# ansible-fact-cache-path-test.sh — pins Q258: configs/ansible/ansible.cfg's
# fact_caching_connection used to point at /tmp/ansible_fact_cache, a fixed,
# guessable path under the world-writable /tmp namespace with no
# ownership/mode hardening anywhere in the file. Any other local user on a
# shared ops box could pre-create or write that directory before an
# archival-node.yml run and poison the cached ansible_distribution_release
# that playbook's apt-repo tasks key off, within fact_caching_timeout
# (3600s) of a prior gather. The fix moves the cache under the operator's
# own home directory, matching vault_password_file and collections_paths
# already in this file.
#
# Structural only (grep over the real config file) — no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CFG="${CFG:-configs/ansible/ansible.cfg}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$CFG" ]; then
  echo "ansible-fact-cache-path-test: FAIL — $CFG not found" >&2
  exit 1
fi

line="$(grep -E '^\s*fact_caching_connection\s*=' "$CFG")"

if [ -z "$line" ]; then
  bad "no fact_caching_connection setting found in $CFG"
elif grep -qE '=\s*/tmp/' <<<"$line"; then
  bad "fact_caching_connection points at a shared /tmp path ($line) — guessable, world-writable, no ownership/mode hardening"
else
  ok "fact_caching_connection does not use a shared /tmp path ($line)"
fi

if [ -n "$line" ] && grep -qE '=\s*~/\.ansible/' <<<"$line"; then
  ok "fact_caching_connection lives under the operator's own ~/.ansible, matching vault_password_file/collections_paths"
else
  bad "fact_caching_connection is not scoped under ~/.ansible ($line)"
fi

echo "ansible-fact-cache-path-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
