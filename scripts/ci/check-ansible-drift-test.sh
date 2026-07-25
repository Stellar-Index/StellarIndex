#!/usr/bin/env bash
# check-ansible-drift-test.sh — fixture tests for the codified≠live gate.
#
# The gate in scripts/ci/check-ansible-drift.sh is the only thing that
# turns the weekly `--check --diff` against live r1 into a verdict. Its
# predecessor was a single `[ "$CHANGED" -gt 13 ]`, so up to thirteen
# unnamed changed tasks — real hand-drift on production included —
# passed as "codified = live" (OBS-drift, audit-2026-07-23).
#
# The load-bearing case is `unknown drift under the OLD allowance`
# below: it runs the pre-fix rule and the current gate against the SAME
# ansible output and asserts they DISAGREE — old passes, new fails.
# That is the defect, executable.
#
# Also pinned, because each is a way a drift gate degrades to a silent
# pass rather than a loud failure:
#   - no PLAY RECAP (the dry-run died) is NOT "no drift";
#   - a stdout-format change that blinds the parser fails, it does not
#     report clean;
#   - `--diff` hunk text containing `changed=` is not read as a recap;
#   - an allowance entry without a reason is rejected;
#   - loop items (several `changed:` lines, one task) count once;
#   - handlers (`RUNNING HANDLER [...]`) are subject to the allowance too.
#
# Run: bash scripts/ci/check-ansible-drift-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/check-ansible-drift.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# run <drift-output> <baseline-content> → sets RC + OUT
run() {
  printf '%s\n' "$1" > "$TMP/drift.out"
  printf '%s\n' "$2" > "$TMP/baseline"
  OUT="$(ANSIBLE_DRIFT_BASELINE="$TMP/baseline" bash "$GATE" "$TMP/drift.out" 2>&1)"
  RC=$?
}

# expect <name> <want-rc> [<want-substring>]
expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_sub" ] && ! printf '%s' "$OUT" | grep -qF -- "$want_sub"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# The pre-fix verdict, verbatim from .github/workflows/ansible-drift.yml
# before this gate replaced it. Kept here so the regression it allowed is
# demonstrated, not asserted.
old_gate() {
  local out="$1"
  local recap changed
  recap="$(grep -E "changed=[0-9]+" "$out" | tail -1)"
  [ -z "$recap" ] && return 1
  changed="$(echo "$recap" | grep -oE "changed=[0-9]+" | cut -d= -f2)"
  [ "${changed:-999}" -gt 13 ] && return 1
  return 0
}

BASELINE_3='Ensure migrations dir exists on r1  # rsync rewrites the dir metadata every run
Sync migrations from repo to r1  # archive-mode mtime compare against a fresh checkout
Enable + start disable-thp service  # Type=oneshot without RemainAfterExit'

# recap <n> — a PLAY RECAP banner + host line reporting changed=<n>.
recap() {
  printf 'PLAY RECAP *********************************************************************\nr1                         : ok=214  changed=%s   unreachable=0    failed=0    skipped=53   rescued=0    ignored=3\n' "$1"
}

# task <name> <status> — one ansible task block.
task() {
  printf 'TASK [archival-node : %s] ***************************************\n%s: [r1]\n\n' "$1" "$2"
}

# ── 1. The defect: unknown drift inside the old numeric allowance ────
# Thirteen changed tasks, only three of them enumerated. The other ten
# are exactly the failure this guard exists for — a hand fix on r1 that
# ansible would erase, or repo config that was never applied.
drift13="$(
  task 'Ensure migrations dir exists on r1' changed
  task 'Sync migrations from repo to r1' changed
  task 'Enable + start disable-thp service' changed
  for i in 1 2 3 4 5 6 7 8 9 10; do task "Undeclared hand-drift $i" changed; done
  recap 13
)"
printf '%s\n' "$drift13" > "$TMP/old.out"
if old_gate "$TMP/old.out"; then
  echo "ok: pre-fix gate PASSED 13 changed tasks (10 of them undeclared) — the defect"
  pass=$((pass + 1))
else
  echo "FAIL: pre-fix gate did not reproduce — expected it to pass changed=13" >&2
  fail=$((fail + 1))
fi
run "$drift13" "$BASELINE_3"
expect 'unknown drift under the OLD allowance now fails' 1 'Undeclared hand-drift 1'

# A single unknown changed task — nowhere near 13 — must also fail.
run "$(
  task 'Sync migrations from repo to r1' changed
  task 'nftables ruleset' changed
  recap 2
)" "$BASELINE_3"
expect 'one unenumerated changed task fails' 1 'nftables ruleset'

# ── 2. The clean paths ───────────────────────────────────────────────
run "$(
  task 'Ensure migrations dir exists on r1' ok
  recap 0
)" "$BASELINE_3"
expect 'changed=0 passes' 0 'codified = live'

run "$(
  task 'Ensure migrations dir exists on r1' changed
  task 'Sync migrations from repo to r1' changed
  task 'Enable + start disable-thp service' changed
  recap 3
)" "$BASELINE_3"
expect 'only enumerated tasks changed passes' 0 'every changed task is enumerated'

run "$(
  task 'Sync migrations from repo to r1' changed
  recap 1
)" "$BASELINE_3"
expect 'unused allowance entries are reported, not fatal' 0 'did NOT report changed'

# ── 3. Fail-closed on a broken dry-run, never "no drift" ─────────────
run 'ERROR! Invalid vault password was provided from file' "$BASELINE_3"
expect 'no PLAY RECAP fails' 1 'no PLAY RECAP'

run "$(
  task 'Sync migrations from repo to r1' changed
  recap 1
)" '# every entry commented out'
expect 'an empty allowance still fails on a changed task' 1 'Sync migrations from repo to r1'

# ── 4. Anti-vacuity: a blinded parser must fail, not report clean ────
# Simulates a callback/format change (or a second host) where the recap
# count and the parsed task blocks no longer line up.
run "$(recap 4)" "$BASELINE_3"
expect 'recap says changed but no task blocks parsed → fails' 1 'parser/recap disagree'

run "$(
  task 'Sync migrations from repo to r1' changed
  recap 9
)" "$BASELINE_3"
expect 'more recap changes than parsed blocks → fails' 1 'parser/recap disagree'

# ── 5. Parsing precision ─────────────────────────────────────────────
# `--diff` prints file hunks before the status line. A hunk containing
# the literal `changed=` must not be mistaken for a PLAY RECAP: the
# pre-fix gate grepped the whole file for `changed=[0-9]+`.
run "$(
  printf 'TASK [archival-node : Template stellarindex.toml] ***********\n'
  printf -- '--- before: /etc/stellarindex.toml\n+++ after: /etc/stellarindex.toml\n@@ -1,2 +1,2 @@\n-mode = "changed=99"\n+mode = "changed=0"\n\n'
  printf 'changed: [r1]\n\n'
  recap 1
)" 'Template stellarindex.toml  # fixture'
expect 'a diff hunk containing changed= is not read as a recap' 0 'changed=1'

# Loop items emit one `changed:` line per item; the recap counts the
# task once. Counting lines instead of blocks would trip the anti-
# vacuity check on every looping task.
run "$(
  printf 'TASK [archival-node : Ownership — data dirs] ****************\n'
  printf 'changed: [r1] => (item=stellarindex)\nchanged: [r1] => (item=galexie)\nok: [r1] => (item=minio)\n\n'
  recap 1
)" 'Ownership — data dirs  # fixture'
expect 'a loop task counts once, not once per item' 0 'every changed task is enumerated'

# Handlers are named blocks too, and are subject to the same allowance.
run "$(
  printf 'RUNNING HANDLER [archival-node : Restart stellarindex-smoke timer] ****\nchanged: [r1]\n\n'
  recap 1
)" "$BASELINE_3"
expect 'a changed handler is not exempt from the allowance' 1 'Restart stellarindex-smoke timer'

# pre_tasks have no `<role> : ` prefix; entries are the bare name either way.
run "$(
  printf 'TASK [Confirm Ubuntu 22.04 or 24.04 LTS] ********************\nchanged: [r1]\n\n'
  recap 1
)" 'Confirm Ubuntu 22.04 or 24.04 LTS  # fixture'
expect 'a task with no role prefix matches its entry' 0 'every changed task is enumerated'

# ── 6. The allowance must justify itself ─────────────────────────────
run "$(
  task 'Sync migrations from repo to r1' changed
  recap 1
)" 'Sync migrations from repo to r1'
expect 'an allowance entry without a reason is rejected' 1 'no reason'

run "$(
  task 'Sync migrations from repo to r1' changed
  recap 1
)" '# just a comment
   # indented comment'
expect 'comment-only baseline yields no allowance' 1 'Sync migrations from repo to r1'

echo
echo "check-ansible-drift-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
