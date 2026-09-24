#!/usr/bin/env bash
# ansible-haproxy-firewall-ipv6-test.sh — pins SL10: 06-firewall.yml
# built its nftables allow-sets with `if ':' not in cidr`, so any IPv6
# CIDR in haproxy_internal_cidrs / haproxy_allowed_ssh_cidrs was silently
# dropped out of the rendered ruleset — there was no ipv6_addr set and
# no `ip6 saddr` rule to enforce it, only the ipv4_addr sets. An
# operator adding an IPv6 admin range to the SSH allow-list, or an
# IPv6 internal CIDR for VRRP peers, would see it accepted by ansible
# but silently unenforced.
#
# Structural only (grep over the real task file) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="${TASKS:-configs/ansible/roles/haproxy/tasks/06-firewall.yml}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$TASKS" ]; then
  echo "ansible-haproxy-firewall-ipv6-test: FAIL — $TASKS not found" >&2
  exit 1
fi

body="$(grep -v '^\s*#' "$TASKS")"

check_set() {  # <set-name> <var> <sign>
  local set_name="$1" var="$2" sign="$3"
  if grep -qE "set ${set_name} \{" <<<"$body" \
    && awk -v s="set ${set_name} {" 'f && /type ipv6_addr/ {found=1} $0 ~ s {f=1} f && /^\s*\}/ && NR>1 {f=0} END{exit !found}' <<<"$body"; then
    ok "$set_name: declared as type ipv6_addr"
  else
    bad "$set_name: missing, or not declared as type ipv6_addr"
  fi
  if grep -qE "for cidr in ${var} if '${sign}' in cidr" <<<"$body"; then
    ok "$set_name: populated from ${var} filtered on IPv6 ('${sign}' in cidr)"
  else
    bad "$set_name: not populated from ${var} filtered on IPv6 ('${sign}' in cidr)"
  fi
}

check_set "internal_v6" "haproxy_internal_cidrs" ":"
check_set "ssh_allowed_v6" "haproxy_allowed_ssh_cidrs" ":"

if grep -qE 'tcp dport 22 ip6 saddr @ssh_allowed_v6 .*accept' <<<"$body"; then
  ok "chain input: ssh accept rule matches ip6 saddr @ssh_allowed_v6"
else
  bad "chain input: no ssh accept rule matching ip6 saddr @ssh_allowed_v6 — IPv6 admins in the allow-list are silently unenforced"
fi

if grep -qE 'ip6 nexthdr vrrp ip6 saddr @internal_v6 accept' <<<"$body"; then
  ok "chain input: vrrp accept rule matches ip6 saddr @internal_v6"
else
  bad "chain input: no vrrp accept rule matching ip6 saddr @internal_v6 — IPv6 CIDRs are silently unenforced"
fi

echo "ansible-haproxy-firewall-ipv6-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
