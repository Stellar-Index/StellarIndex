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

# ── The report (#496) ────────────────────────────────────────────────
# A weekly check that reliably finds something and is reliably ignored
# is worse than none: it teaches everyone to skip the notification. The
# verdict below therefore renders into the run's JOB SUMMARY — the page
# a person actually lands on — and NAMES the tasks and the host paths
# that would change. "r1 has unapplied changes" is unactionable; "these
# six tasks would change, here are their files" was a twenty-minute fix
# on 2026-09-08.
#
# Every drifted task also gets its own `::error file=…,line=…::`
# annotation, resolved back to the `- name:` line in the role, so the
# finding is attached to the code rather than buried in ~1,800 lines of
# ansible stdout.
#
# Unset GITHUB_STEP_SUMMARY (a local run, the fixture tests) writes no
# summary and changes nothing else.
SUMMARY_FILE="${GITHUB_STEP_SUMMARY:-}"
summary() {
  [ -n "$SUMMARY_FILE" ] || return 0
  printf '%s\n' "$*" >> "$SUMMARY_FILE"
}

# where_defined <task name> → "<path>:<line>" for the `- name:` that
# declares it, or empty. Literal match (task names carry em dashes,
# slashes and plus signs), then an exact compare on the stripped line so
# `Install X` does not match `Install X units`.
where_defined() {
  local needle="$1" hit
  while IFS= read -r hit; do
    [ -z "$hit" ] && continue
    local path line text
    path="${hit%%:*}"
    line="${hit#*:}"
    line="${line%%:*}"
    text="${hit#"$path":"$line":}"
    # strip leading whitespace + the list dash
    text="${text#"${text%%[![:space:]]*}"}"
    text="${text#- }"
    text="${text#name: }"
    text="${text%"${text##*[![:space:]]}"}"
    if [ "$text" = "$needle" ]; then
      printf '%s:%s\n' "$path" "$line"
      return 0
    fi
  done < <(grep -rnF -- "name: $needle" configs/ansible 2>/dev/null || true)
  return 0
}

if [ "$#" -ne 1 ]; then
  echo "usage: check-ansible-drift.sh <ansible --check --diff output file>" >&2
  exit 2
fi
DRIFT_OUT="$1"

if [ ! -s "$DRIFT_OUT" ]; then
  echo "ansible-drift ❌ dry-run output '$DRIFT_OUT' is missing or empty — the playbook never produced output. This is NOT a drift signal." >&2
  summary "## ansible-drift ❌ no dry-run output"
  summary ""
  summary "\`$DRIFT_OUT\` is missing or empty — the playbook never produced output."
  summary "This is **not** a drift signal: look at the playbook step above."
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
detail_file="$(mktemp)"
trap 'rm -f "$allowed_file" "$changed_file" "$detail_file"' EXIT

bad_entry=0
while IFS= read -r line; do
  # Blank and comment lines (leading whitespace tolerated, matching
  # scripts/ci/lint-baseline-growth.sh's `^\s*(#|$)`).
  case "${line#"${line%%[![:space:]]*}"}" in
    ''|'#'*) continue ;;
  esac
  if ! grep -qE '#[[:space:]]*[^[:space:]]' <<<"$line"; then
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
  summary "## ansible-drift ❌ no PLAY RECAP"
  summary ""
  summary "The dry-run never finished — invalid vault password, unreachable host,"
  summary "or a task error. **This is not a drift verdict**; read the playbook step."
  exit 1
fi

recap_changed=0
recap_failed=0
while IFS= read -r line; do
  [ -z "$line" ] && continue
  # sigpipe-ok: the producer is a printf of ONE PLAY RECAP line, so grep -oE
  # emits at most a handful of `changed=N` tokens — bytes, not kilobytes.
  n="$(printf '%s' "$line" | grep -oE 'changed=[0-9]+' | head -1 | cut -d= -f2)"
  recap_changed=$((recap_changed + ${n:-0}))
  # Parameter expansion, no pipeline: one recap line carries at most one
  # `failed=` token, and this avoids another early-exit consumer under
  # pipefail (scripts/ci/lint-shell-sigpipe.sh).
  f=0
  case "$line" in
    *failed=*) f="${line##*failed=}"; f="${f%%[!0-9]*}" ;;
  esac
  recap_failed=$((recap_failed + ${f:-0}))
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

# ── What each changed task would touch ───────────────────────────────
# `--diff` prints `--- before: <host path>` for a file it would rewrite,
# and `(item=<unit>)` for a loop. Both answer "which file?", which is the
# half of the report that makes a finding actionable rather than a count.
awk '
  /^TASK \[/ || /^RUNNING HANDLER \[/ {
    name = $0
    sub(/^[^[]*\[/, "", name)
    sub(/\][[:space:]]*\**[[:space:]]*$/, "", name)
    sub(/^[A-Za-z0-9_.\/-]+ : /, "", name)
    task = name
    next
  }
  /^PLAY RECAP/ { task = ""; next }
  /^--- before: / {
    if (task != "") {
      p = substr($0, 13)
      if (!index(paths[task], p)) paths[task] = paths[task] (paths[task] == "" ? "" : ", ") p
    }
    next
  }
  /^changed: \[/ {
    if (task == "") next
    it = ""
    if (match($0, /\(item=[^)]*\)/)) it = substr($0, RSTART + 6, RLENGTH - 7)
    if (it != "") items[task] = items[task] (items[task] == "" ? "" : ", ") it
    hit[task] = 1
  }
  END {
    for (t in hit) {
      d = paths[t]
      if (items[t] != "") d = d (d == "" ? "" : "; ") "items: " items[t]
      if (d != "") printf "%s\t%s\n", t, d
    }
  }
' "$DRIFT_OUT" > "$detail_file"

# detail_for <task> → the host paths / loop items that task would touch.
detail_for() {
  awk -F'\t' -v t="$1" '$1 == t { print $2; exit }' "$detail_file"
}

echo "ansible-drift: PLAY RECAP reports changed=$recap_changed; parsed $parsed_count changed task block(s); $allowed_count enumerated allowance(s)."

# 1b — an ABORTED preview. A task that errors under `--check` is fatal,
# so ansible stops the play and every task after it is never evaluated:
# the run reports a recap, but that recap describes a PREFIX of the role,
# not the role. This is what the weekly run hit on 2026-08-10 (ok=192,
# census-rollup.timer) and 2026-08-24 (ok=117, sla-probe.timer) — in both
# cases the drift the run existed to report sat in the tasks that never
# ran. Fail with the failing task NAMED, and never let a truncated pass
# read as "codified = live".
if [ "$recap_failed" -gt 0 ]; then
  failing="$(awk '
    /^TASK \[/ || /^RUNNING HANDLER \[/ {
      name = $0
      sub(/^[^[]*\[/, "", name); sub(/\][[:space:]]*\**[[:space:]]*$/, "", name)
      sub(/^[A-Za-z0-9_.\/-]+ : /, "", name)
      task = name; next
    }
    /^fatal: \[/ || /^failed: \[/ {
      if (task == "" || done[task]) next
      done[task] = 1
      msg = ""
      if (match($0, /"msg": "[^"]*"/)) msg = substr($0, RSTART + 8, RLENGTH - 9)
      printf "%s\t%s\n", task, msg
    }
  ' "$DRIFT_OUT")"
  {
    echo "::error::ansible-drift: the dry-run ABORTED — $recap_failed task(s) errored, so this run covered only the tasks BEFORE the failure and its changed=$recap_changed is a PREFIX, not a verdict."
    if [ -n "$failing" ]; then
      echo "failed task(s):"
      printf '%s\n' "$failing" | while IFS="$(printf '\t')" read -r t m; do
        loc="$(where_defined "$t")"
        echo "  ✗ ${t}${loc:+  (${loc})}${m:+ — ${m}}"
        if [ -n "$loc" ]; then
          echo "::error file=${loc%%:*},line=${loc##*:}::ansible-drift: this task errored under --check and aborted the preview${m:+ — $m}"
        fi
      done
    fi
    cat <<EOF

"Could not find the requested service <unit>" means the unit is INSTALLED
BY THIS ROLE and is not on the host yet: --check never really wrote it.
That is drift AND a broken preview. Guard the enable/start with the
CHECK-MODE UNIT GUARD rule in
configs/ansible/roles/archival-node/tasks/main.yml, and apply the role so
the unit actually lands.
EOF
  } >&2
  summary "## ansible-drift ❌ preview aborted — this is not a verdict"
  summary ""
  summary "\`$recap_failed\` task(s) errored under \`--check\`, so ansible stopped the play."
  summary "Everything after the failure was never evaluated: \`changed=$recap_changed\` is a **prefix**, not a result."
  summary ""
  if [ -n "$failing" ]; then
    summary "| failed task | where | message |"
    summary "| --- | --- | --- |"
    printf '%s\n' "$failing" | while IFS="$(printf '\t')" read -r t m; do
      summary "| \`$t\` | \`$(where_defined "$t")\` | $m |"
    done
    summary ""
  fi
  summary "See the CHECK-MODE UNIT GUARD rule in \`configs/ansible/roles/archival-node/tasks/main.yml\`."
  exit 1
fi

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
  summary "## ansible-drift ❌ parser/recap disagree"
  summary ""
  summary "PLAY RECAP says \`changed=$recap_changed\` but \`$parsed_count\` changed task block(s) parsed."
  summary "Ansible's stdout format no longer matches this gate's parser. **This is not a drift verdict** — fix the parser, do not widen the allowance."
  exit 1
fi

if [ "$recap_changed" -eq 0 ]; then
  echo "ansible-drift ✅ codified = live (changed=0)."
  summary "## ansible-drift ✅ codified = live"
  summary ""
  summary "The \`--check --diff\` pass of \`archival-node\` against r1 reported \`changed=0\`."
  summary "Every task the role owns is already in the state the repo declares."
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

  # One annotation per drifted task, pinned to its `- name:` line, so the
  # finding is attached to the code instead of ending up as a single
  # unnamed "drift detected" line above ~1,800 lines of ansible stdout.
  drift_count="$(printf '%s\n' "$unexpected" | grep -c . || true)"
  summary "## ansible-drift ❌ $drift_count task(s) would change on r1"
  summary ""
  summary "\`--check --diff\` of \`archival-node\` against r1. Each row is a task whose"
  summary "live state does not match the repo — either r1 needs the playbook applied,"
  summary "or a hand fix on r1 needs codifying (see \`$RUNBOOK\`)."
  summary ""
  summary "| task | declared in | would touch |"
  summary "| --- | --- | --- |"
  while IFS= read -r t; do
    [ -z "$t" ] && continue
    loc="$(where_defined "$t")"
    det="$(detail_for "$t")"
    summary "| \`$t\` | \`${loc:-—}\` | ${det:-—} |"
    if [ -n "$loc" ]; then
      echo "::error file=${loc%%:*},line=${loc##*:}::ansible-drift: this task would change on r1 — codified ≠ live${det:+ ($det)}" >&2
    else
      echo "::error::ansible-drift: '$t' would change on r1 — codified ≠ live${det:+ ($det)}" >&2
    fi
  done <<EOF
$unexpected
EOF
  summary ""
  summary "**Fix:** apply the playbook (\`workflow_dispatch\` with \`apply=true\`), or codify the hand fix."
  summary "A genuinely non-idempotent task belongs in \`$BASELINE\` with a reason and a"
  summary "\`Baseline-Growth:\` commit trailer — a repo-ahead-of-r1 task does **not**."
  exit 1
fi

# 5 — more changed tasks than there are entries to explain them. With a
# single host this cannot trigger once 3 passed, but it is the backstop
# if the parser ever de-duplicates something the recap counts twice.
if [ "$recap_changed" -gt "$allowed_count" ]; then
  echo "::error::ansible drift detected — changed=$recap_changed exceeds the $allowed_count enumerated allowance(s) in $BASELINE." >&2
  summary "## ansible-drift ❌ more changed tasks than allowances"
  summary ""
  summary "\`changed=$recap_changed\` exceeds the \`$allowed_count\` enumerated allowance(s) in \`$BASELINE\`."
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
summary "## ansible-drift ✅ every changed task is enumerated"
summary ""
summary "\`changed=$recap_changed\`, and every one of them is a named, reasoned entry in"
summary "\`$BASELINE\` — no unexplained drift on r1."
summary ""
summary "| task reporting changed | declared in | would touch |"
summary "| --- | --- | --- |"
while IFS= read -r t; do
  [ -z "$t" ] && continue
  summary "| \`$t\` | \`$(where_defined "$t")\` | $(detail_for "$t") |"
done < <(sort -u "$changed_file")
if [ -n "$stale" ]; then
  summary ""
  summary "Allowance(s) that did **not** fire this run — retire them if they have become idempotent:"
  while IFS= read -r t; do
    [ -z "$t" ] && continue
    summary "- \`$t\`"
  done <<EOF
$stale
EOF
fi
