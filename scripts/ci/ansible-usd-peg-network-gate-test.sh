#!/usr/bin/env bash
# ansible-usd-peg-network-gate-test.sh — the archival-node role prices against PUBNET identities by default (the USD
# peg is mainnet USDC; the fiat pegs, oracle contracts and CEX/FX connectors are pubnet's). A test net that turns
# run_aggregator on must render none of them:
#   1. pubnet, run_aggregator on, role defaults: the [trades] and [pricing_guard] render is byte-equal to
#      internal/config/testdata/ansible-pubnet-pegs.toml (which internal/config loads), and the oracle and
#      connector blocks render;
#   2. testnet, run_aggregator on, peg vars overridden: the override renders, no pubnet peg issuer does, and no
#      [oracle.*] or [external] block renders;
#   3. testnet, run_aggregator on, role defaults: 01-preflight.yml's peg assert fails the play naming the USD var;
#   4. the same with only the USD peg overridden: the assert names the fiat var;
#   5. testnet, run_aggregator off, role defaults: the assert passes (no pricing, nothing rendered from the pegs).
#
# Runs the role's REAL template, defaults and preflight assert task (ansible-playbook -c local).
# Needs ansible-playbook (the ci.yml toolchain-gates job has it).
# Run: bash scripts/ci/ansible-usd-peg-network-gate-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="${ROLE:-$PWD/configs/ansible/roles/archival-node}"   # override for a red proof against a pre-fix copy
GOLDEN="$PWD/internal/config/testdata/ansible-pubnet-pegs.toml"
TASK_NAME="Verify a test net does not price against a pubnet USD or fiat peg"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if ! command -v ansible-playbook >/dev/null; then
  echo "ansible-usd-peg-network-gate-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi
for f in "$ROLE/templates/stellarindex.toml.j2" "$ROLE/defaults/main.yml" "$ROLE/tasks/01-preflight.yml" "$GOLDEN"; do
  if [ ! -f "$f" ]; then
    echo "ansible-usd-peg-network-gate-test: FAIL — $f not found" >&2
    exit 1
  fi
done

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
R="$TMP/roles/archival-node"
mkdir -p "$R/tasks" "$R/templates" "$R/defaults"
cp "$ROLE/templates/stellarindex.toml.j2" "$R/templates/"
cp "$ROLE/defaults/main.yml" "$R/defaults/"
# The one preflight task under test: from its name line to the blank line that ends it.
awk -v n="- name: $TASK_NAME" '$0 == n {p=1} p && /^$/ {exit} p' "$ROLE/tasks/01-preflight.yml" > "$R/tasks/peg-assert.yml"
if ! grep -q 'ansible.builtin.assert' "$R/tasks/peg-assert.yml"; then
  echo "ansible-usd-peg-network-gate-test: FAIL — no '$TASK_NAME' assert in $ROLE/tasks/01-preflight.yml" >&2
  exit 1
fi
cat > "$R/tasks/main.yml" <<'YML'
---
- ansible.builtin.import_tasks: peg-assert.yml
- ansible.builtin.template: {src: stellarindex.toml.j2, dest: "{{ peg_test_out }}"}
YML
cat > "$TMP/play.yml" <<'YML'
- hosts: localhost
  connection: local
  gather_facts: false
  roles: [archival-node]
YML
TESTNET_USDC="USDC-GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5"
printf 'stellarindex_usd_pegged_classic_assets: ["%s"]\n' "$TESTNET_USDC" > "$TMP/usd.yml"
printf 'stellarindex_fiat_pegged_classic_assets: {}\n' > "$TMP/fiat.yml"

render() { # <network> <run_aggregator> <out> [extra-vars file...]
  local net="$1" agg="$2" out="$3"; shift 3
  local extra=()
  for f in "$@"; do extra+=(-e "@$f"); done
  ANSIBLE_ROLES_PATH="$TMP/roles" ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
    ansible-playbook -i localhost, "$TMP/play.yml" -e region_id=r1 -e "stellar_network=$net" \
    -e "run_aggregator=$agg" -e "peg_test_out=$out" ${extra[@]+"${extra[@]}"} 2>&1
}
pegs() { awk '/^\[(trades|pricing_guard)\]$/ {p=1} p && /^#/ {p=0} p' "$1"; }
# Every issuer the role's defaults peg to, read from the defaults file rather than re-typed here.
PUBNET_ISSUERS="$(grep -oE '"[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}"' "$ROLE/defaults/main.yml" | sed -E 's/^"[^-]*-//; s/"$//' | sort -u)"
[ -n "$PUBNET_ISSUERS" ] || bad "found no peg issuers in $ROLE/defaults/main.yml"

# 1. pubnet defaults
out="$(render pubnet true "$TMP/pubnet.toml")"; rc=$?
if [ $rc -ne 0 ]; then
  bad "pubnet render with role defaults failed (rc=$rc): $out"
else
  if cmp -s <(pegs "$TMP/pubnet.toml") "$GOLDEN"; then
    ok "pubnet [trades] + [pricing_guard] render byte-equal to $(basename "$GOLDEN")"
  else
    bad "pubnet peg render drifted from $GOLDEN: $(diff <(pegs "$TMP/pubnet.toml") "$GOLDEN")"
  fi
  if grep -q '^\[oracle.reflector\]$' "$TMP/pubnet.toml" && grep -q '^\[external\]$' "$TMP/pubnet.toml" &&
     grep -q '"reflector-dex"' <(grep '^enabled_sources' "$TMP/pubnet.toml"); then
    ok "pubnet still renders the oracle contracts, oracle sources and connectors"
  else
    bad "pubnet lost its oracle contracts, oracle sources or [external] block"
  fi
fi

# 2. testnet with overrides
out="$(render testnet true "$TMP/testnet.toml" "$TMP/usd.yml" "$TMP/fiat.yml")"; rc=$?
if [ $rc -ne 0 ]; then
  bad "testnet render with overridden pegs failed (rc=$rc): $out"
else
  if grep -qx "  \"$TESTNET_USDC\"," <(pegs "$TMP/testnet.toml"); then
    ok "testnet renders the overridden USD peg"
  else
    bad "testnet USD peg override did not render: $(pegs "$TMP/testnet.toml")"
  fi
  leaked=""
  for i in $PUBNET_ISSUERS; do grep -q "$i" <(pegs "$TMP/testnet.toml") && leaked="$leaked $i"; done
  if [ -z "$leaked" ]; then ok "testnet peg render carries no pubnet issuer"; else bad "testnet peg render carries pubnet issuer(s):$leaked"; fi
  if grep -qE '^\[\[?oracle\.' "$TMP/testnet.toml"; then
    bad "testnet with run_aggregator on renders pubnet [oracle.*] contracts"
  else
    ok "testnet with run_aggregator on renders no [oracle.*] block"
  fi
  if grep -qE '^\[external(\.|\])' "$TMP/testnet.toml"; then
    bad "testnet with run_aggregator on enables the CEX/FX connectors (mainnet XLM price)"
  else
    ok "testnet with run_aggregator on renders no [external] connectors"
  fi
  if grep -qE '"(reflector-dex|reflector-cex|reflector-fx|redstone|chainlink-stellar|band)"' <(grep '^enabled_sources' "$TMP/testnet.toml"); then
    bad "testnet enables an oracle source with no oracle contract to read"
  else
    ok "testnet enables no oracle source"
  fi
fi

# 3. testnet defaults: assert fires on the USD var
out="$(render testnet true "$TMP/t3.toml")"; rc=$?
if [ $rc -ne 0 ] && grep -q 'stellarindex_usd_pegged_classic_assets carries pubnet issuer(s) \[USDC-GA5Z' <<<"$out" &&
   [ ! -f "$TMP/t3.toml" ]; then
  ok "testnet + run_aggregator + default pegs fails the play naming stellarindex_usd_pegged_classic_assets"
else
  bad "testnet + run_aggregator + default pegs did not fail on the USD var (rc=$rc): $out"
fi

# 4. only the USD peg overridden: assert fires on the fiat var
out="$(render testnet true "$TMP/t4.toml" "$TMP/usd.yml")"; rc=$?
if [ $rc -ne 0 ] && grep -q 'stellarindex_fiat_pegged_classic_assets carries \[AUDD-' <<<"$out" &&
   grep -q 'stellarindex_usd_pegged_classic_assets carries pubnet issuer(s) \[\]' <<<"$out"; then
  ok "testnet with default fiat pegs fails the play naming stellarindex_fiat_pegged_classic_assets"
else
  bad "testnet with default fiat pegs did not fail on the fiat var (rc=$rc): $out"
fi

# 5. testnet with pricing off: defaults are inert, the assert passes
out="$(render testnet false "$TMP/t5.toml")"; rc=$?
if [ $rc -eq 0 ] && grep -qE '^usd_pegged_classic_assets = \[\] *$' "$TMP/t5.toml"; then
  ok "testnet with run_aggregator off passes the assert and renders no USD peg"
else
  bad "testnet with run_aggregator off failed or rendered a USD peg (rc=$rc): $out"
fi

echo "ansible-usd-peg-network-gate-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
