#!/usr/bin/env bash
# ansible-hashdb-enable-var-test.sh — pins NS17: the ADR-0016 hashdb
# drift detector's config.go doc string promises operators can "opt in
# per region once proven", but stellarindex.toml.j2 rendered no
# [hashdb] section at all — there was no ansible lever to turn it on
# anywhere, on any region, ever. This pins that the rendered config
# carries a [hashdb] section whose `enabled` line is templated off a
# per-region override var (not a bare literal), so an inventory
# group_vars/host_vars override is all a region needs.
#
# Structural only (grep over the real file) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TEMPLATE="configs/ansible/roles/archival-node/templates/stellarindex.toml.j2"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$TEMPLATE" ]; then
  echo "ansible-hashdb-enable-var-test: FAIL — $TEMPLATE not found" >&2
  exit 1
fi

if grep -qE '^\[hashdb\]\s*$' "$TEMPLATE"; then
  ok "$TEMPLATE: [hashdb] section present"
else
  bad "$TEMPLATE: no [hashdb] section — hashdb has no ansible lever at all"
fi

if grep -qE '^enabled\s*=\s*\{\{ .*stellarindex_hashdb_enabled' "$TEMPLATE"; then
  ok "$TEMPLATE: [hashdb] enabled derives from stellarindex_hashdb_enabled"
else
  bad "$TEMPLATE: [hashdb] enabled does not reference stellarindex_hashdb_enabled — a region cannot opt in via inventory"
fi

echo "ansible-hashdb-enable-var-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
