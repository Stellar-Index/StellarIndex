#!/usr/bin/env bash
# check-ansible-drift.sh — the codified≠live verdict for ansible-drift.yml.
#
# Why an ENUMERATION, not a changed-task count: a count lets a hand fix on r1
# that ansible would erase pass silently inside the slack. Every task allowed
# to report `changed` is named, with a reason, in scripts/ci/ansible-drift.baseline;
# ANY other changed task fails at count 1.
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
#   Every failure mode is fail-closed: the gate can be wrong about WHAT drifted,
#   never about WHETHER something did.
#
# Baseline format (scripts/ci/ansible-drift.baseline):
#
#       <task name>  # <why this task reports changed on every run>
#
#   The reason is MANDATORY (an entry without one fails). The task name is
#   matched after stripping ansible's `<role> : ` prefix, so `TASK [archival-node :
#   Sync migrations from repo to r1]` matches `Sync migrations from repo to r1`.
#   Handlers (`RUNNING HANDLER [...]`) match the same way.
#
#   `*.baseline` makes the file SHRINK-ONLY under scripts/ci/lint-baseline-growth.sh:
#   adding an entry needs a `Baseline-Growth:` commit trailer.
#
# Usage:
#   bash scripts/ci/check-ansible-drift.sh /tmp/drift.out
#   ANSIBLE_DRIFT_BASELINE=<file> bash scripts/ci/check-ansible-drift.sh <out>
set -euo pipefail

cd "$(dirname "$0")/../.."

BASELINE="${ANSIBLE_DRIFT_BASELINE:-scripts/ci/ansible-drift.baseline}"
RUNBOOK="docs/operations/ansible-drift.md"

# ── The report ───────────────────────────────────────────────────────
# A weekly check that reliably finds something and is reliably ignored is worse
# than none. The verdict renders into the run's JOB SUMMARY and NAMES the tasks
# and host paths that would change ("these six tasks, here are their files",
# not "r1 has unapplied changes"). Every drifted task also gets a
# `::error file=…,line=…::` annotation on its `- name:` line in the role.
# Unset GITHUB_STEP_SUMMARY (local run, fixture tests) writes no summary.
SUMMARY_FILE="${GITHUB_STEP_SUMMARY:-}"
summary() {
  [ -n "$SUMMARY_FILE" ] || return 0
  printf '%s\n' "$*" >> "$SUMMARY_FILE"
}

# where_defined <task name> → "<path>:<line>" of the `- name:` that declares it,
# or empty. Literal match (names carry em dashes, slashes, plus signs), then an
# exact compare on the stripped line so `Install X` does not match `Install X units`.
where_defined() {
  local needle="$1" hit
  while IFS= read -r hit; do
    [ -z "$hit" ] && continue
    local path line text
    path="${hit%%:*}"
    line="${hit#*:}"
    line="${line%%:*}"
    text="${hit#"$path":"$line":}"
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
# The host the dry-run targeted; only names it in messages.
DRIFT_HOST="${DRIFT_HOST:-r1}"

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
  # Blank and comment lines; leading whitespace tolerated (as lint-baseline-growth.sh).
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
# Only lines AFTER the `PLAY RECAP` banner count: a `--diff` hunk can contain
# the literal text `changed=`.
recap_lines="$(awk '/^PLAY RECAP/ {inrecap=1; next} inrecap && /changed=[0-9]+/ {print}' "$DRIFT_OUT")"

if [ -z "$recap_lines" ]; then
  cat >&2 <<EOF
ansible-drift ❌ ansible produced no PLAY RECAP — the dry-run failed
(invalid vault password, unreachable host, or a task error). See the
"Dry-run the playbook against $DRIFT_HOST" step above. This is NOT a drift signal.
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
  # Parameter expansion, no pipeline: avoids another early-exit consumer under
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
# One line per TASK/RUNNING HANDLER block with at least one `changed:` result,
# normalized to the bare task name. Loop items emit several `changed:` lines
# for one task and the recap counts the task once, so the block is the unit.
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
# `--diff` prints `--- before: <host path>` for a file it would rewrite and
# `(item=<unit>)` for a loop: the "which file?" half that makes a finding actionable.
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

# ── comment-only diffs and the handlers they trigger ──────────────────
# A template whose live copy differs from the repo ONLY in comment text (an
# incident note on r1, a rewritten comment block, a trailing `# why`) reports
# `changed` and notifies its restart handler like a real change, but the
# effective configuration is identical and applying it would buy a production
# restart for a comment. Such a task is classified comment-only, reported and
# not counted as drift; a changed handler is consequential when every
# non-allowed changed task it could follow is comment-only.
#
# "Comment-only" is decided on EFFECTIVE lines: each removed/added line has its
# trailing comment stripped (and leading whitespace, unless indentation is
# semantic — see indent_semantic()), blank and comment-only lines are dropped,
# and the task is comment-only iff removed and added lines form the same
# ORDERED sequence. A changed value, an appearing/disappearing line, or two
# swapped lines stays drift; a per-line multiset compare would pass a reorder
# of two substantive lines.
#
# What is a comment is decided PER FILE from the host path on the hunk's
# `--- before: <path>` line: stripping every `#`, `--` and `//` would discard
# URL hosts and long flags and report a changed endpoint as "comments only".
# The table names ONE line-comment token per file type (the rows of
# `comment_marker` in scripts/ci/config-apply-gate.sh plus the extensionless
# host paths this role renders); a type with no known convention (ClickHouse
# `.xml`, apt.conf.d, a binary) is substantive. A marker only counts when it
# BEGINS a token, so the `//` of `https://` and a URL `#fragment` are not comments.
comment_only_file="$(mktemp)"
handler_file="$(mktemp)"
trap 'rm -f "$allowed_file" "$changed_file" "$detail_file" "$comment_only_file" "$handler_file"' EXIT
awk '
  function strip(n) {
    sub(/^[^[]*\[/, "", n); sub(/\][[:space:]]*\**[[:space:]]*$/, "", n)
    sub(/^[A-Za-z0-9_.\/-]+ : /, "", n); return n
  }
  # marker_for(path) — the line-comment token for the file at that host
  # path, or "" when none is known. Keep in step with comment_marker in
  # scripts/ci/config-apply-gate.sh; the path rows are for files the
  # role renders without an extension.
  function marker_for(path) {
    if (path ~ /\.sql$/) return "--"
    if (path ~ /\.(j2|ya?ml|sh|env|service|timer|socket|mount|target|conf|cfg|ini|toml|py|rules)$/) return "#"
    if (path ~ /^\/etc\/(default|logrotate\.d|cron\.d|sudoers\.d)\//) return "#"
    if (path ~ /\/(Caddyfile|sshd_config)$/) return "#"
    if (path ~ /\.(go|ts|js)$/) return "//"
    return ""
  }
  # indent_semantic(path) — true for file types where leading whitespace
  # is part of the effective configuration (YAML/J2 render to YAML,
  # Python), so stripping it before compare would fold an indentation
  # change — e.g. a list item promoted out from under its parent key —
  # into "comment-only".
  function indent_semantic(path) {
    return (path ~ /\.(j2|ya?ml|py)$/) ? 1 : 0
  }
  function effective(body, mk, keep_indent) {
    if (mk != "") sub("(^|[[:space:]])" mk ".*$", "", body)
    if (!keep_indent) sub(/^[[:space:]]+/, "", body)
    sub(/[[:space:]]+$/, "", body)
    return body
  }
  /^TASK \[/            { task = strip($0); handler[task] = 0; marker = ""; indent = 0; next }
  /^RUNNING HANDLER \[/ { task = strip($0); handler[task] = 1; marker = ""; indent = 0; next }
  /^PLAY RECAP/         { task = ""; marker = ""; indent = 0; next }
  /^--- before: / {
    if (task != "") {
      hunks[task]++
      path = substr($0, 13)
      marker = marker_for(path)
      indent = indent_semantic(path)
      if (marker == "") substantive[task] = 1
    }
    next
  }
  /^(\+\+\+ after: |@@ )/ { next }
  /^[-+]/ {
    if (task == "") next
    body = effective(substr($0, 2), marker, indent)
    if (body == "") next
    # Ordered sequence, not a multiset: two unchanged lines that merely
    # swap position must stay drift, and a per-line count compare cannot
    # tell that apart from an actual reorder-free edit.
    if (substr($0, 1, 1) == "-") minus_seq[task] = minus_seq[task] SUBSEP body
    else plus_seq[task] = plus_seq[task] SUBSEP body
    seqtask[task] = 1
    next
  }
  /^changed: \[/ { if (task != "") { changed[task] = 1 } next }
  END {
    for (t in seqtask) {
      if (minus_seq[t] != plus_seq[t]) substantive[t] = 1
    }
    for (t in changed) {
      if (handler[t]) print t > HANDLERS
      else if (hunks[t] > 0 && !substantive[t]) print t > COMMENT_ONLY
    }
  }
' HANDLERS="$handler_file" COMMENT_ONLY="$comment_only_file" "$DRIFT_OUT"
comment_only_count="$(grep -c . "$comment_only_file" || true)"

echo "ansible-drift: PLAY RECAP reports changed=$recap_changed; parsed $parsed_count changed task block(s); $allowed_count enumerated allowance(s)."

# 1b — an ABORTED preview. A task that errors under `--check` is fatal, so
# ansible stops and later tasks are never evaluated: the recap describes a
# PREFIX of the role and drift may sit in tasks that never ran. Fail with the
# failing task NAMED; never let a truncated pass read as "codified = live".
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

# 2 + 3 — anti-vacuity guards: a format change that makes the parser see
# nothing must fail, not report "no drift".
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
  summary "The \`--check --diff\` pass of \`archival-node\` against $DRIFT_HOST reported \`changed=0\`."
  summary "Every task the role owns is already in the state the repo declares."
  exit 0
fi

echo "changed tasks:"
sed 's/^/  - /' "$changed_file"

# 4 — the verdict.
unexpected="$(sort -u "$changed_file" | grep -vxF -f "$allowed_file" | grep -vxF -f "$comment_only_file" || true)"
consequential=""
if [ -n "$unexpected" ] && [ "$comment_only_count" -gt 0 ]; then
  # If every remaining unexpected task is a handler, they are the restarts a
  # comment-only template change notified: consequential, not drift. One real
  # task among them keeps every handler drift (a handler cannot say what it follows).
  only_handlers="$(printf '%s\n' "$unexpected" | grep -vxF -f "$handler_file" || true)"
  if [ -z "$only_handlers" ]; then
    consequential="$unexpected"
    unexpected=""
  fi
fi
if [ "$comment_only_count" -gt 0 ]; then
  echo "::notice::ansible-drift: $comment_only_count task(s) differ from the repo in comment text only — the effective configuration matches; reported, not counted as drift:"
  sed 's/^/  ≈ /' "$comment_only_file"
  if [ -n "$consequential" ]; then
    echo "  handler(s) that would run only because of those comment-only changes (consequential, not drift):"
    printf '%s\n' "$consequential" | sed 's/^/  ↳ /'
  fi
  echo "  Applying the playbook clears the diff; on $DRIFT_HOST that runs the handlers above, so do it in a maintenance window."
fi
if [ -n "$unexpected" ]; then
  {
    echo "::error::ansible drift detected — changed task(s) with no entry in $BASELINE:"
    printf '%s\n' "$unexpected" | sed 's/^/  ✗ /'
    cat <<EOF

Either a hand fix on $DRIFT_HOST needs codifying, or repo config needs applying —
see $RUNBOOK.

If a task above is GENUINELY non-idempotent (it reports changed on every
run even when nothing differs), add it to $BASELINE
with a reason. That file is shrink-only: the addition needs a
\`Baseline-Growth: <why>\` commit trailer (scripts/ci/lint-baseline-growth.sh),
so the widening is reviewable. Do NOT add a task that is merely
"repo-ahead-of-$DRIFT_HOST" — that is drift, and applying the playbook is the fix.
EOF
  } >&2

  # One annotation per drifted task, pinned to its `- name:` line, not one
  # unnamed "drift detected" line above ~1,800 lines of ansible stdout.
  drift_count="$(printf '%s\n' "$unexpected" | grep -c . || true)"
  summary "## ansible-drift ❌ $drift_count task(s) would change on $DRIFT_HOST"
  summary ""
  summary "\`--check --diff\` of \`archival-node\` against $DRIFT_HOST. Each row is a task whose"
  summary "live state does not match the repo — either $DRIFT_HOST needs the playbook applied,"
  summary "or a hand fix on $DRIFT_HOST needs codifying (see \`$RUNBOOK\`)."
  summary ""
  summary "| task | declared in | would touch |"
  summary "| --- | --- | --- |"
  while IFS= read -r t; do
    [ -z "$t" ] && continue
    loc="$(where_defined "$t")"
    det="$(detail_for "$t")"
    summary "| \`$t\` | \`${loc:-—}\` | ${det:-—} |"
    if [ -n "$loc" ]; then
      echo "::error file=${loc%%:*},line=${loc##*:}::ansible-drift: this task would change on $DRIFT_HOST — codified ≠ live${det:+ ($det)}" >&2
    else
      echo "::error::ansible-drift: '$t' would change on $DRIFT_HOST — codified ≠ live${det:+ ($det)}" >&2
    fi
  done <<EOF
$unexpected
EOF
  summary ""
  summary "**Fix:** apply the playbook (\`workflow_dispatch\` with \`apply=true\`), or codify the hand fix."
  summary "A genuinely non-idempotent task belongs in \`$BASELINE\` with a reason and a"
  summary "\`Baseline-Growth:\` commit trailer — a repo-ahead-of-$DRIFT_HOST task does **not**."
  exit 1
fi

# 5 — more changed tasks than entries to explain them. Backstop if the parser
# ever de-duplicates something the recap counts twice.
consequential_count="$(printf '%s\n' "$consequential" | grep -c . || true)"
if [ "$recap_changed" -gt $((allowed_count + comment_only_count + consequential_count)) ]; then
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

if [ "$comment_only_count" -gt 0 ]; then
  echo "ansible-drift ✅ every changed task is enumerated in $BASELINE, differs in comments only, or is a handler those comment-only changes notified."
else
  echo "ansible-drift ✅ every changed task is enumerated in $BASELINE."
fi
summary "## ansible-drift ✅ every changed task is enumerated or comment-only"
summary ""
summary "\`changed=$recap_changed\`, and every one of them is a named, reasoned entry in"
summary "\`$BASELINE\` — no unexplained drift on $DRIFT_HOST."
summary ""
summary "| task reporting changed | declared in | would touch |"
summary "| --- | --- | --- |"
while IFS= read -r t; do
  [ -z "$t" ] && continue
  tag=""
  grep -qxF -- "$t" "$comment_only_file" && tag=" — **comments only**"
  grep -qxF -- "$t" <<<"$consequential" && tag=" — handler of a comment-only change"
  summary "| \`$t\`$tag | \`$(where_defined "$t")\` | $(detail_for "$t") |"
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
