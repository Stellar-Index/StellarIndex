#!/usr/bin/env bash
# lint-migration-immutability.sh — freeze already-shipped migrations (W1-migrations-5).
#
# Why this gate exists (audit W1-migrations-5):
#
#   cmd/stellarindex-migrate/main.go is a thin wrapper over
#   golang-migrate/migrate: it opens `file://migrations` and calls
#   Up/Steps/Force. golang-migrate keys each migration ONLY on the
#   version integer parsed from the NNNN_ prefix — it records the applied
#   version in schema_migrations and never re-runs a number it has
#   already seen, and it never content-hashes the file. So an in-place
#   edit of an already-numbered migration file (after it was committed,
#   and possibly applied somewhere) is silently undetectable: an
#   environment that ran the OLD text stays permanently diverged from a
#   fresh DB built from the NEW text, and nothing complains.
#
#   This already happened. 0138_defindex_flows_harvest_direction.up.sql
#   was committed (8eb223fc) referencing a nonexistent table
#   `defindex_strategy_flows`, then edited IN PLACE (7b7089d3) to
#   `defindex_flows`. It was safe only by luck — the original would have
#   ERRORed, so no DB recorded it as applied. The hazard is the
#   normalized practice of editing a shipped migration at all.
#
# What this enforces:
#
#   Every migrations/*.sql file is pinned to its SHA-256 in
#   scripts/ci/migration-immutability.sha256 (the checksum baseline).
#   The gate recomputes each file's hash and:
#
#     * MUTATED — a baselined file whose content no longer matches its
#       recorded hash → FAIL. This is the core defense: changing a
#       shipped migration now requires consciously updating a visible,
#       reviewable checksum line instead of slipping by unnoticed.
#     * MISSING — a baselined file deleted or renamed → FAIL. Dropping a
#       shipped migration is the same divergence hazard in reverse.
#     * UNBASELINED — a migration file with no baseline entry → FAIL.
#       New migrations are APPEND-ONLY: allowed freely, but their
#       checksum must be added in the SAME PR (run `--write`), so the
#       baseline stays complete and the next edit of that file is caught.
#
#   The immutability contract is therefore: shipped migrations are frozen
#   (their baseline line never changes silently); new migrations only
#   ever get appended. Legitimately squashing/removing a not-yet-shipped
#   migration is still possible — it just becomes an explicit `--write`
#   in the diff rather than a silent drop.
#
# Maintenance:
#
#   Adding (or, deliberately, editing/removing) a migration → refresh the
#   baseline in the same change and commit it:
#
#       ./scripts/ci/lint-migration-immutability.sh --write
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

# Portable SHA-256: coreutils `sha256sum` (Linux/CI) or `shasum -a 256`
# (macOS default). Both print "<hash>  <path>"; we keep the hash only.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -- "$1" | awk '{print $1}'
  else
    shasum -a 256 -- "$1" | awk '{print $1}'
  fi
}

# Current-tree fingerprint: "<sha256>  <basename>" per *.sql, sorted by
# name (zero-padded NNNN prefixes → append order == sort order). Keyed on
# the BASENAME, not the path, so the baseline is independent of where the
# migrations dir lives (the self-test points MIGRATIONS_DIR at a tmpdir).
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
# Default FS (whitespace runs) makes $1 the hash and $2 the basename for
# both the one- and two-space forms; comment/blank lines are skipped.
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

Fix one of these ways:
  * Do NOT edit a shipped migration — write a NEW numbered migration that
    makes the correction forward, then refresh the baseline (--write).
  * If the file was genuinely never shipped/applied anywhere and editing
    it is safe, make the edit and refresh the baseline in the same PR:
        ./scripts/ci/lint-migration-immutability.sh --write
    The changed checksum line is then visible + reviewable in the diff.
EOF
  exit 1
fi

echo "✅ migration immutability lint passed ($(wc -l < "$MANIFEST" | tr -d '[:space:]') migrations frozen in $MANIFEST)."
