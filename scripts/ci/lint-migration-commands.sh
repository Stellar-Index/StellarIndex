#!/usr/bin/env bash
# lint-migration-commands.sh — a SQL command written into a migration
# header is a CLAIM, and claims get tested.
#
# WHY THIS EXISTS. A migration header handed the operator its only
# abort mechanism — a `SELECT alter_job(job_id, scheduled => false)`
# with a predicate resolved from the continuous-aggregates catalogue's
# materialization-hypertable-name column. `timescaledb_information.jobs`
# renders that column as COALESCE(ca.user_view_name, ht.table_name), so
# for a continuous aggregate the predicate matched ZERO rows. `alter_job`
# over an empty set is not an error: no output, exit 0 — a disarm that
# matched nothing reads exactly like a disarm that worked. Had the
# policy shipped armed, 66 GB would have dropped 24 hours later while
# the operator believed it was disabled.
#
# Nothing in CI could see that, because nothing in CI had any opinion
# about the SQL above `BEGIN;`. The migration's BODY is executed by the
# integration suite; its HEADER — which is where every operator
# procedure in this repo lives — was prose to every gate in the tree.
#
# migrations/0156_prices_1m_retention.up.sql is the worked example of
# the shape this gate generalises:
# internal/storage/timescale/retention_policy_test.go pins that file's
# arm and disarm commands, its `force => true` requirement, and the
# `DO NOT RUN:` marker convention for a form quoted as a warning rather
# than offered as a recipe.
#
# ── WHAT COUNTS AS AN EXECUTABLE COMMAND ─────────────────────────────
#
# Migration headers in this tree are mostly prose, and prose is full of
# SQL words ("DROP TABLE removes the hypertable, its chunks and its
# compression settings in one statement"). A candidate is an executable
# command only when ALL FOUR hold:
#
#   1. it starts at an UPPERCASE statement verb — SELECT INSERT UPDATE
#      DELETE CALL CREATE ALTER DROP TRUNCATE REFRESH GRANT REVOKE
#      VACUUM ANALYZE CLUSTER REINDEX COPY SET COMMENT EXPLAIN WITH —
#      anywhere in the comment paragraph, not only at its start (a
#      recipe is often introduced mid-sentence with "e.g.");
#   2. it TERMINATES with a semicolon inside the same paragraph. A
#      statement an operator is meant to paste ends in `;`; a fragment
#      quoted to name a shape does not;
#   3. it carries a STRUCTURAL SQL marker — a second uppercase keyword
#      (FROM WHERE INTO TABLE INDEX VIEW VALUES SET ON AS JOIN NULL AND
#      OR ADD COLUMN CONSTRAINT EXISTS CASCADE POLICY JOB MATERIALIZED
#      CONCURRENTLY USING WITH ORDER GROUP LIMIT) or a `name(` function
#      call. "one INSERT per swap (low volume, sparse hot path);" has
#      neither;
#   4. it contains NO lowercase English function word outside quoted
#      literals. SQL keywords in this tree are uppercase and English
#      ones are not, which is the discriminator that keeps this gate
#      off the prose it is surrounded by.
#
# Three classes come out of that, and only one is held to the rule:
#
#   RECIPE   — a runnable command. MUST be covered by a test.
#   WARNING  — carries the `DO NOT RUN` marker. Exempt: it is quoted
#              precisely so an operator RECOGNISES the destructive form,
#              and a test that pinned it would be pinning something
#              nobody should ever type. (0081 / 0126 / 0147 all document
#              `CALL refresh_continuous_aggregate('twap_1h', NULL,
#              now());`, which post-0156 deletes nine years of TWAP
#              history — 0156 quotes it under the marker for exactly
#              this reason.)
#   SKETCH   — contains a literal `...` ellipsis. Exempt: it cannot be
#              pasted and run, so there is no claim to execute. Reported
#              in the summary so the class stays visible rather than
#              becoming a hiding place.
#
# ── HOW A TEST PROVES IT COVERS A COMMAND ────────────────────────────
#
# A RECIPE is covered when some `*_test.go` in the tree BOTH names the
# migration file AND contains a verbatim 16-character-or-longer slice of
# the command (both sides whitespace-normalised first). Naming the file
# alone is not enough — a test that reads a migration for some unrelated
# reason would blanket-cover every command in it. The shared slice is
# what makes the link specific: `scheduled => false`, `SELECT job_id,
# scheduled, next_start`, `hypertable_name = 'prices_1m'` and
# `refresh_continuous_aggregate` are each the assertion that would have
# caught the zero-row predicate, and each is a slice the covering test
# holds verbatim.
#
# This is a TEXT gate, not an execution gate — a shell lint cannot run
# SQL. What it enforces is that somebody wrote an assertion ABOUT the
# command, which is the step whose absence shipped the defect. It is a
# LINK requirement and it is stated as one: a slice as generic as the
# catalogue view the command reads
# (`timescaledb_information.jobs`, 28 characters) satisfies it, so the
# gate cannot judge whether the assertion is the RIGHT one. It can only
# refuse a command that nothing anywhere asserts on, and refuse a test
# that claims a whole header by naming its file. Judging the assertion
# is the reviewer's job, and 0156's test is what that looks like when it
# is done properly.
#
# ── GRANDFATHERING ───────────────────────────────────────────────────
#
# 151 migrations predate this rule and 10 of them carry header commands.
# A gate that failed on all of them would ship disabled, which is worse
# than not shipping it. Pre-existing commands are listed in
# scripts/ci/lint-migration-commands.baseline, one
# `<migration-path>#<despaced-command>  <reason>` entry each, reason
# REQUIRED. It is a scripts/ci/*.baseline, so growing it needs a
# `Baseline-Growth:` commit trailer naming it (lint-baseline-growth.sh),
# and an entry whose command is gone or has since been covered is STALE
# and fails until the line is deleted. The debt shrinks monotonically
# and a NEW migration cannot join it quietly.
#
# Usage: lint-migration-commands.sh [repo-root]
#   The optional root exists for
#   scripts/ci/lint-migration-commands-test.sh, which runs this gate
#   against fixture trees.
#
# Exit 0 clean, non-zero on any violation.
set -euo pipefail
cd "${1:-$(dirname "$0")/../..}"

MIGRATIONS="migrations"
BASELINE="scripts/ci/lint-migration-commands.baseline"

die() { echo "lint-migration-commands: FAIL — $*" >&2; exit 1; }

migs=()
while IFS= read -r f; do
  [ -n "$f" ] || continue
  migs+=("$f")
done < <(find "$MIGRATIONS" -type f -name '*.sql' 2>/dev/null | sed 's#^\./##' | sort)

if [ "${#migs[@]}" -eq 0 ]; then
  die "no .sql files under $MIGRATIONS/.
  A gate with an empty subject set passes forever. If migrations moved,
  move this gate with them — do not delete the check."
fi

# ── extraction ───────────────────────────────────────────────────────
#
# Emits `<file>\t<line>\t<class>\t<normalised command>` per candidate,
# then a `#counts <paragraphs> <recipes> <warnings> <sketches>` line so
# an extractor that silently stopped matching reads as a fault.
commands="$(
  awk '
    function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }

    # English prose, not SQL. Checked with quoted literals blanked so a
    # string value cannot make a real command look like a sentence.
    function prose(s,   t) {
      t = s
      gsub(/'"'"'[^'"'"']*'"'"'/, " ", t)
      return t ~ /(^|[^A-Za-z0-9_])(the|a|an|is|are|was|were|be|been|this|that|these|those|it|its|and|or|not|in|on|of|to|for|with|without|because|so|if|when|which|while|one|two|each|every|all|any|no|does|do|did|has|have|had|will|would|should|must|than|then|but|as|at|by|we|our|you|your|they|their|them|about|into|only|again|why|how|what|who|whose|whom|more|most|less|least|just|even|rather|instead|already|never|always)([^A-Za-z0-9_]|$)/
    }

    # A structural SQL marker: a second uppercase keyword, or a
    # function call written without a space before its paren.
    function structural(s) {
      if (s ~ /(^|[^A-Za-z0-9_])(FROM|WHERE|INTO|TABLE|INDEX|VIEW|VALUES|SET|ON|AS|JOIN|NULL|AND|OR|ADD|COLUMN|CONSTRAINT|EXISTS|CASCADE|POLICY|JOB|MATERIALIZED|CONCURRENTLY|USING|WITH|ORDER|GROUP|LIMIT)([^A-Za-z0-9_]|$)/) return 1
      return s ~ /[A-Za-z0-9_]\(/
    }

    function scan(run, line,   pos, s, vstart, semi, cand, n, before, class) {
      pos = 1
      while (1) {
        s = substr(run, pos)
        if (match(s, /(^|[^A-Za-z0-9_.])(SELECT|INSERT|UPDATE|DELETE|CALL|CREATE|ALTER|DROP|TRUNCATE|REFRESH|GRANT|REVOKE|VACUUM|ANALYZE|CLUSTER|REINDEX|COPY|SET|COMMENT|EXPLAIN|WITH)[ ]/) == 0) return
        vstart = pos + RSTART - 1
        if (substr(run, vstart, 1) !~ /[A-Z]/) vstart++
        cand = substr(run, vstart)
        semi = index(cand, ";")
        if (semi == 0) { pos = vstart + 1; continue }        # no terminator
        cand = substr(cand, 1, semi)
        n = cand; gsub(/[ \t]+/, " ", n); n = trim(n)
        if (prose(n) || !structural(n)) { pos = vstart + 1; continue }

        # The marker sits BEFORE the verb ("DO NOT RUN: CALL …"), so it
        # is read out of the text preceding the command as well as from
        # the command itself.
        before = substr(run, (vstart > 48 ? vstart - 48 : 1), (vstart > 48 ? 48 : vstart - 1))
        if (index(before, "DO NOT RUN") || index(n, "DO NOT RUN")) { class = "WARNING"; warnings++ }
        else if (index(n, "...")) { class = "SKETCH"; sketches++ }
        else { class = "RECIPE"; recipes++ }
        printf "%s\t%d\t%s\t%s\n", FILENAME, line, class, n
        pos = vstart + semi
      }
    }

    function flushrun() { if (run != "") { paragraphs++; scan(run, runline) } run = "" }

    FNR == 1 { flushrun() }
    {
      if ($0 !~ /^[ \t]*--/) { flushrun(); next }
      body = $0; sub(/^[ \t]*--/, "", body)
      if (trim(body) == "") { flushrun(); next }   # a blank comment line ends the paragraph
      if (run == "") runline = FNR
      run = run " " body
    }
    END { flushrun(); printf "#counts %d %d %d %d\n", paragraphs + 0, recipes + 0, warnings + 0, sketches + 0 }
  ' "${migs[@]}"
)"

read -r _ paragraphs n_recipe n_warning n_sketch <<<"$(printf '%s\n' "$commands" | grep '^#counts ')"
commands="$(printf '%s\n' "$commands" | grep -v '^#counts ' || true)"

if [ "${paragraphs:-0}" -eq 0 ]; then
  die "no comment paragraph was read across ${#migs[@]} migration file(s).
  Every migration in this tree carries a header, so a zero here means the
  comment extraction no longer matches — a gate reporting clean because it
  read nothing. Fix the extractor, do not delete the check."
fi

# ── baseline ─────────────────────────────────────────────────────────
declare -a base_keys=()
if [ -f "$BASELINE" ]; then
  lineno=0
  while IFS= read -r raw; do
    lineno=$((lineno + 1))
    case "$raw" in ''|'#'*) continue ;; esac
    key="${raw%%[[:space:]]*}"
    reason="${raw#"$key"}"
    reason="${reason#"${reason%%[![:space:]]*}"}"
    if [ -z "$key" ] || [ "${#reason}" -lt 10 ]; then
      die "$BASELINE:$lineno has no reason:
    $raw
  Format is \`<migration-path>#<despaced-command>  <reason>\`. An exemption
  without a stated reason is the failure mode a baseline exists to prevent."
    fi
    base_keys+=("$key")
  done < "$BASELINE"
fi

in_baseline() {
  local k="$1" b
  for b in ${base_keys+"${base_keys[@]}"}; do
    [ "$b" = "$k" ] && return 0
  done
  return 1
}

# The Go test corpus, enumerated ONCE. Re-walking the tree per command
# turned a sub-second gate into a multi-minute one.
TESTLIST="$(mktemp)"
trap 'rm -f "$TESTLIST"' EXIT
find . \( -name .git -o -name vendor -o -name node_modules \
          -o -name .discovery-repos \) -prune -o \
     -type f -name '*_test.go' -print 2>/dev/null \
  | sed 's#^\./##' | sort > "$TESTLIST"

# covering_tests <migration-path> — every *_test.go naming that file.
covering_tests() {
  local mig="$1" base
  base="$(basename "$mig")"
  [ -s "$TESTLIST" ] || return 0
  tr '\n' '\0' < "$TESTLIST" | xargs -0 grep -lF -e "$base" -- || true
}

# covered <command> <test-file...> — true when a test holds a verbatim
# 16-character slice of the command, both sides whitespace-normalised.
covered() {
  local cmd="$1"; shift
  [ "$#" -gt 0 ] || return 1
  # The command rides in the environment rather than through `awk -v`,
  # which expands backslash escapes in the value it is handed.
  LINT_CMD="$cmd" awk '
    BEGIN { window = 16; cmd = ENVIRON["LINT_CMD"] }
    { line = $0; gsub(/[ \t]+/, " ", line); T = T " " line }
    END {
      gsub(/[ \t]+/, " ", cmd)
      n = length(cmd)
      if (n < window) exit 1
      for (i = 1; i + window - 1 <= n; i++)
        if (index(T, substr(cmd, i, window)) > 0) exit 0
      exit 1
    }
  ' "$@"
}

fail=0
uncovered=0
n_covered=0
observed=""

cur_file=""
tests=()
while IFS=$'\t' read -r file line class cmd; do
  [ -n "$file" ] || continue
  [ "$class" = "RECIPE" ] || continue

  key_cmd="${cmd// /}"
  key="${file}#${key_cmd:0:80}"
  observed="${observed}|${key}|"

  # Commands arrive grouped by file (awk walks the sorted migration
  # list), so the test lookup runs once per migration, not per command.
  if [ "$file" != "$cur_file" ]; then
    cur_file="$file"
    tests=()
    while IFS= read -r t; do
      [ -n "$t" ] || continue
      tests+=("$t")
    done < <(covering_tests "$file")
  fi

  if covered "$cmd" ${tests+"${tests[@]}"}; then
    n_covered=$((n_covered + 1))
    continue
  fi
  uncovered=$((uncovered + 1))
  in_baseline "$key" && continue

  if [ "$fail" -eq 0 ]; then
    echo "lint-migration-commands: FAIL — executable command(s) in a migration header with no test:" >&2
  fi
  fail=1
  echo "  $file:$line" >&2
  echo "    $cmd" >&2
  echo "    baseline key: $key" >&2
done <<<"$commands"

stale=0
for b in ${base_keys+"${base_keys[@]}"}; do
  case "$observed" in
    *"|$b|"*) : ;;
    *) echo "lint-migration-commands: baseline stale: no uncovered command matches" >&2
       echo "    $b" >&2
       echo "  — the command is gone or is now covered by a test; remove the line from $BASELINE." >&2
       stale=1 ;;
  esac
done

if [ "$fail" -ne 0 ]; then
  cat >&2 <<'EOF'

A command in a migration header is the procedure an operator runs, and a
wrong one is silent: `alter_job` over a predicate that matches zero rows
prints nothing and exits 0, which is why 0156's disarm needed a test and
not a review. Close this one of three ways:

  1. TEST IT. Add or extend a *_test.go that names the migration file and
     asserts on the command — pin its predicate, its named arguments, and
     the verification SELECT that shows the outcome.
     internal/storage/timescale/retention_policy_test.go is the worked
     example for migrations/0156_prices_1m_retention.up.sql.

  2. MARK IT A WARNING. If the command is quoted so an operator
     RECOGNISES a destructive form rather than runs it, prefix it with
     `DO NOT RUN:` — the marker this tree already uses for
     `CALL refresh_continuous_aggregate('twap_1h', NULL, now());`.

  3. GRANDFATHER IT. Add the printed baseline key plus a reason to
     scripts/ci/lint-migration-commands.baseline, with a
     `Baseline-Growth:` commit trailer naming that file. This is the
     shrink-only path and it is meant to be the least attractive one.
EOF
  exit 1
fi
[ "$stale" -eq 0 ] || exit 1

echo "lint-migration-commands: OK — ${#migs[@]} migration file(s), ${paragraphs} header" \
     "paragraph(s); ${n_recipe} recipe(s) (${n_covered} test-covered," \
     "${uncovered} grandfathered), ${n_warning} DO-NOT-RUN warning(s)," \
     "${n_sketch} sketch(es)."
