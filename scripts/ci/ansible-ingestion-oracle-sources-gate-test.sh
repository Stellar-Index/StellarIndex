#!/usr/bin/env bash
# ansible-ingestion-oracle-sources-gate-test.sh — pins T188: stellarindex.toml.j2
# used to render `[ingestion] enabled_sources` unconditionally from
# stellarindex_enabled_sources while the [oracle.*] blocks a few lines below
# are gated on `run_aggregator | default(true) | bool`. The pubnet default
# list includes the oracle sources (reflector-dex/cex/fx, redstone, band),
# so a host that sets run_aggregator: false without ALSO overriding
# stellarindex_enabled_sources would ship oracle contract addresses to an
# indexer with no aggregator running to consume them. testnet.yml and
# futurenet.yml happen to override the source list by hand (to [sdex]) so
# no host hit this in practice — that is coincidence, not a mechanism.
#
# Behavioural: a real `ansible-playbook -c local` render of the actual
# template with run_aggregator: false and the pubnet default source list
# must NOT include any oracle source in [ingestion] enabled_sources.

set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="$PWD/configs/ansible/roles/archival-node"
TEMPLATE="${TEMPLATE:-$ROLE/templates/stellarindex.toml.j2}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if ! command -v ansible-playbook >/dev/null; then
  echo "ansible-ingestion-oracle-sources-gate-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi
if [ ! -f "$TEMPLATE" ]; then
  echo "ansible-ingestion-oracle-sources-gate-test: FAIL — $TEMPLATE not found" >&2
  exit 1
fi

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
ORACLE_SOURCES="reflector-dex reflector-cex reflector-fx redstone band"

render() { # <run_aggregator bool literal> <out file>
  cat > "$TMP/fixture.yml" <<YML
---
- hosts: localhost
  connection: local
  gather_facts: false
  vars_files:
    - $ROLE/defaults/main.yml
  vars:
    region_id: r1
    run_aggregator: $1
  tasks:
    - name: render
      ansible.builtin.template:
        src: "$TEMPLATE"
        dest: "$2"
YML
  ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
    ansible-playbook -i localhost, "$TMP/fixture.yml" 2>&1
}

out="$(render false "$TMP/off.toml")"
rc=$?
if [ $rc -ne 0 ]; then
  bad "template render (run_aggregator: false) failed (rc=$rc): $out"
else
  line="$(grep '^enabled_sources' "$TMP/off.toml")"
  leaked=""
  for s in $ORACLE_SOURCES; do
    grep -q "\"$s\"" <<<"$line" && leaked="$leaked $s"
  done
  if [ -n "$leaked" ]; then
    bad "run_aggregator: false still renders oracle source(s) in enabled_sources:$leaked -- $line"
  else
    ok "run_aggregator: false excludes all oracle sources from enabled_sources"
  fi
  if grep -q '"sdex"' <<<"$line"; then
    ok "run_aggregator: false still includes a non-oracle source (sdex)"
  else
    bad "run_aggregator: false dropped a non-oracle source (sdex) too"
  fi
fi

out="$(render true "$TMP/on.toml")"
rc=$?
if [ $rc -ne 0 ]; then
  bad "template render (run_aggregator: true) failed (rc=$rc): $out"
else
  line="$(grep '^enabled_sources' "$TMP/on.toml")"
  missing=""
  for s in $ORACLE_SOURCES; do
    grep -q "\"$s\"" <<<"$line" || missing="$missing $s"
  done
  if [ -n "$missing" ]; then
    bad "run_aggregator: true is missing oracle source(s) from enabled_sources:$missing -- $line"
  else
    ok "run_aggregator: true still includes all oracle sources in enabled_sources"
  fi
fi

echo "ansible-ingestion-oracle-sources-gate-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
