#!/usr/bin/env bash
# ansible-galexie-restart-test.sh — pins the archival-node role's restart
# wiring (2026-08-28 audit, deploy-ansible-handlers-7 + -drift-3).
#
# Structural (grep over the real task files):
#   1. None of the five galexie-input render tasks in 07-galexie.yml
#      (wrapper copy, captive cfg, galexie.toml, /etc/default/galexie,
#      unit) notifies `Restart galexie` directly — a comment-only edit
#      used to cost a ~9-minute mainnet cold catchup. The galexie BINARY
#      install/copy tasks may (a new binary is a real restart).
#   2. The bootstrap binary-install task in 14-stellarindex-services.yml
#      notifies a restart for every long-running unit it replaces
#      (indexer, aggregator, api) — it used to notify the indexer only, so
#      the api kept running the old binary from memory.
#
# Behavioural (a real `ansible-playbook -c local` run of
# galexie-effective-checksum.yml against temp files, with a stub
# `Restart galexie` handler that writes a marker):
#   3. comment/blank-line-only edit → handler NOT fired;
#   4. code edit → handler fired;
#   5. input absent before, present after → handler fired.
#
# Needs ansible-playbook (the ci.yml ansible-check job installs it).
# Run: bash scripts/ci/ansible-galexie-restart-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="$PWD/configs/ansible/roles/archival-node"
ROLE_TASKS="${ROLE_TASKS:-$ROLE/tasks}"   # override for the red-proof against a pre-fix copy

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if ! command -v ansible-playbook >/dev/null; then
  echo "ansible-galexie-restart-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi

# ── 1. galexie input render tasks must not notify Restart galexie ─────────
# task_block <file> <pattern> — print the task block (from its `- name:` to
# the next top-level `- name:`) that contains the pattern.
# The pattern must match a whole (trimmed) line, so `dest: /etc/default/galexie`
# does not also match the galexie-backfill env-file task.
task_block() {
  awk -v pat="$2" '
    /^- name:/ { if (hit) { print buf; exit } buf = ""; hit = 0 }
    { buf = buf $0 "\n"; t = $0; sub(/^[ \t]+/, "", t); sub(/[ \t]+$/, "", t); if (t == pat) hit = 1 }
    END { if (hit) print buf }' "$1"
}
G="$ROLE_TASKS/07-galexie.yml"
for pat in "src: galexie-append.sh" "dest: /etc/stellar/captive-core-galexie.cfg" \
           "dest: /etc/galexie/galexie.toml" "dest: /etc/default/galexie" \
           "src: systemd/galexie.service.j2"; do
  blk="$(task_block "$G" "$pat")"
  if [ -z "$blk" ]; then bad "07-galexie.yml: task with '$pat' not found"; continue; fi
  if grep -v '^\s*#' <<<"$blk" | grep -q 'Restart galexie'; then
    bad "07-galexie.yml: task '$pat' notifies Restart galexie directly (comment-only edits restart galexie)"
  else
    ok "07-galexie.yml: task '$pat' does not notify Restart galexie directly"
  fi
done
if grep -q 'galexie-effective-checksum.yml' "$G"; then
  ok "07-galexie.yml imports galexie-effective-checksum.yml"
else
  bad "07-galexie.yml does not import galexie-effective-checksum.yml (no effective-change gate)"
fi

# ── 2. bootstrap install notifies every daemon it replaces ────────────────
S="$ROLE_TASKS/14-stellarindex-services.yml"
blk="$(task_block "$S" "- name: Install stellarindex binaries on r1")"
for h in "Restart stellarindex-indexer" "Restart stellarindex-aggregator" "Restart stellarindex-api"; do
  # accept both `notify: X` (single) and `  - X` (list) forms
  if grep -v '^\s*#' <<<"$blk" | grep -qE -- "(notify:|-) $h\$"; then
    ok "14-stellarindex-services.yml: binary install notifies '$h'"
  else
    bad "14-stellarindex-services.yml: binary install does NOT notify '$h' (unit keeps the old binary in memory)"
  fi
done

# ── 3-5. behavioural: the effective-checksum gate ─────────────────────────
CHK="$ROLE_TASKS/galexie-effective-checksum.yml"
if [ ! -f "$CHK" ]; then
  bad "galexie-effective-checksum.yml missing — cannot exercise the gate"
else
  TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
  cat > "$TMP/fixture.yml" <<YML
---
- hosts: localhost
  connection: local
  gather_facts: false
  vars:
    galexie_effective_inputs:
      - "$TMP/a.sh"
      - "$TMP/b.toml"
  tasks:
    - name: before
      ansible.builtin.include_tasks: "$CHK"
      vars: { galexie_effective_stage: before }
    - name: mutate
      ansible.builtin.command: "bash $TMP/mutate.sh"
      changed_when: false
    - name: after
      ansible.builtin.include_tasks: "$CHK"
      vars: { galexie_effective_stage: after }
  handlers:
    - name: Restart galexie
      ansible.builtin.copy:
        content: fired
        dest: "$TMP/restarted"
YML
  run_case() {  # <desc> <expect: fired|quiet>
    rm -f "$TMP/restarted"
    out="$(cd "$TMP" && ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
           ansible-playbook -i localhost, fixture.yml 2>&1)"; rc=$?
    if [ "$rc" -ne 0 ]; then bad "$1: play failed (rc=$rc): $out"; return; fi
    fired=quiet; [ -f "$TMP/restarted" ] && fired=fired
    if [ "$fired" = "$2" ]; then ok "$1 → Restart galexie $fired (expected $2)"
    else bad "$1 → Restart galexie $fired (expected $2): $out"; fi
  }
  # 3. comment/blank-only edit
  printf '#!/bin/bash\n# header v1\nset -euo pipefail\n\necho run\n' > "$TMP/a.sh"
  printf '# toml v1\nkey = 1\n' > "$TMP/b.toml"
  printf 'printf "#!/bin/bash\\n# header v2 (rewrapped)\\n#\\nset -euo pipefail\\n\\n\\necho run\\n" > a.sh\nprintf "# toml v2\\n\\nkey = 1\\n" > b.toml\n' > "$TMP/mutate.sh"
  run_case "comment/whitespace-only edit" quiet
  # 4. code edit
  printf 'printf "#!/bin/bash\\n# header v2 (rewrapped)\\nset -euo pipefail\\necho run2\\n" > a.sh\n' > "$TMP/mutate.sh"
  run_case "code edit" fired
  # 5. shebang edit counts (bash vs sh)
  printf 'printf "#!/bin/sh\\n# header v2 (rewrapped)\\nset -euo pipefail\\necho run2\\n" > a.sh\n' > "$TMP/mutate.sh"
  run_case "shebang edit" fired
  # 6. absent before, present after
  rm -f "$TMP/b.toml"
  printf 'printf "key = 1\\n" > b.toml\n' > "$TMP/mutate.sh"
  run_case "input created" fired
fi

echo "ansible-galexie-restart-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
