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
#   3. Only inputs the running galexie has loaded at start (captive cfg,
#      galexie.toml, /etc/default/galexie, the unit) are in the effective-
#      inputs list; the append wrapper / oneshot scripts / apt key are not
#      and never notify the restart (2026-08-29 r1 incident: a wrapper-only
#      change restarted a healthy galexie); galexie_restart_ack defaults off.
#
# Behavioural (a real `ansible-playbook -c local` run of
# galexie-effective-checksum.yml against temp files, with a stub
# `Restart galexie` handler that writes a marker and a stub systemctl):
#   4. comment/blank-line-only difference → handler NOT fired;
#   5. code edit, galexie inactive → handler fired (no ack needed);
#   6. code edit, galexie active, no ack → play FAILS (fail-closed);
#   7. code edit, active, galexie_restart_ack=true → handler fired;
#   8. --check --diff shows RUNNING HANDLER [Restart galexie] for a real
#      edit (the 2026-08-29 dry-run showed nothing);
#   9. shebang edit counts; 10. input absent on disk → handler fired.
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

# ── 3. only loaded-at-start inputs may decide a restart ───────────────────
# 2026-08-29 r1 incident: galexie-append.sh sat in the effective-inputs
# list, so a wrapper-only change (exec'd once per service start — the
# running process never reads it) restarted a healthy galexie.
blk="$(task_block "$G" "- name: Galexie inputs whose effective content decides a restart")"
if [ -z "$blk" ]; then
  bad "07-galexie.yml: effective-inputs task not found"
else
  for p in /etc/stellar/captive-core-galexie.cfg /etc/galexie/galexie.toml \
           /etc/default/galexie /etc/systemd/system/galexie.service; do
    if grep -v '^\s*#' <<<"$blk" | grep -q "path: $p\$"; then ok "effective inputs include $p"
    else bad "effective inputs missing $p (a real change there would not restart galexie)"; fi
  done
  for p in galexie-append galexie-archive-tip-lag galexie-archive-fill galexie-archive-contiguity sdf.asc; do
    if grep -v '^\s*#' <<<"$blk" | grep -q "$p"; then
      bad "effective inputs list '$p' — not read by the running galexie; a change there must never restart it"
    else ok "effective inputs do not list $p"; fi
  done
fi
for pat in "src: galexie-archive-tip-lag.sh" "src: galexie-archive-fill.sh" "src: galexie-archive-contiguity.sh" \
           "src: galexie-backfill-status.sh" "dest: /etc/apt/keyrings/sdf.asc"; do
  blk="$(task_block "$G" "$pat")"
  if [ -z "$blk" ]; then bad "07-galexie.yml: task with '$pat' not found"; continue; fi
  if grep -v '^\s*#' <<<"$blk" | grep -q 'Restart galexie'; then
    bad "07-galexie.yml: task '$pat' notifies Restart galexie (the daemon never reads it)"
  else ok "07-galexie.yml: task '$pat' does not notify Restart galexie"; fi
done
if grep -q '^galexie_restart_ack: false$' "$ROLE_TASKS/../defaults/main.yml" 2>/dev/null; then
  ok "defaults/main.yml: galexie_restart_ack defaults to false (fail-closed)"
else
  bad "defaults/main.yml: galexie_restart_ack must default to false"
fi

# ── 4-10. behavioural: the effective-checksum gate + ack guard ───────────
# A real `ansible-playbook -c local` run of galexie-effective-checksum.yml
# against temp files, with the would-be content given inline, a stub
# `Restart galexie` handler that writes a marker, and a stub `systemctl`
# on PATH whose is-active answer is the case's.
CHK="$ROLE_TASKS/galexie-effective-checksum.yml"
if [ ! -f "$CHK" ]; then
  bad "galexie-effective-checksum.yml missing — cannot exercise the gate"
else
  TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
  mkdir -p "$TMP/bin"
  printf '#!/bin/sh\ncat "%s/is-active"; [ "$(cat %s/is-active)" = active ]\n' "$TMP" "$TMP" > "$TMP/bin/systemctl"
  chmod +x "$TMP/bin/systemctl"
  if ! command -v sha256sum >/dev/null; then  # macOS dev box: coreutils absent
    printf '#!/bin/sh\nshasum -a 256 "$@"\n' > "$TMP/bin/sha256sum"; chmod +x "$TMP/bin/sha256sum"
  fi
  cat > "$TMP/fixture.yml" <<YML
---
- hosts: localhost
  connection: local
  gather_facts: false
  vars:
    galexie_restart_ack: false
    galexie_effective_inputs:
      - path: "$TMP/a.cfg"
        content: "{{ lookup('file', '$TMP/want-a.cfg') }}"
      - path: "$TMP/b.toml"
        content: "{{ lookup('file', '$TMP/want-b.toml') }}"
  tasks:
    - name: gate
      ansible.builtin.include_tasks: "$CHK"
  handlers:
    - name: Restart galexie
      ansible.builtin.copy:
        content: fired
        dest: "$TMP/restarted"
YML
  run_case() {  # <desc> <active|inactive> <expect: fired|quiet|refused|previewed> [extra ansible args…]
    local desc="$1" active="$2" expect="$3"; shift 3
    rm -f "$TMP/restarted"; printf '%s\n' "$active" > "$TMP/is-active"
    out="$(cd "$TMP" && PATH="$TMP/bin:$PATH" ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
           ansible-playbook -i localhost, fixture.yml "$@" 2>&1)"; rc=$?
    got=quiet
    [ -f "$TMP/restarted" ] && got=fired
    [ "$rc" -ne 0 ] && got=refused
    if [ "$got" = quiet ] && grep -q 'RUNNING HANDLER \[Restart galexie\]' <<<"$out"; then got=previewed; fi
    if [ "$got" = "$expect" ]; then ok "$desc → $got (expected $expect)"
    else bad "$desc → $got (expected $expect): $out"; fi
  }
  # 4. comment/blank-only difference → no restart, ack not needed even when active
  printf '# header v1\nset -euo pipefail\n\nkey = 1\n' > "$TMP/a.cfg"
  printf '# header v2 (rewrapped)\n#\nset -euo pipefail\n\n\nkey = 1' > "$TMP/want-a.cfg"
  printf '# toml v1\nkey = 1\n' > "$TMP/b.toml"
  printf '# toml v2\n\nkey = 1\n\n' > "$TMP/want-b.toml"
  run_case "comment/whitespace-only difference, galexie active" active quiet
  # 5. real edit, galexie not running (bootstrap) → restart fires, no ack needed
  printf '# toml v2\nkey = 2\n' > "$TMP/want-b.toml"
  run_case "code edit, galexie inactive" inactive fired
  # 6. real edit, galexie active, no ack → the play FAILS before anything runs
  run_case "code edit, galexie active, no ack" active refused
  # 7. same, acknowledged → restart fires
  run_case "code edit, galexie active, ack" active fired -e galexie_restart_ack=true
  # 8. check mode surfaces the handler (the 2026-08-29 dry-run showed none)
  run_case "code edit, galexie active, --check" active previewed --check --diff
  # 9. shebang edit counts (bash vs sh)
  printf '#!/bin/sh\nkey = 1\n' > "$TMP/a.cfg"; printf '#!/bin/bash\nkey = 1\n' > "$TMP/want-a.cfg"
  printf '# toml v2\nkey = 1\n' > "$TMP/want-b.toml"
  run_case "shebang edit, galexie inactive" inactive fired
  # 10. input absent on disk → restart (nothing running can have loaded it)
  printf '#!/bin/sh\nkey = 1\n' > "$TMP/want-a.cfg"; rm -f "$TMP/b.toml"
  run_case "input absent on disk, galexie inactive" inactive fired
fi

echo "ansible-galexie-restart-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
