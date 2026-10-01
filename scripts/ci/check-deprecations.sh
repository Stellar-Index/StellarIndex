#!/usr/bin/env bash
# check-deprecations.sh — every `// Deprecated:` Godoc paragraph in tracked
# Go must name its scheduled removal version (vX.Y.Z), per
# docs/engineering-standards.md §2.4 item 5. The paragraph is the marker
# line plus the contiguous non-blank `//` lines after it. Files carrying a
# `// Code generated … DO NOT EDIT.` banner are skipped: their text is not
# ours to schedule.
#
# Usage: check-deprecations.sh [ROOT]   (ROOT defaults to the repo root; the
# self-test points it at a fixture repo, same convention as lint-lexicon.sh).
# Exit 0 clean, 1 on any violation, 2 if ROOT tracks no Go at all.
set -euo pipefail
cd "$(dirname "$0")/../.."

root="${1:-.}"
cd "$root"

# git ls-files, not a tree walk: untracked checkouts (node_modules, agent
# worktrees) would otherwise be scanned as if they were ours.
n_files=$(git ls-files -- '*.go' | wc -l | tr -d ' ')
if [ "$n_files" -eq 0 ]; then
  echo "check-deprecations: no tracked .go files under $root — refusing to pass vacuously." >&2
  exit 2
fi

# shellcheck disable=SC2016 # awk program, not shell expansion
hits=$(git ls-files -z -- '*.go' | xargs -0 awk '
  function close_para() {
    if (inpara && !has)
      out = out pfile ":" start ": `// Deprecated:` names no removal version (vX.Y.Z)\n"
    inpara = 0
  }
  function flush() {
    if (!gen) printf "%s", out
    out = ""; gen = 0
  }
  FNR == 1 { close_para(); flush() }
  /^\/\/ Code generated .* DO NOT EDIT\.$/ { gen = 1 }
  {
    marker = ($0 ~ /^[ \t]*\/\/[ \t]*Deprecated:/)
    if (inpara && !marker && $0 ~ /^[ \t]*\/\/[ \t]*[^ \t]/) {
      if ($0 ~ /v[0-9]+\.[0-9]+\.[0-9]+/) has = 1
      next
    }
    close_para()
    if (marker) {
      inpara = 1; pfile = FILENAME; start = FNR
      has = ($0 ~ /v[0-9]+\.[0-9]+\.[0-9]+/)
    }
  }
  END { close_para(); flush() }
')

if [ -n "$hits" ]; then
  echo "DEPRECATION: every deprecation needs a scheduled removal version from day one"
  echo "             (docs/engineering-standards.md §2.4), e.g. \"Will be removed in v3.0.0.\""
  printf '%s\n' "$hits" | sed 's/^/  /'
  exit 1
fi
echo "check-deprecations: OK ($n_files Go files; every Deprecated: names a removal version)."
