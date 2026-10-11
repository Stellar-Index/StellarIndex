#!/usr/bin/env bash
# lint-migration-immutability.sh — freeze already-shipped migrations.
#
# Why: cmd/stellarindex-migrate wraps golang-migrate, which keys a migration
# ONLY on the version integer in the NNNN_ prefix. It records applied versions
# in schema_migrations, never re-runs a number it has seen, and never
# content-hashes the file. An in-place edit of a numbered migration is thus
# silently undetectable: an environment that ran the OLD text stays diverged
# from a fresh DB built from the NEW text. This has happened once
# (0138_defindex_flows_harvest_direction.up.sql was edited from a nonexistent
# table to `defindex_flows`), safe only because the original would have ERRORed
# and so was never recorded as applied.
#
# What it enforces: every migrations/*.sql is pinned to its SHA-256 in
# scripts/ci/migration-immutability.sha256. Each file's hash is recomputed:
#   * MUTATED    baselined file whose content no longer matches -> FAIL. Changing
#                a shipped migration must move a visible, reviewable checksum line.
#   * MISSING    baselined file deleted or renamed -> FAIL (same hazard in reverse).
#   * UNBASELINED  migration with no baseline entry -> FAIL. New migrations are
#                append-only, but their checksum is added in the SAME PR (`--write`)
#                so the next edit of that file is caught.
# Squashing or removing a not-yet-shipped migration stays possible; it becomes
# an explicit `--write` in the diff rather than a silent drop.
#
# WHAT MAY BE EDITED (migrations/README.md "Amending a shipped migration" has
# the long form): a shipped migration's UP body is immutable; its DOWN body and
# header COMMENTS may be corrected via the baseline refresh (`--write`), said so
# in the commit message; anything stored in the database (COMMENT ON, defaults)
# needs a new migration. The one UP exception is the README's "Neutralising an
# unapplied migration".
#
# The gate deliberately cannot tell those apart: it hashes bytes and cannot know
# whether a changed line was above BEGIN;. It is the FORCING FUNCTION, not the
# judgement: even a correctable edit must move a visible checksum line, which
# puts the decision in front of a reviewer. Reviewers enforce the rule; this
# script guarantees they get the chance to.
#
# Maintenance: adding (or, per the rule above, editing/removing) a migration
# means refreshing the baseline in the same change and committing it:
#   ./scripts/ci/lint-migration-immutability.sh --write
#
# Usage:
#   bash scripts/ci/lint-migration-immutability.sh            # check (CI + verify.sh)
#   bash scripts/ci/lint-migration-immutability.sh --write    # (re)generate the baseline
set -euo pipefail

cd "$(dirname "$0")/../.."

MIGRATIONS_DIR="${MIGRATIONS_DIR:-migrations}"
MANIFEST="${MIGRATION_IMMUTABILITY_MANIFEST:-scripts/ci/migration-immutability.sha256}"

WRITE=0
for arg in "$@"; do
  case "$arg" in
    --write|--update) WRITE=1 ;;
    *)
      echo "lint-migration-immutability ❌ unknown argument: $arg" >&2
      exit 2
      ;;
  esac
done

if [ ! -d "$MIGRATIONS_DIR" ]; then
  echo "lint-migration-immutability ❌ migrations dir not found: $MIGRATIONS_DIR" >&2
  exit 1
fi

# Portable SHA-256: coreutils `sha256sum` (Linux/CI) or `shasum -a 256` (macOS);
# only the hash is kept.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -- "$1" | awk '{print $1}'
  else
    shasum -a 256 -- "$1" | awk '{print $1}'
  fi
}

# Current-tree fingerprint: "<sha256>  <basename>" per *.sql, sorted by name
# (zero-padded prefixes: append order == sort order). Keyed on the BASENAME so the
# baseline is independent of the migrations dir (the self-test uses a tmpdir).
CUR="$(mktemp)"
trap 'rm -f "$CUR"' EXIT
for f in "$MIGRATIONS_DIR"/*.sql; do
  [ -e "$f" ] || continue
  printf '%s  %s\n' "$(sha256_of "$f")" "$(basename "$f")"
done | LC_ALL=C sort -k2 > "$CUR"

if [ "$WRITE" -eq 1 ]; then
  cp "$CUR" "$MANIFEST"
  chmod 0644 "$MANIFEST"   # deterministic mode; mktemp source is 0600
  echo "✅ wrote $(wc -l < "$MANIFEST" | tr -d '[:space:]') migration checksums to $MANIFEST"
  exit 0
fi

if [ ! -f "$MANIFEST" ]; then
  echo "lint-migration-immutability ❌ checksum baseline not found: $MANIFEST" >&2
  echo "    Generate it once and commit it: $0 --write" >&2
  exit 1
fi

# Single awk pass over baseline (first) then current tree (second):
#   NEW  <name>                 — file present, no baseline entry
#   MUT  <name> <want> <got>    — baseline entry differs from file
#   GONE <name>                 — baseline entry with no file
# Default FS makes $1 the hash and $2 the basename for both the one- and
# two-space forms; comment/blank lines are skipped.
report="$(
  awk '
    FNR == NR {
      if ($0 ~ /^[[:space:]]*$/ || $0 ~ /^[[:space:]]*#/) next
      want[$2] = $1
      next
    }
    {
      if ($0 ~ /^[[:space:]]*$/) next
      cur[$2] = 1
      if (!($2 in want))       { print "NEW\t"  $2 }
      else if (want[$2] != $1) { print "MUT\t"  $2 "\t" want[$2] "\t" $1 }
    }
    END {
      for (n in want) if (!(n in cur)) print "GONE\t" n
    }
  ' "$MANIFEST" "$CUR"
)"

fail=0
if [ -n "$report" ]; then
  while IFS=$'\t' read -r kind name want got; do
    [ -n "$kind" ] || continue
    case "$kind" in
      NEW)
        echo "lint-migration-immutability ❌ ${MIGRATIONS_DIR}/${name}: not in the checksum baseline ($MANIFEST)." >&2
        echo "    New migrations are append-only — add its checksum in this PR: $0 --write" >&2
        fail=1
        ;;
      MUT)
        echo "lint-migration-immutability ❌ ${MIGRATIONS_DIR}/${name}: content changed since it was baselined — an already-shipped migration is frozen (W1-migrations-5)." >&2
        echo "    baselined ${want}" >&2
        echo "    current   ${got}" >&2
        fail=1
        ;;
      GONE)
        echo "lint-migration-immutability ❌ baselined migration missing from tree: ${name} (deleted or renamed)." >&2
        echo "    If this removal is intentional, refresh the baseline in this PR: $0 --write" >&2
        fail=1
        ;;
    esac
  done <<< "$report"
fi

if [ "$fail" -ne 0 ]; then
  cat >&2 <<EOF

golang-migrate keys migrations on the version integer only — it never
re-runs an applied number and never content-hashes (cmd/stellarindex-migrate).
So editing a shipped migration in place leaves every environment that ran
the old text permanently diverged from a fresh DB, silently.

Fix one of these ways (migrations/README.md "Amending a shipped migration"):
  * The change touches a shipped up.sql's SQL, or anything STORED in the
    database (COMMENT ON text, a column DEFAULT) — do NOT edit the file.
    Write a NEW numbered migration that makes the correction forward,
    then refresh the baseline (--write). Editing the file only changes
    what a FRESH database gets; every applied environment stays wrong.
  * The change is a HEADER COMMENT on an up.sql, or a down.sql body —
    that is correctable in place. Make the edit, refresh the baseline in
    the same commit, and say so in the commit message:
        ./scripts/ci/lint-migration-immutability.sh --write
    The changed checksum line is then visible + reviewable in the diff.
  * The file was genuinely never shipped/applied anywhere — same
    mechanics as above.
EOF
  exit 1
fi

echo "✅ migration immutability lint passed ($(wc -l < "$MANIFEST" | tr -d '[:space:]') migrations frozen in $MANIFEST)."
