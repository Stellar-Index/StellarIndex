#!/usr/bin/env bash
# check-ansible-drift.sh — the codified≠live verdict for ansible-drift.yml.
#
# Why this script exists (OBS-drift, audit-2026-07-23):
#
#   The verdict used to be one inline line in the workflow:
#
#       if [ "${CHANGED:-999}" -gt 13 ]; then … fail … fi
#
#   i.e. up to THIRTEEN changed tasks passed the "codified = live"
#   check, on the strength of a one-line comment ("known non-idempotent
#   command tasks (chown recurse, bucket-ensure)") that named nothing.
#   A hand fix on r1 that ansible would erase — the exact failure this
#   guard was built for after the 2026-06-11 rsyslog incident — passes
#   silently as long as it lands inside the slack. The number was also
#   load-bearing folklore: docs/operations/r1-ansible-drift-2026-07-03.md
#   enumerates the eleven tasks it was sized around, but the gate never
#   read that list, so an enumerated task clearing (a real improvement)
#   just handed the slack to genuine drift.
#
#   This replaces the blanket count with an ENUMERATION. Every task that
#   is allowed to report `changed` is named, with a reason, in
#   scripts/ci/ansible-drift.baseline. ANY changed task not on that list
#   fails the gate — at count 1, not count 14.
#
# The verdict, in order:
#
#   1. No PLAY RECAP            → FAIL "the dry-run never ran" (NOT drift).
#   2. recap changed>0 but the  → FAIL "parser found nothing" — a format
#      parser found no tasks       change must not degrade to a silent pass.
#   3. parsed blocks ≠ recap    → FAIL "parser/recap disagree" — same reason.
#   4. any changed task not in  → FAIL: DRIFT. This is the verdict.
#      the baseline
#   5. recap changed > the      → FAIL: more changed tasks than there are
#      number of entries           entries to explain them.
#
#   Every failure mode is fail-closed: the gate can be wrong about WHAT
#   drifted, never wrong about WHETHER something did.
#
# Baseline format (scripts/ci/ansible-drift.baseline):
#
#       <task name>  # <why this task reports changed on every run>
#
#   The reason is MANDATORY — an entry without one fails the gate. The
#   task name is matched after stripping ansible's `<role> : ` prefix,
#   so `TASK [archival-node : Sync migrations from repo to r1]` matches
#   the entry `Sync migrations from repo to r1`. Handlers
#   (`RUNNING HANDLER [...]`) are matched the same way.
#
#   The file is `*.baseline`, so scripts/ci/lint-baseline-growth.sh
#   makes it SHRINK-ONLY: adding an entry needs an explicit
#   `Baseline-Growth:` commit trailer. Widening the allowance is now an
#   auditable act with a name and a reason attached, not a `13` → `14`.
#
# Usage:
#   bash scripts/ci/check-ansible-drift.sh /tmp/drift.out
#   ANSIBLE_DRIFT_BASELINE=<file> bash scripts/ci/check-ansible-drift.sh <out>
set -euo pipefail

cd "$(dirname "$0")/../.."

BASELINE="${ANSIBLE_DRIFT_BASELINE:-scripts/ci/ansible-drift.baseline}"
RUNBOOK="docs/operations/r1-ansible-drift-2026-07-03.md"

if [ "$#" -ne 1 ]; then
  echo "usage: check-ansible-drift.sh <ansible --check --diff output file>" >&2
  exit 2
fi
DRIFT_OUT="$1"

if [ ! -s "$DRIFT_OUT" ]; then
  echo "ansible-drift ❌ dry-run output '$DRIFT_OUT' is missing or empty — the playbook never produced output. This is NOT a drift signal." >&2
  exit 1
fi
if [ ! -f "$BASELINE" ]; then
  echo "ansible-drift ❌ baseline not found: $BASELINE" >&2
  exit 1
fi

# ── The enumerated allowance ─────────────────────────────────────────
# One entry per line: `<task name>  # <reason>`. The reason is required.
allowed_file="$(mktemp)"
changed_file="$(mktemp)"
trap 'rm -f "$allowed_file" "$changed_file"' EXIT

bad_entry=0
while IFS= read -r line; do
  # Blank and comment lines (leading whitespace tolerated, matching
  # scripts/ci/lint-baseline-growth.sh's `^\s*(#|$)`).
  case "${line#"${line%%[![:space:]]*}"}" in
    ''|'#'*) continue ;;
  esac
  if ! printf '%s' "$line" | grep -qE '#[[:space:]]*[^[:space:]]'; then
    echo "ansible-drift ❌ baseline entry has no reason: $line" >&2
    bad_entry=1
    continue
  fi
  # Strip the trailing `# reason` and surrounding whitespace.
  entry="$(printf '%s' "$line" | sed -E 's/[[:space:]]*#.*$//; s/[[:space:]]+$//; s/^[[:space:]]+//')"
  if [ -z "$entry" ]; then
    echo "ansible-drift ❌ baseline entry is a bare reason with no task name: $line" >&2
    bad_entry=1
    continue
  fi
  printf '%s\n' "$entry" >> "$allowed_file"
done < "$BASELINE"

if [ "$bad_entry" -ne 0 ]; then
  echo "ansible-drift: every allowance must name a task AND say why it reports changed every run." >&2
  exit 1
fi

allowed_count="$(wc -l < "$allowed_file" | tr -d ' ')"

# ── The PLAY RECAP ───────────────────────────────────────────────────
# Only lines AFTER the `PLAY RECAP` banner are recap lines; a `--diff`
# hunk can contain the literal text `changed=` and must not be read as
# one (the pre-fix gate grepped the whole file).
recap_lines="$(awk '/^PLAY RECAP/ {inrecap=1; next} inrecap && /changed=[0-9]+/ {print}' "$DRIFT_OUT")"

if [ -z "$recap_lines" ]; then
  cat >&2 <<EOF
ansible-drift ❌ ansible produced no PLAY RECAP — the dry-run failed
(invalid vault password, unreachable host, or a task error). See the
"Dry-run the playbook against r1" step above. This is NOT a drift signal.
EOF
  exit 1
fi

recap_changed=0
while IFS= read -r line; do
  [ -z "$line" ] && continue
  n="$(printf '%s' "$line" | grep -oE 'changed=[0-9]+' | head -1 | cut -d= -f2)"
  recap_changed=$((recap_changed + ${n:-0}))
done <<EOF
$recap_lines
EOF

# ── The changed tasks ────────────────────────────────────────────────
# One line per TASK/RUNNING HANDLER block that reported at least one
# `changed:` result, normalized to the bare task name. Loop items emit
# several `changed:` lines for a single task; the recap counts the task
# once, so the block — not the line — is the unit.
awk '
  /^TASK \[/ || /^RUNNING HANDLER \[/ {
    name = $0
    sub(/^[^[]*\[/, "", name)          # drop "TASK [" / "RUNNING HANDLER ["
    sub(/\][[:space:]]*\**[[:space:]]*$/, "", name)   # drop "] ****" padding
    sub(/^[A-Za-z0-9_.\/-]+ : /, "", name)            # drop the "<role> : " prefix
    task = name
    seen = 0
    next
  }
  /^PLAY RECAP/ { task = ""; seen = 0; next }
  /^changed: \[/ {
    if (task != "" && seen == 0) { seen = 1; print task }
  }
' "$DRIFT_OUT" > "$changed_file"

parsed_count="$(wc -l < "$changed_file" | tr -d ' ')"

echo "ansible-drift: PLAY RECAP reports changed=$recap_changed; parsed $parsed_count changed task block(s); $allowed_count enumerated allowance(s)."

# 2 + 3 — the anti-vacuity guards. A callback/format change that makes
# the parser silently see nothing must fail, not report "no drift".
if [ "$recap_changed" -ne "$parsed_count" ]; then
  cat >&2 <<EOF
ansible-drift ❌ parser/recap disagree — PLAY RECAP says changed=$recap_changed
but $parsed_count changed task block(s) were parsed from the output.

This is NOT a drift verdict: ansible's stdout format (configs/ansible/ansible.cfg
sets stdout_callback=default, result_format=yaml) no longer matches what this
script parses, or the play ran against more than one host. Fix the parser —
do not raise an allowance to make this pass.
EOF
  exit 1
fi

if [ "$recap_changed" -eq 0 ]; then
  echo "ansible-drift ✅ codified = live (changed=0)."
  exit 0
fi

echo "changed tasks:"
sed 's/^/  - /' "$changed_file"

# 4 — the verdict.
unexpected="$(sort -u "$changed_file" | grep -vxF -f "$allowed_file" || true)"
if [ -n "$unexpected" ]; then
  {
    echo "::error::ansible drift detected — changed task(s) with no entry in $BASELINE:"
    printf '%s\n' "$unexpected" | sed 's/^/  ✗ /'
    cat <<EOF

Either a hand fix on r1 needs codifying, or repo config needs applying —
see $RUNBOOK.

If a task above is GENUINELY non-idempotent (it reports changed on every
run even when nothing differs), add it to $BASELINE
with a reason. That file is shrink-only: the addition needs a
\`Baseline-Growth: <why>\` commit trailer (scripts/ci/lint-baseline-growth.sh),
so the widening is reviewable. Do NOT add a task that is merely
"repo-ahead-of-r1" — that is drift, and applying the playbook is the fix.
EOF
  } >&2
  exit 1
fi

# 5 — more changed tasks than there are entries to explain them. With a
# single host this cannot trigger once 3 passed, but it is the backstop
# if the parser ever de-duplicates something the recap counts twice.
if [ "$recap_changed" -gt "$allowed_count" ]; then
  echo "::error::ansible drift detected — changed=$recap_changed exceeds the $allowed_count enumerated allowance(s) in $BASELINE." >&2
  exit 1
fi

# Stale entries are reported, not fatal: an allowance task can legitimately
# be skipped on a given run (a `when:` gate, a role tag selection).
stale="$(grep -vxF -f <(sort -u "$changed_file") "$allowed_file" || true)"
if [ -n "$stale" ]; then
  echo "::notice::ansible-drift: enumerated allowance(s) that did NOT report changed this run — retire them from $BASELINE if they have become idempotent:"
  printf '%s\n' "$stale" | sed 's/^/  · /'
fi

echo "ansible-drift ✅ every changed task is enumerated in $BASELINE."
