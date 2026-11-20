#!/usr/bin/env bash
# ansible-keepalived-healthcheck-test.sh — pins that chk_haproxy is not
# a bare `pgrep haproxy`, a pure process-existence check with no HTTP or
# backend-pool reachability probe. A wedged-but-resident haproxy (accepting
# no connections, or with every api_pool server DOWN) would still pass, so
# this host would keep MASTER — and the VIP — while traffic blackholes.
# The vrrp_script routes through chk_haproxy.sh, which curls the
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

vrrp_block() { awk -v n="vrrp_script $1 {" 'index($0, n) == 1 {f=1} f{print} f&&/^}/{exit}' "$KEEPALIVED_TPL"; }
alive_block="$(vrrp_block chk_haproxy_alive)"
pool_block="$(vrrp_block chk_api_pool)"

if grep -qE 'pgrep' <<<"$alive_block$pool_block"; then
  bad "chk_haproxy still runs a bare pgrep — process-existence only, no reachability probe"
else
  ok "chk_haproxy no longer runs a bare pgrep"
fi

if grep -qE '^\s*script\s+"/usr/local/bin/chk_haproxy\.sh alive"' <<<"$alive_block" &&
   grep -qE '^\s*script\s+"/usr/local/bin/chk_haproxy\.sh pool"' <<<"$pool_block"; then
  ok "the alive and pool vrrp_scripts invoke the reachability-probe script"
else
  bad "chk_haproxy_alive / chk_api_pool do not invoke /usr/local/bin/chk_haproxy.sh alive|pool"
fi

# Both LBs probe the same pool: an empty pool must lower priority, not FAULT
# both instances and drop the VIP. Only the alive probe may fault.
if grep -qE '^\s*weight\s+-([1-9]|\{\{ keepalived_pool_weight \| int \}\})' <<<"$pool_block" && ! grep -qE '^\s*weight' <<<"$alive_block"; then
  ok "pool probe lowers priority (negative weight); alive probe faults"
else
  bad "chk_api_pool needs a negative weight and chk_haproxy_alive none"
fi

track="$(awk '/track_script/{f=1} f{print} f&&/}/{exit}' "$KEEPALIVED_TPL")"
if grep -qw chk_haproxy_alive <<<"$track" && grep -qw chk_api_pool <<<"$track"; then
  ok "VI_API tracks both probes"
else
  bad "VI_API does not track chk_haproxy_alive and chk_api_pool"
fi

# A degraded MASTER keeps advertising at its lowered priority, and preempt_delay
# makes the peer accept those adverts, so failover would wait out the delay.
# A flapping pool is damped on the probe's rise (the recovery edge) instead.
instance="$(awk '/^vrrp_instance VI_API/{f=1} f{print} f&&/^}/{exit}' "$KEEPALIVED_TPL")"
defaults="$(dirname "$KEEPALIVED_TPL")/../defaults/main.yml"
recovery="$(sed -n 's/^keepalived_pool_recovery_seconds:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$defaults")"
weight="$(sed -n 's/^keepalived_pool_weight:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$defaults")"
if grep -qE '^\s*state\s+BACKUP\s*$' <<<"$instance" && ! grep -qE '^\s*(preempt_delay|nopreempt)' <<<"$instance" &&
   grep -qE '^\s*rise\s+\{\{ \(keepalived_pool_recovery_seconds \| int\) // 2 \}\}' <<<"$pool_block" &&
   grep -qE '^\s*interval\s+2\s*$' <<<"$pool_block" && [ "${recovery:-0}" -ge 30 ]; then
  ok "VI_API starts BACKUP without preempt_delay; the pool probe needs ${recovery}s healthy to rise"
else
  bad "VI_API needs state BACKUP and no preempt_delay, and chk_api_pool rise from keepalived_pool_recovery_seconds (>= 30)"
fi

# The weight must exceed the LBs' priority gap, or a one-sided pool loss never moves the VIP.
preflight="$(dirname "$KEEPALIVED_TPL")/../tasks/01-preflight.yml"
if [ "${weight:-0}" -gt 0 ] && grep -qF "(_lb_priorities | max) - (_lb_priorities | min) < (keepalived_pool_weight | int)" "$preflight" &&
   grep -qF "keepalived_preempt_delay is not defined" "$preflight"; then
  ok "preflight bounds the priority gap by keepalived_pool_weight (${weight}) and refuses a stale preempt_delay"
else
  bad "preflight must assert the LB priority gap is below keepalived_pool_weight and refuse keepalived_preempt_delay"
fi

# The deployed probe script must actually query the stats endpoint and gate
# on a real backend server being UP, not just curl succeeding.
task_body="$(sed -n '/dest: \/usr\/local\/bin\/chk_haproxy\.sh/,/^- name:/p' "$KEEPALIVED_TASKS")"

if grep -q 'curl' <<<"$task_body" && grep -q ';csv' <<<"$task_body"; then
  ok "probe script queries the haproxy stats CSV endpoint"
else
  bad "probe script does not curl the stats CSV endpoint"
fi

# Run the real probe against stats CSV fixtures ($18 is the server status).
if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  python3 - "$KEEPALIVED_TASKS" "$tmp" <<'PYEOF'
import sys, yaml
t = next(t for t in yaml.safe_load(open(sys.argv[1])) if t.get("copy", {}).get("dest") == "/usr/local/bin/chk_haproxy.sh")
body = t["copy"]["content"].replace("/usr/bin/curl", sys.argv[2] + "/curl").replace("{{ haproxy_stats_listen }}", "127.0.0.1:8404")
open(sys.argv[2] + "/chk.sh", "w").write(body)
PYEOF
  row() { printf 'api_pool,%s' "$1"; for _ in $(seq 3 17); do printf ',0'; done; printf ',%s\n' "$2"; }
  probe() { # $1 = mode, $2 = curl stub body; prints the exit code
    printf '#!/bin/sh\n%s\n' "$2" >"$tmp/curl"; chmod +x "$tmp/curl"
    sh "$tmp/chk.sh" "$1" </dev/null >/dev/null 2>&1; echo $?
  }
  csv() { printf 'cat <<EOF\n%s\nEOF' "$(row BACKEND UP; for s in "$@"; do row "api-$s" "$s"; done)"; }
  expect() { # $1 = wanted exit code, $2 = got, $3 = label
    if [ "$1" = "$2" ]; then ok "$3"; else bad "$3 (exit $2, want $1)"; fi
  }
  expect 0 "$(probe pool "$(csv 'UP')")" "pool: an UP server passes"
  expect 0 "$(probe pool "$(csv 'UP 1/3' 'UP 2/3')")" "pool: 'UP 1/3' (still in rotation) passes"
  expect 1 "$(probe pool "$(csv 'DOWN' 'DOWN 1/2' 'MAINT')")" "pool: no server in rotation fails"
  expect 0 "$(probe alive "$(csv 'DOWN')")" "alive: answers with an empty pool"
  expect 1 "$(probe alive 'exit 7')" "alive: unreachable stats fails"
  expect 1 "$(probe bogus "$(csv 'UP')")" "unknown mode fails closed"
elif [ "${CI:-}" = "true" ]; then
  bad "python3 with yaml is required in CI to run the probe"
else
  echo "  skip — probe run (no python3 with yaml locally)"
fi

echo "ansible-keepalived-healthcheck-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
