#!/usr/bin/env bash
# lint-agents-file.sh — keep AGENTS.md operative rather than expository.
#
# AGENTS.md is the file an agent or a new contributor reads COLD and has
# to retain. Its value is inversely proportional to its length: a rule
# buried on line 600 is a rule nobody follows.
#
# It reached 829 lines by accumulating reference material — a 112-line
# directory listing, 194 lines of domain narrative, 183 lines of task
# recipes. All of it was true and none of it was operative. Comparable
# projects keep this file between 18 and 161 lines (uv 27, kubernetes 38,
# firecrawl 18) because they keep only rules, commands and constraints in
# it, and file the explanations as docs.
#
# What this gate asserts:
#
#   1. LENGTH. A hard ceiling, so the file cannot silently re-accumulate.
#   2. NO REFERENCE SECTIONS. A directory listing, docs index or changelog
#      inside AGENTS.md means the split is being undone.
#   3. IT STILL CONTAINS RULES. A file that has been emptied of its
#      imperative vocabulary passes 1 and 2 while being useless — the
#      vacuous-gate shape this repo keeps finding. So a minimum count of
#      ALWAYS/NEVER/PREFER/AVOID directives is required.
#   4. NO SELF-LINKS. A markdown link from a file to itself is always a
#      rename artefact; three of them survived the CLAUDE.md -> AGENTS.md
#      rename and pointed readers in a circle.
#   5. THE POINTERS RESOLVE. Every relative link out of AGENTS.md must
#      exist on disk, or the split has stranded the reference material.
#
# Exit code = number of failures.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

FILE="AGENTS.md"
MAX_LINES=175
MIN_DIRECTIVES=25
FAILURES=0

err() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILURES=$((FAILURES + 1)); }
ok()  { printf '  \033[32mok\033[0m   %s\n' "$1"; }

if [ ! -f "$FILE" ]; then
  echo "lint-agents-file: $FILE is missing — it is the entry point every other doc assumes"
  exit 1
fi

# 1. Length ceiling.
lines=$(wc -l < "$FILE" | tr -d ' ')
if [ "$lines" -gt "$MAX_LINES" ]; then
  err "$FILE is $lines lines, ceiling is $MAX_LINES. Reference material belongs in docs/ — see docs/architecture/repo-map.md, docs/architecture/domain-traps.md and docs/contributing/task-recipes.md for where the last three sections went."
else
  ok "$FILE is $lines lines (ceiling $MAX_LINES)"
fi

# 2. No reference sections creeping back in.
#
# Headings are read OUTSIDE fenced blocks only: this file's main job is
# holding commands, and a shell comment such as `# changelog entry goes
# under [Unreleased]` inside a ```sh block is not a heading. Matching it
# made the gate red on a legitimate file.
#
# The synonym list is deliberately broad and matches ANY heading depth.
# A narrow list is how the section comes back under a new name: "##
# Directory structure" and "#### Repo map" both passed an earlier cut.
ref_hits=0
in_fence=0
while IFS= read -r line; do
  case "$line" in '```'*|'~~~'*) in_fence=$((1 - in_fence)); continue ;; esac
  [ "$in_fence" -eq 1 ] && continue
  case "$line" in '#'*) : ;; *) continue ;; esac
  heading="$(printf '%s' "$line" | sed -E 's/^#+[[:space:]]*//' | tr '[:upper:]' '[:lower:]')"
  case "$heading" in
    *"repo map"*|*"repository map"*|*"repo layout"*|*"directory structure"*|\
    *"directory layout"*|*"file layout"*|*"repo structure"*|*"where things live"*|\
    *"docs index"*|*"documentation index"*|*"task recipes"*|*"common recipes"*|\
    *"changelog"*|*"release notes"*)
      err "$FILE has a reference section: '$heading'. That is docs/ material — link to it. See docs/architecture/repo-map.md, docs/architecture/domain-traps.md and docs/contributing/task-recipes.md for where the originals went."
      ref_hits=$((ref_hits + 1)) ;;
  esac
done < "$FILE"
[ "$ref_hits" -eq 0 ] && ok "no reference sections"

# 3. It still contains rules. Guards against an empty file passing.
directives=$(grep -cE '\b(ALWAYS|NEVER|PREFER|AVOID)\b' "$FILE" || true)
if [ "$directives" -lt "$MIN_DIRECTIVES" ]; then
  err "$FILE has $directives ALWAYS/NEVER/PREFER/AVOID directives, minimum is $MIN_DIRECTIVES. A short file with no rules in it passes a length check while being useless."
else
  ok "$directives imperative directives (minimum $MIN_DIRECTIVES)"
fi

# 4. No self-referential links.
selflinks=$(grep -oE '\]\(\.?/?AGENTS\.md[^)]*\)' "$FILE" | wc -l | tr -d ' ')
if [ "$selflinks" -gt 0 ]; then
  err "$FILE links to itself $selflinks time(s) — always a rename artefact; the reader is sent in a circle"
else
  ok "no self-referential links"
fi

# 5. Every relative link out of it resolves.
missing=0
while IFS= read -r target; do
  [ -z "$target" ] && continue
  path="${target%%#*}"
  [ -z "$path" ] && continue
  case "$path" in http*|mailto:*) continue ;; esac
  if [ ! -e "$path" ]; then
    err "$FILE links to '$path', which does not exist"
    missing=$((missing + 1))
  fi
done < <(grep -oE '\]\([^)]+\)' "$FILE" | sed -E 's/^\]\(//; s/\)$//' | grep -vE '^(https?:|mailto:|#)')
[ "$missing" -eq 0 ] && ok "every relative link resolves"

echo
if [ "$FAILURES" -eq 0 ]; then
  echo "lint-agents-file: OK — $FILE is $lines lines with $directives directives"
else
  echo "lint-agents-file: $FAILURES failure(s)"
fi
exit "$FAILURES"
