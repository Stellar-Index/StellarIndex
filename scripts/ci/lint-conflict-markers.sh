#!/usr/bin/env bash
# lint-conflict-markers.sh — fails on a merge-conflict marker left in a text
# file: a line starting `<<<<<<< ` or `>>>>>>> `, and a bare `=======` line
# between the two. A bare `=======` elsewhere is a Markdown setext heading or
# an RST/ASCII rule and passes. Binary files are skipped (grep -I).
#
# No other gate catches this: lint-docs reads a marked doc as ordinary text
# and Go/shell files with markers only fail if the class that builds them runs.
#
# Usage:
#   lint-conflict-markers.sh              every tracked file
#   lint-conflict-markers.sh <file>...    just these (lint-changed.sh's mode)
#
# Exit 0 clean, 1 on a marker, 2 when there was nothing to scan.
set -euo pipefail

# Explicit paths resolve against the caller's work tree (lint-changed.sh runs
# from the changed files' repo); only the whole-tree mode needs the repo root.
files=()
if [ "$#" -gt 0 ]; then
  files=("$@")
else
  cd "$(dirname "$0")/../.."
  while IFS= read -r f; do files+=("$f"); done < <(git ls-files)
fi

# Tracked symlinks, directories (submodules) and deleted paths are not text
# files to scan.
scan=()
for f in ${files[@]+"${files[@]}"}; do
  if [ -f "$f" ] && [ ! -L "$f" ]; then scan+=("$f"); fi
done
if [ "${#scan[@]}" -eq 0 ]; then
  echo "lint-conflict-markers: FAIL — no files to scan (of ${#files[@]} requested)" >&2
  exit 2
fi

# grep exits 1 when nothing matches, which here is the pass case.
marked="$(printf '%s\0' "${scan[@]}" | xargs -0 grep -I -l -E '^(<<<<<<<|>>>>>>>) ' -- || true)"

if [ -z "$marked" ]; then
  echo "lint-conflict-markers: OK — ${#scan[@]} file(s) scanned of ${#files[@]} requested, no conflict marker"
  exit 0
fi

hits=()
while IFS= read -r f; do hits+=("$f"); done <<<"$marked"
awk '
  FNR == 1 { open = 0 }
  /^<<<<<<< / { open = 1; print FILENAME ":" FNR ": " $0; next }
  /^>>>>>>> / { open = 0; print FILENAME ":" FNR ": " $0; next }
  open && /^=======\r?$/ { print FILENAME ":" FNR ": " $0 }
' "${hits[@]}" | sed 's/^/  /' >&2
echo "lint-conflict-markers: FAIL — ${#hits[@]} file(s) carry merge-conflict markers (${#scan[@]} scanned); resolve the conflict and delete the marker lines" >&2
exit 1
