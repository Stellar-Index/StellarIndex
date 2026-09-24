#!/usr/bin/env bash
# ansible-caddy-key-pipefail-test.sh — pins Q246: the Caddy signing-key
# task in 19-caddy.yml runs `curl ... | gpg --dearmor ... -o
# <keyring path>` guarded by `creates: <that same keyring path>`. Without
# `set -o pipefail` the shell body's exit status is gpg's alone, so a
# failing/truncated curl inside the pipe does not fail the task — gpg can
# still write a corrupt keyring, and the next run's `creates:` guard then
# skips re-fetching it forever. Structural only (grep over the real task
# file) — no hosts, no network.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="${TASKS:-configs/ansible/roles/archival-node/tasks/19-caddy.yml}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$TASKS" ]; then
  echo "ansible-caddy-key-pipefail-test: FAIL — $TASKS not found" >&2
  exit 1
fi

# task_block <file> <pattern> — the task from its `- name:` line up to
# (not including) the next top-level `- name:`.
task_block() {
  awk -v pat="$2" '
    /^- name:/ { if (hit) { print buf; exit } buf = ""; hit = 0 }
    { buf = buf $0 "\n"; t = $0; sub(/^[ \t]+/, "", t); sub(/[ \t]+$/, "", t); if (t == pat) hit = 1 }
    END { if (hit) print buf }' "$1"
}

blk="$(task_block "$TASKS" "- name: Install Caddy signing key (dearmored, official path)")"
if [ -z "$blk" ]; then
  bad "task 'Install Caddy signing key (dearmored, official path)' not found — test out of date"
else
  ok "found the Caddy signing-key task"

  if ! grep -qE '\|[[:space:]]*gpg' <<<"$blk"; then
    bad "task no longer pipes into gpg — test out of date"
  else
    ok "task still pipes curl into gpg"
  fi

  if grep -qE '^[[:space:]]*set[[:space:]]+.*-o[[:space:]]+pipefail|^[[:space:]]*set[[:space:]]+-[a-zA-Z]*o[a-zA-Z]*[[:space:]]+pipefail' <<<"$blk"; then
    ok "shell body sets pipefail — a failing curl fails the task, not just a failing gpg"
  else
    bad "shell body has no 'set -o pipefail' — a failing curl inside 'curl | gpg' is invisible; gpg's own exit code is all that's checked, and it can still write a keyring from truncated input"
  fi

  if grep -qE '^\s*executable:\s*/bin/bash\s*$' <<<"$blk"; then
    ok "executable: /bin/bash is set (pipefail is a bashism; dash rejects it)"
  else
    bad "no 'executable: /bin/bash' — dash (ansible.builtin.shell's default) rejects 'set -o pipefail'"
  fi
fi

echo "ansible-caddy-key-pipefail-test: $pass ok, $fail failed"
[ "$fail" -eq 0 ]
