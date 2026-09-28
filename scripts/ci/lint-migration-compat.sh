#!/usr/bin/env bash
# lint-migration-compat.sh — enforce migrations/README.md rule 9:
# every up-migration must be ADDITIVE and OLD-BINARY-SAFE.
#
# Why this gate exists (CID-14, audit-2026-07-23):
#
#   The deploy pipeline applies `migrate up` in the playbook's
#   pre_tasks — BEFORE any binary is swapped. If the new binary then
#   fails its health probe, configs/ansible/tasks/deploy-one-binary.yml
#   rolls back the BINARY ONLY; the schema stays at the new version
#   (CS-099 — that is deliberate policy, because down-migrations are
#   data-destructive and the pipeline cannot know what already depends
#   on what it would revert).
#
#   That policy is only safe if every migration keeps the PREVIOUS
#   released binary working against the NEW schema. Rule 9 said so in
#   prose; nothing enforced it. A single `DROP COLUMN` shipped in the
#   same release that stops reading the column turns a failed deploy
#   into an outage that the rollback cannot clear.
#
# What is flagged (the mechanically-detectable half of rule 9):
#
#   drop-table            DROP TABLE
#   drop-column           ALTER TABLE … DROP COLUMN
#   rename                RENAME TO / RENAME COLUMN
#   alter-type            ALTER [COLUMN] … [SET DATA] TYPE
#   set-not-null          ALTER … SET NOT NULL
#   add-not-null-column   ADD COLUMN … NOT NULL with no DEFAULT
#   drop-view             DROP [MATERIALIZED] VIEW
#   add-constraint        ALTER TABLE … ADD CONSTRAINT (tightening)
#   dml                   DELETE FROM / TRUNCATE / UPDATE … SET, unguarded
#                         (GH #1163 — 0137's bare `DELETE FROM
#                         comet_liquidity;` passed every other class, and
#                         its down was `SELECT 1`: a down/up cycle after
#                         the replay repaired the table silently deleted
#                         the repaired rows again)
#
#   Loosening operations (DROP CONSTRAINT, DROP INDEX, ADD COLUMN
#   nullable, CREATE TABLE/INDEX/VIEW) are additive-safe and are NOT
#   flagged.
#
# Escape hatches:
#
#   1. Inline, per statement — the rule-9 two-release dance:
#          … DROP COLUMN foo;  -- migration-compat:ok <reason>
#      The reason is mandatory and must say why the PREVIOUS released
#      binary is unaffected (e.g. "shape added in 0104, no released
#      binary reads it"). Does NOT clear `dml` — see 3 below.
#
#   2. scripts/ci/migration-compat.baseline — the grandfathered set of
#      pre-existing violations, keyed `<file>:<class>`. Shrink-only:
#      stale entries FAIL, and scripts/ci/lint-baseline-growth.sh makes
#      any growth require an explicit `Baseline-Growth:` commit trailer.
#
#   3. `dml` only: a `RAISE EXCEPTION` row-count guard anywhere in the
#      same migration file (the pattern in
#      migrations/0152_drop_unwired_scaffold_tables.up.sql:88-97 — a
#      `DO $$ … IF n > 0 THEN RAISE EXCEPTION … END IF; END $$;` block).
#      An unconditional DELETE/TRUNCATE/UPDATE cannot prove the previous
#      released binary's row-count assumptions still hold, so the inline
#      marker alone is not enough for this class; it needs a statement
#      that refuses to run rather than a promise in a comment.
#
# Usage:
#   bash scripts/ci/lint-migration-compat.sh              # repo migrations/
#   MIGRATIONS_DIR=dist/migrations \
#     bash scripts/ci/lint-migration-compat.sh --staged   # deploy.yml
#
# `--staged` skips the stale-baseline check: a released tag legitimately
# contains fewer migrations than the current tree, and a deploy must not
# fail because a baseline entry has no counterpart in the older set. New
# violations still fail, which is the point — the staged run is the last
# gate before `migrate up` touches production.
set -euo pipefail

cd "$(dirname "$0")/../.."

MIGRATIONS_DIR="${MIGRATIONS_DIR:-migrations}"
BASELINE="${MIGRATION_COMPAT_BASELINE:-scripts/ci/migration-compat.baseline}"
STAGED=0
for arg in "$@"; do
  case "$arg" in
    --staged) STAGED=1 ;;
    *)
      echo "lint-migration-compat ❌ unknown argument: $arg" >&2
      exit 2
      ;;
  esac
done

if [ ! -d "$MIGRATIONS_DIR" ]; then
  echo "lint-migration-compat ❌ migrations dir not found: $MIGRATIONS_DIR" >&2
  exit 1
fi

# Four parallel arrays (a single `class|regex` string would split on
# the `|` ALTERNATION inside the regexes themselves — that bug silently
# truncated the `rename` pattern to `RENAME[[:space:]]+(TO`, which grep
# rejected as unbalanced and `|| true` swallowed).
#   CLASSES[i]  — display name, also the baseline key suffix
#   MATCH[i]    — case-insensitive ERE that flags the line
#   EXCLUDE[i]  — optional ERE; a matched line containing it is cleared
#   GUARD[i]    — optional ERE; if found ANYWHERE in the file, every hit
#                 of this class in this file is cleared outright (a
#                 file-level escape, stronger than the per-line inline
#                 marker — see `dml`)
CLASSES=(
  'drop-table'
  'drop-column'
  'rename'
  'alter-type'
  'set-not-null'
  'add-not-null-column'
  'drop-view'
  'add-constraint'
  'dml'
)
MATCH=(
  'DROP[[:space:]]+TABLE'
  'DROP[[:space:]]+COLUMN'
  'RENAME[[:space:]]+(TO|COLUMN)'
  'ALTER[[:space:]]+(COLUMN[[:space:]]+)?[a-zA-Z0-9_"]+[[:space:]]+(SET[[:space:]]+DATA[[:space:]]+)?TYPE'
  'SET[[:space:]]+NOT[[:space:]]+NULL'
  'ADD[[:space:]]+(COLUMN[[:space:]]+)?(IF[[:space:]]+NOT[[:space:]]+EXISTS[[:space:]]+)?[a-zA-Z0-9_"]+[[:space:]]+[a-zA-Z0-9_()[:space:],]*NOT[[:space:]]+NULL'
  'DROP[[:space:]]+(MATERIALIZED[[:space:]]+)?VIEW'
  'ADD[[:space:]]+CONSTRAINT'
  # Anchored to the start of the (whitespace-trimmed) line: a real
  # DELETE/TRUNCATE/UPDATE statement always opens its line. That is what
  # keeps this off prose — "TRUNCATE of audit_log" inside a COMMENT ON
  # string, or a `BEFORE TRUNCATE ON t` trigger declaration — which never
  # has the keyword as the line's first token.
  '^[[:space:]]*(DELETE[[:space:]]+FROM|TRUNCATE|UPDATE[[:space:]]+[a-zA-Z0-9_."]+[[:space:]]+SET)'
)
EXCLUDE=(
  ''
  ''
  ''
  ''
  ''
  # `ADD COLUMN … NOT NULL DEFAULT <x>` IS old-binary-safe: an INSERT
  # from the previous binary that omits the column gets the default.
  # Only a NOT NULL column with no default breaks it.
  'DEFAULT'
  ''
  ''
  ''
)
GUARD=(
  ''
  ''
  ''
  ''
  ''
  ''
  ''
  ''
  'RAISE[[:space:]]+EXCEPTION'
)

# Baseline entries, one `<file>:<class>` per non-comment line.
BASELINE_ENTRIES=()
if [ -f "$BASELINE" ]; then
  while IFS= read -r line; do
    case "$line" in ''|\#*) continue ;; esac
    BASELINE_ENTRIES+=("$line")
  done < "$BASELINE"
fi

in_baseline() {
  local needle="$1" e
  for e in "${BASELINE_ENTRIES[@]:-}"; do
    [ "$e" = "$needle" ] && return 0
  done
  return 1
}

# grep exit 1 is "no match" (expected); anything >1 is a real error
# (bad regex, unreadable file) and must NOT be swallowed into a pass.
grep_lines() {
  local out status
  out="$(grep "$@")" && status=0 || status=$?
  if [ "$status" -gt 1 ]; then
    echo "lint-migration-compat ❌ grep failed (status $status) for: $*" >&2
    exit 2
  fi
  printf '%s' "$out"
}

fail=0
SEEN=()   # `<file>:<class>` actually observed this run

for f in "$MIGRATIONS_DIR"/*.up.sql; do
  [ -e "$f" ] || continue
  base="$(basename "$f")"
  for i in "${!CLASSES[@]}"; do
    class="${CLASSES[$i]}"
    re="${MATCH[$i]}"
    exclude="${EXCLUDE[$i]}"
    # Drop whole-line SQL comments before matching so a `-- we used to
    # DROP COLUMN here` note never trips the gate.
    hits="$(grep_lines -nEi -- "$re" "$f")"
    if [ -n "$hits" ]; then
      hits="$(printf '%s\n' "$hits" | grep_lines -vE -- '^[0-9]+:[[:space:]]*--')"
    fi
    if [ -n "$exclude" ] && [ -n "$hits" ]; then
      hits="$(printf '%s\n' "$hits" | grep_lines -viE -- "$exclude")"
    fi
    [ -z "$hits" ] && continue
    SEEN+=("${base}:${class}")
    guard="${GUARD[$i]}"
    if [ -n "$guard" ]; then
      # Guarded classes (`dml`) do NOT get the inline per-line marker
      # escape — a comment is not proof. Only the guard itself, or the
      # baseline, clears a hit.
      if grep -qiE -- "$guard" "$f"; then
        continue
      fi
      if ! in_baseline "${base}:${class}"; then
        echo "lint-migration-compat ❌ ${f}: ${class} — not old-binary-safe (migrations/README.md rule 9):" >&2
        printf '%s\n' "$hits" | sed 's/^/  /' >&2
        fail=1
      fi
      continue
    fi
    # An inline `-- migration-compat:ok <reason>` on the matched line
    # is the rule-9 two-release-dance escape. Reason is mandatory.
    unmarked="$(printf '%s\n' "$hits" \
      | grep_lines -viE -- '--[[:space:]]*migration-compat:ok[[:space:]]+[^[:space:]]')"
    if [ -n "$unmarked" ] && ! in_baseline "${base}:${class}"; then
      echo "lint-migration-compat ❌ ${f}: ${class} — not old-binary-safe (migrations/README.md rule 9):" >&2
      printf '%s\n' "$unmarked" | sed 's/^/  /' >&2
      fail=1
    fi
  done
done

if [ "$fail" -ne 0 ]; then
  cat >&2 <<'EOF'

The deploy pipeline runs `migrate up` BEFORE the binary swap, and a
failed health probe rolls back the BINARY ONLY (CS-099). A migration
that the previous released binary cannot run against therefore turns a
rollback into an outage.

Fix one of these ways:
  * Make it additive (new nullable column / new table / new index) and
    do the destructive half in a later release — the rule-9 two-release
    dance (release N adds, N+1 switches, N+2 drops).
  * If the previous released binary provably cannot be affected, mark
    the statement inline and say why:
        ALTER TABLE t DROP COLUMN c;  -- migration-compat:ok column added in 0104, never read by a released binary
  * For `dml` (DELETE/TRUNCATE/UPDATE): add a RAISE EXCEPTION row-count
    guard to the migration — the inline marker alone does not clear this
    class. See migrations/0152_drop_unwired_scaffold_tables.up.sql:88-97.
EOF
  exit 1
fi

# ── Shrink-only bookkeeping (repo tree only; see header) ──
if [ "$STAGED" -eq 0 ]; then
  stale=0
  for e in "${BASELINE_ENTRIES[@]:-}"; do
    [ -z "$e" ] && continue
    found=0
    for s in "${SEEN[@]:-}"; do
      [ "$s" = "$e" ] && found=1 && break
    done
    if [ "$found" -eq 0 ]; then
      echo "lint-migration-compat ❌ stale baseline entry (no longer matches): $e" >&2
      stale=1
    fi
  done
  if [ "$stale" -ne 0 ]; then
    echo "  Remove it from $BASELINE — the baseline only shrinks." >&2
    exit 1
  fi
fi

echo "✅ migration backward-compat lint passed (rule 9; ${#BASELINE_ENTRIES[@]} grandfathered entr$([ "${#BASELINE_ENTRIES[@]}" -eq 1 ] && echo y || echo ies) in $BASELINE)."
