#!/usr/bin/env bash
# check-commitlint-scope-parity.sh — verifies commitlint.config.js's
# scope-enum lists every scope actually used in recent commit subjects
# (type(scope): ...). Catches the drift where a new scope starts
# appearing in real commits (e.g. `docs(changelog)`, `fix(explorer)`)
# without a matching entry in the enum, which would make the next
# commit using that scope fail commitlint if/when it is wired up.
#
# Usage: check-commitlint-scope-parity.sh <config-file> [<git-log-ref-range>]
set -uo pipefail

CONFIG="${1:?usage: check-commitlint-scope-parity.sh <config-file> [<range>]}"
RANGE="${2:-HEAD~50..HEAD}"

if [ ! -f "$CONFIG" ]; then
  echo "check-commitlint-scope-parity: config not found: $CONFIG" >&2
  exit 2
fi

# Pull the quoted scopes out of the scope-enum array.
enum_scopes="$(sed -n "/'scope-enum'/,/\],\?$/p" "$CONFIG" | grep -oE "'[a-z0-9-]+'" | tr -d "'")"

missing=0
while IFS= read -r used; do
  [ -z "$used" ] && continue
  if ! grep -qx "$used" <<<"$enum_scopes"; then
    echo "missing scope in $CONFIG scope-enum: $used" >&2
    missing=$((missing + 1))
  fi
done < <(git log --format='%s' "$RANGE" 2>/dev/null | grep -oE '^[a-z]+\([a-z0-9-]+\)' | sed -E 's/^[a-z]+\(([a-z0-9-]+)\)/\1/' | sort -u)

if [ "$missing" -gt 0 ]; then
  exit 1
fi
exit 0
