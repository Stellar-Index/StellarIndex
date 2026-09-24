#!/usr/bin/env bash
# ansible-keepalived-healthcheck-test.sh — pins SL16: chk_haproxy used to be
# a bare `pgrep haproxy`, a pure process-existence check with no HTTP or
# backend-pool reachability probe. A wedged-but-resident haproxy (accepting
# no connections, or with every api_pool server DOWN) would still pass, so
# this host would keep MASTER — and the VIP — while traffic blackholes.
# The fix routes the vrrp_script through chk_haproxy.sh, which curls the
# stats CSV and requires at least one UP server in api_pool.
#
# Structural only (grep over the real templates/tasks) — no hosts, no
# ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
KEEPALIVED_TPL="${KEEPALIVED_TPL:-configs/ansible/roles/haproxy/templates/keepalived.conf.j2}"
KEEPALIVED_TASKS="${KEEPALIVED_TASKS:-configs/ansible/roles/haproxy/tasks/04-keepalived-configure.yml}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

for f in "$KEEPALIVED_TPL" "$KEEPALIVED_TASKS"; do
  if [ ! -f "$f" ]; then
    echo "ansible-keepalived-healthcheck-test: FAIL — $f not found" >&2
    exit 1
  fi
done

vrrp_block="$(awk '/vrrp_script chk_haproxy/{f=1} f{print} f&&/^}/{exit}' "$KEEPALIVED_TPL")"

if grep -qE '^\s*script\s+"/usr/bin/pgrep\s+haproxy"' <<<"$vrrp_block"; then
  bad "chk_haproxy still runs a bare pgrep — process-existence only, no reachability probe"
else
  ok "chk_haproxy no longer runs a bare pgrep"
fi

if grep -qE '^\s*script\s+"/usr/local/bin/chk_haproxy\.sh"' <<<"$vrrp_block"; then
  ok "chk_haproxy invokes the reachability-probe script"
else
  bad "chk_haproxy does not invoke /usr/local/bin/chk_haproxy.sh"
fi

# The deployed probe script must actually query the stats endpoint and gate
# on a real backend server being UP, not just curl succeeding.
task_body="$(sed -n '/dest: \/usr\/local\/bin\/chk_haproxy\.sh/,/^- name:/p' "$KEEPALIVED_TASKS")"

if grep -q 'curl' <<<"$task_body" && grep -q ';csv' <<<"$task_body"; then
  ok "probe script queries the haproxy stats CSV endpoint"
else
  bad "probe script does not curl the stats CSV endpoint"
fi

if grep -q 'api_pool' <<<"$task_body" && grep -q '"UP"' <<<"$task_body"; then
  ok "probe script gates on an UP server in api_pool"
else
  bad "probe script does not check for an UP server in the backend pool"
fi

echo "ansible-keepalived-healthcheck-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
