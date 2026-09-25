#!/usr/bin/env bash
# ansible-divergence-chainlink-secret-test.sh — pins RSEC-S2: the archival-node
# role's 14-stellarindex-services.yml templates /etc/stellarindex.toml with
# owner: root, group: root, mode: "0644" (world-readable — intentional, the
# on-disk config is supposed to carry everything except secrets). The
# [divergence.chainlink] block in stellarindex.toml.j2 used to render
# `rpc_url = "{{ vault_chainlink_rpc_url | default(...) }}"` directly, so the
# keyed Alchemy endpoint (API key embedded in the URL path) landed in that
# world-readable file. [external.chainlink]'s rpc_url correctly stays out of
# the template and comes from CHAINLINK_RPC_URL in /etc/default/stellarindex
# (mode 0640 root:stellarindex) instead — internal/config/load.go's
# ApplyEnvOverrides feeds both External.Chainlink.RPCUrl and
# Divergence.Chainlink.RPCURL from that one env var, so the TOML value was
# never needed for a real deploy.
#
# Behavioural: a real `ansible-playbook -c local` render of the actual
# template (role defaults + a fake vault_chainlink_rpc_url) must not contain
# the secret value anywhere in the output.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="$PWD/configs/ansible/roles/archival-node"
TEMPLATE="${TEMPLATE:-$ROLE/templates/stellarindex.toml.j2}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if ! command -v ansible-playbook >/dev/null; then
  echo "ansible-divergence-chainlink-secret-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi
if [ ! -f "$TEMPLATE" ]; then
  echo "ansible-divergence-chainlink-secret-test: FAIL — $TEMPLATE not found" >&2
  exit 1
fi

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
SECRET="https://eth-mainnet.g.alchemy.com/v2/FAKESECRETTOKEN1234"  # gitleaks:allow — fixture value, not a real key

cat > "$TMP/fixture.yml" <<YML
---
- hosts: localhost
  connection: local
  gather_facts: false
  vars_files:
    - $ROLE/defaults/main.yml
  vars:
    region_id: r1
    vault_chainlink_rpc_url: "$SECRET"
  tasks:
    - name: render
      ansible.builtin.template:
        src: "$TEMPLATE"
        dest: "$TMP/out.toml"
YML

out="$(ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
       ansible-playbook -i localhost, "$TMP/fixture.yml" 2>&1)"
rc=$?
if [ $rc -ne 0 ]; then
  bad "template render failed (rc=$rc): $out"
elif grep -q "$SECRET" "$TMP/out.toml"; then
  bad "rendered /etc/stellarindex.toml (mode 0644, world-readable) contains the vault_chainlink_rpc_url secret"
else
  ok "rendered /etc/stellarindex.toml does not contain the vault_chainlink_rpc_url secret"
fi

# The task that writes this rendered file must stay world-readable-by-design
# (owner/group root, mode 0644) — the fix is keeping the secret OUT of the
# template, not locking the file down, which would contradict its own
# "carries everything except secrets" doc comment.
TASKS="$ROLE/tasks/14-stellarindex-services.yml"
blk="$(awk '/^- name: Template \/etc\/stellarindex.toml/{f=1} f{print} f&&/notify:/{exit}' "$TASKS")"
if grep -q 'mode: "0644"' <<<"$blk" && grep -q 'group: root' <<<"$blk"; then
  ok "14-stellarindex-services.yml: /etc/stellarindex.toml task still mode 0644 group root (secret removed, not the file locked down)"
else
  bad "14-stellarindex-services.yml: /etc/stellarindex.toml task owner/group/mode changed unexpectedly"
fi

echo "ansible-divergence-chainlink-secret-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
