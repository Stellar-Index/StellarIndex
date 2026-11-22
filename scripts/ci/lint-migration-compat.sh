#!/usr/bin/env bash
# lint-migration-compat.sh — enforce migrations/README.md rule 9:
# every up-migration must be ADDITIVE and OLD-BINARY-SAFE.
#
# Why: the playbook runs `migrate up` in pre_tasks, BEFORE any binary swap. If
# the new binary fails its health probe, deploy-one-binary.yml rolls back the
# BINARY ONLY; the schema stays (down-migrations are data-destructive). That
# is safe only if every migration keeps the PREVIOUS released binary working
# against the NEW schema. A `DROP COLUMN` shipped with the release that stops
# reading it turns a failed deploy into an outage rollback cannot clear.
#
# What it does NOT check: the rollback target is the binary the host was
# running, which after a skip-ahead deploy is older than the previous
# release. This lint judges each file on its own and never sees the host's
# version, so a marker reason that holds only from some release on (0207:
# "only for a fleet already on the window key") is not enforced; deploy
# such a host through that release first.
#
# Flagged (the mechanically detectable half of rule 9):
#   drop-table            DROP TABLE
#   drop-column           ALTER TABLE … DROP [COLUMN] c
#   rename                RENAME TO / RENAME [COLUMN] c TO d
#   alter-type            ALTER [COLUMN] … [SET DATA] TYPE
#   set-not-null          ALTER … SET NOT NULL
#   add-not-null-column   ADD COLUMN … NOT NULL with no DEFAULT
#   drop-view             DROP [MATERIALIZED] VIEW
#   add-constraint        ALTER TABLE … ADD CONSTRAINT (tightening)
#   add-unique            CREATE UNIQUE INDEX, ADD PRIMARY KEY / UNIQUE / CHECK
#                         (the same tightening without the keyword)
#   drop-arbiter          DROP CONSTRAINT / DROP INDEX / RENAME CONSTRAINT (the
#                         previous binary's ON CONFLICT target may be gone)
#   drop-default          ALTER … DROP DEFAULT (an INSERT omitting the column
#                         now fails or writes NULL)
#   drop-function         DROP FUNCTION / PROCEDURE / TYPE
#   add-foreign-key       ADD FOREIGN KEY
#   revoke                REVOKE
#   before-trigger        CREATE TRIGGER … BEFORE …
#   drop-other            any other DROP (TRIGGER, SCHEMA, SEQUENCE, OWNED …),
#                         DISABLE/ENABLE TRIGGER
#   owner                 OWNER TO
#   dml                   DELETE FROM / TRUNCATE / UPDATE … SET / MERGE INTO
#                         at a statement start (DO blocks and CTEs included),
#                         the chunk-dropping SELECT, unguarded
#                         (a bare DELETE with a `SELECT 1` down re-deletes
#                         replay-repaired rows on a down/up)
# Nullable ADD COLUMN and CREATE TABLE/INDEX/VIEW are additive-safe and not
# flagged. Matching is per line after each ALTER is split into one clause per
# line; other statements split across lines are not seen. Not flagged:
# AFTER/CONSTRAINT triggers and CREATE OR REPLACE FUNCTION, which can change
# what the previous binary's writes do (0215/0220/0222, 0188/0205/0223 ship
# that shape unbaselined).
#
# Escape hatches:
#   1. Inline, per statement (the rule-9 two-release dance):
#          … DROP COLUMN foo;  -- migration-compat:ok <reason>
#      The reason is mandatory and must say why the PREVIOUS released binary
#      is unaffected, and name the oldest release it holds for if that is
#      not every older one. The lint checks only that a reason is present.
#      Does NOT clear `dml` (see 3).
#   2. scripts/ci/migration-compat.baseline — grandfathered violations keyed
#      `<file>:<class>`. Shrink-only: stale entries FAIL, and growth needs a
#      `Baseline-Growth:` trailer (scripts/ci/lint-baseline-growth.sh).
#   3. `dml` only: a `RAISE EXCEPTION` row-count guard anywhere in the same
#      file (`DO $$ … IF n > 0 THEN RAISE EXCEPTION … END IF; END $$;`, as in
#      migrations/0152_drop_unwired_scaffold_tables.up.sql:88-97). An
#      unconditional DELETE/TRUNCATE/UPDATE cannot prove the previous binary's
#      row-count assumptions; it needs a statement that refuses to run, not a
#      comment promise.
#
# Usage:
#   bash scripts/ci/lint-migration-compat.sh              # repo migrations/
#   MIGRATIONS_DIR=dist/migrations \
#     bash scripts/ci/lint-migration-compat.sh --staged   # deploy.yml
# `--staged` skips the stale-baseline check (a released tag legitimately has
# fewer migrations than the tree); new violations still fail, as the last gate
# before `migrate up` touches production.
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

# Four parallel arrays, not `class|regex` strings: the `|` alternation inside
# the regexes would split them (it once truncated `rename` to an unbalanced
# regex that `|| true` swallowed).
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
  'add-unique'
  'drop-column'
  'drop-arbiter'
  'drop-default'
  'drop-function'
  'add-foreign-key'
  'revoke'
  'before-trigger'
  'drop-other'
  'owner'
  'dml'
)
MATCH=(
  'DROP[[:space:]]+TABLE'
  'DROP[[:space:]]+COLUMN'
  'RENAME[[:space:]]+(TO|COLUMN)|RENAME[[:space:]]+[a-zA-Z0-9_"]+[[:space:]]+TO'
  'ALTER[[:space:]]+(COLUMN[[:space:]]+)?[a-zA-Z0-9_"]+[[:space:]]+(SET[[:space:]]+DATA[[:space:]]+)?TYPE'
  'SET[[:space:]]+NOT[[:space:]]+NULL'
  'ADD[[:space:]]+(COLUMN[[:space:]]+)?(IF[[:space:]]+NOT[[:space:]]+EXISTS[[:space:]]+)?[a-zA-Z0-9_"]+[[:space:]]+[a-zA-Z0-9_()[:space:],]*NOT[[:space:]]+NULL'
  'DROP[[:space:]]+(MATERIALIZED[[:space:]]+)?VIEW'
  'ADD[[:space:]]+CONSTRAINT'
  # Tightening without the CONSTRAINT keyword: a write the previous binary
  # still makes can now be refused.
  'CREATE[[:space:]]+UNIQUE[[:space:]]+INDEX|ADD[[:space:]]+(PRIMARY[[:space:]]+KEY|UNIQUE|CHECK)([[:space:]]|\()'
  # DROP with the COLUMN keyword left out; EXCLUDE clears the other DROP forms.
  'ALTER[[:space:]]+TABLE.*[[:space:]]DROP[[:space:]]+(IF[[:space:]]+EXISTS[[:space:]]+)?[a-zA-Z0-9_"]+'
  'DROP[[:space:]]+(CONSTRAINT|INDEX)|RENAME[[:space:]]+CONSTRAINT'
  'DROP[[:space:]]+DEFAULT'
  'DROP[[:space:]]+(FUNCTION|PROCEDURE|TYPE)'
  'ADD[[:space:]]+FOREIGN[[:space:]]+KEY'
  # A privilege the previous binary still uses can be gone after this.
  '^[[:space:]]*REVOKE[[:space:]]'
  # A BEFORE trigger can refuse or rewrite a write the previous binary makes.
  'CREATE[[:space:]].*TRIGGER[[:space:]].*[[:space:]]BEFORE[[:space:]]'
  # Every DROP the classes above do not name (TRIGGER, SCHEMA, SEQUENCE, OWNED,
  # POLICY, EXTENSION …), and switching a trigger off.
  '^[[:space:]]*DROP[[:space:]]+[A-Za-z]|(DISABLE|ENABLE)[[:space:]]+(REPLICA[[:space:]]+|ALWAYS[[:space:]]+)?(TRIGGER|RULE)'
  'OWNER[[:space:]]+TO'
  # Anchored to a statement start: line start, `;`, `(` (a data-modifying
  # CTE) or BEGIN/THEN/ELSE/LOOP/AS (a DO block). That keeps it off prose
  # ("TRUNCATE of audit_log" in a COMMENT ON string, a `BEFORE TRUNCATE ON t`
  # trigger). drop_chunks is a call, so it is matched wherever it is called.
  # Not seen: DML built as a string for a dynamic EXECUTE.
  # flatten() joins a statement that opens a line, so `UPDATE t` / `SET …` is one line.
  '(^|[;(]|[^a-zA-Z0-9_](BEGIN|THEN|ELSE|LOOP|AS))[[:space:]]*(DELETE[[:space:]]+FROM|TRUNCATE|MERGE[[:space:]]+INTO|UPDATE[[:space:]]+(ONLY[[:space:]]+)?[a-zA-Z0-9_."]+([[:space:]]+(AS[[:space:]]+)?[a-zA-Z0-9_"]+)?[[:space:]]+SET)|(^|[^a-zA-Z0-9_])drop_chunks[[:space:]]*\('
)
EXCLUDE=(
  ''
  ''
  ''
  ''
  ''
  # `ADD COLUMN … NOT NULL DEFAULT <x>` is old-binary-safe (an INSERT omitting
  # the column gets the default); only a NOT NULL column without one breaks.
  'DEFAULT'
  ''
  ''
  ''
  'DROP[[:space:]]+(COLUMN|CONSTRAINT|DEFAULT|NOT|EXPRESSION|IDENTITY)([[:space:]]|;|$)'
  ''
  ''
  ''
  ''
  ''
  ''
  '^[0-9]+:[[:space:]]*DROP[[:space:]]+(TABLE|MATERIALIZED|VIEW|CONSTRAINT|INDEX|FUNCTION|PROCEDURE|TYPE|COLUMN|DEFAULT)([[:space:]]|$)'
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
  ''
  ''
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

# fn_body_lines <file>: line numbers inside CREATE FUNCTION/PROCEDURE $$ bodies, which
# run when called, not at migrate time. A routine the same file calls at top level
# (SELECT/PERFORM/CALL name(...)) does run at migrate time, so its body is kept.
# Other body quoting is left to the DML checks.
fn_body_lines() {
  awk '
    function fname(l,    n) {
      n = l; sub(/^[ \t]*create (or replace )?(function|procedure)[ \t]+/, "", n); sub(/[ \t(].*$/, "", n); return n
    }
    { l = tolower($0) }
    infn { body[cur] = body[cur] " " NR; if (gsub(/\$[a-z0-9_]*\$/, "&", l) % 2) infn = 0; next }
    l ~ /^[ \t]*create (or replace )?(function|procedure) / { pend = 1; cur = fname(l) }
    pend && (c = gsub(/\$[a-z0-9_]*\$/, "&", l)) { pend = 0; if (c % 2) infn = 1; next }
    pend && l ~ /;/ { pend = 0 }
    l !~ /^[ \t]*--/ && l ~ /(^|[^a-z_])(select|perform|call)[ \t]/ { top[++nt] = l }
    END {
      for (name in body) {
        bare = name; sub(/^.*\./, "", bare)
        called = 0
        for (i = 1; i <= nt && !called; i++)
          if (index(top[i], name "(") || index(top[i], name " (") || index(top[i], "." bare "(") ||
              top[i] ~ ("(select|perform|call)[ \t]+" bare "[ \t]*\\("))
            called = 1
        if (!called) { n = split(body[name], a, " "); for (j = 1; j <= n; j++) print a[j] }
      }
    }' "$1"
}

# flatten <file>: the file with each ALTER statement split into one clause per
# line (`ALTER TABLE t ADD …, DROP …` becomes two lines) and each multi-line
# CREATE TRIGGER joined onto its first line, so a class regex sees a statement
# or clause whole and an EXCLUDE or inline marker applies to its own clause only.
# A clause lands on the output line where it starts; later lines fill with empty
# ones so line numbers hold (a statement with more clauses than lines drifts).
flatten() {
  awk '
    # Quote-aware: a `--` inside a string or a same-line /* */ must not leave a
    # held statement without its closing `;` and swallow the next one.
    function strip(l,    i, ch, out, sq, inb) {
      out = ""; sq = 0; inb = 0
      for (i = 1; i <= length(l); i++) {
        ch = substr(l, i, 1)
        if (inb) { if (substr(l, i, 2) == "*/") { inb = 0; i++ }; continue }
        if (ch == "\047") sq = !sq
        if (!sq && substr(l, i, 2) == "--") break
        if (!sq && substr(l, i, 2) == "/*") { inb = 1; i++; continue }
        out = out ch
      }
      return out
    }
    function feed(t,    i, ch) {
      for (i = 1; i <= length(t); i++) {
        ch = substr(t, i, 1)
        if (ch == "\047") q = !q
        if (!q && ch == "(") depth++
        if (!q && ch == ")") depth--
        if (split_clauses && !q && depth <= 0 && ch == ",") { n++; cl[n] = cur; cur = ""; continue }
        cur = cur ch
      }
      cur = cur " "
    }
    function mark(lo, m,    k, hi) {
      if (m == "") return
      hi = (cur ~ /^[ \t]*$/) ? n : n + 1
      for (k = lo; k <= hi; k++) mk[k] = m
    }
    function flush(    i, out, c) {
      n++; cl[n] = cur
      out = 0
      for (i = 1; i <= n; i++) {
        c = cl[i]; sub(/^[ \t]+/, "", c); sub(/[ \t]+$/, "", c)
        if (c == "" || c == ";") continue
        print (i > 1 ? "ALTER TABLE " : "") c (mk[i] != "" ? " " mk[i] : ""); out++
      }
      for (; out < held + 1; out++) print ""
      open = 0
    }
    function begin(raw, t, m) {
      open = 1; held = 0; n = 0; cur = ""; q = 0; depth = 0
      delete cl; delete mk
      split_clauses = (raw ~ /^[ \t]*[Aa][Ll][Tt][Ee][Rr][ \t]/)
      feed(t); mark(1, m)
    }
    {
      raw = $0
      if (match(raw, /--[ \t]*migration-compat:ok[ \t]+[^ \t].*$/)) m = substr(raw, RSTART); else m = ""
      t = strip(raw); sub(/[ \t]+$/, "", t)
      if (open) {
        held++
        lo = n + 1
        feed(t); mark(lo, m)
        if (t ~ /;$/ || held > 80) flush()
        next
      }
      if (raw ~ /^[ \t]*[Aa][Ll][Tt][Ee][Rr][ \t]/ || tolower(raw) ~ /^[ \t]*create[ \t]+(or[ \t]+replace[ \t]+)?(constraint[ \t]+)?trigger[ \t]/ ||
          tolower(t) ~ /^[ \t]*(update|delete|truncate)([ \t]|$)/) {
        begin(raw, t, m)
        if (t ~ /;$/) flush()
        next
      }
      print raw
    }
    END { if (open) flush() }' "$1"
}

# strip_comments <file>: the file with `--` tails and `/* */` spans blanked, line numbers kept.
strip_comments() {
  awk '{
    l = $0; out = ""
    while (l != "") {
      if (inc) { e = index(l, "*/"); if (!e) break; l = substr(l, e + 2); inc = 0; continue }
      s = index(l, "/*"); d = index(l, "--")
      if (d && (!s || d < s)) { out = out substr(l, 1, d - 1); break }
      if (s) { out = out substr(l, 1, s - 1) " "; l = substr(l, s + 2); inc = 1; continue }
      out = out l; break
    }
    print out
  }' "$1"
}

FLAT="$(mktemp)"
NOCOMMENT="$(mktemp)"
trap 'rm -f "$FLAT" "$NOCOMMENT"' EXIT

fail=0
SEEN=()   # `<file>:<class>` actually observed this run

for f in "$MIGRATIONS_DIR"/*.up.sql; do
  [ -e "$f" ] || continue
  base="$(basename "$f")"
  flatten "$f" > "$FLAT"
  strip_comments "$FLAT" > "$NOCOMMENT"
  fnlines="$(fn_body_lines "$FLAT" | tr '\n' ' ')"
  for i in "${!CLASSES[@]}"; do
    class="${CLASSES[$i]}"
    re="${MATCH[$i]}"
    exclude="${EXCLUDE[$i]}"
    # Whole-line SQL comments are dropped below so a `-- we used to DROP
    # COLUMN here` note never trips the gate.
    hits="$(grep_lines -nEi -- "$re" "$FLAT")"
    if [ -n "$hits" ]; then
      hits="$(printf '%s\n' "$hits" | grep_lines -vE -- '^[0-9]+:[[:space:]]*--')"
    fi
    if [ -n "$hits" ] && [ -n "$fnlines" ]; then
      hits="$(printf '%s\n' "$hits" | awk -F: -v skip="$fnlines" \
        'BEGIN { n = split(skip, a, " "); for (i = 1; i <= n; i++) in_fn[a[i]] = 1 } !($1 in in_fn)')"
    fi
    if [ -n "$exclude" ] && [ -n "$hits" ]; then
      # Judged on the statement, not on a trailing `--` comment.
      hits="$(printf '%s\n' "$hits" | awk -v ex="$exclude" '
        { l = toupper($0); cut = index(l, "--"); if (cut > 0) l = substr(l, 1, cut - 1) }
        l !~ ex')"
    fi
    [ -z "$hits" ] && continue
    SEEN+=("${base}:${class}")
    [ "$class" = dml ] && SEEN+=("${base}:dml-joined")
    guard="${GUARD[$i]}"
    if [ -n "$guard" ]; then
      # Guarded classes (`dml`) get no inline-marker escape: a comment is not
      # proof. Only the guard or the baseline clears a hit.
      # The guard must be a statement that runs at migrate time: not prose in
      # a comment, not the body of a function this file only defines.
      ghits="$(grep_lines -nEi -- "$guard" "$NOCOMMENT")"
      if [ -n "$ghits" ]; then
        ghits="$(printf '%s\n' "$ghits" | grep_lines -vE -- '^[0-9]+:[[:space:]]*--')"
      fi
      if [ -n "$ghits" ] && [ -n "$fnlines" ]; then
        ghits="$(printf '%s\n' "$ghits" | awk -F: -v skip="$fnlines" \
          'BEGIN { n = split(skip, a, " "); for (i = 1; i <= n; i++) in_fn[a[i]] = 1 } !($1 in in_fn)')"
      fi
      if [ -n "$ghits" ]; then
        continue
      fi
      if ! in_baseline "${base}:${class}" && ! { [ "$class" = dml ] && in_baseline "${base}:dml-joined"; }; then
        echo "lint-migration-compat ❌ ${f}: ${class} — not old-binary-safe (migrations/README.md rule 9):" >&2
        printf '%s\n' "$hits" | sed 's/^/  /' >&2
        fail=1
      fi
      continue
    fi
    # Inline `-- migration-compat:ok <reason>` is the two-release-dance escape.
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
    # An entry for a class this copy does not know is not stale: CI judges a
    # PR with the base copy of this gate, and the PR's baseline may already
    # name a class the PR adds.
    known=0
    # `dml-joined` keys a `dml` hit only visible once statements are joined
    # across lines; the key differs so a copy without that join skips it.
    [ "${e##*:}" = dml-joined ] && known=1
    for c in "${CLASSES[@]}"; do
      [ "${e##*:}" = "$c" ] && known=1 && break
    done
    [ "$known" -eq 0 ] && found=1
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
