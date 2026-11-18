#!/usr/bin/env bash
# shellcheck disable=SC2016 # the awk program is single-quoted on purpose
# A literal SQL `LIMIT <N>` (N >= 100) in internal/storage and internal/platform Go code silently truncates a read the caller may present as the whole
# set. Each must carry `cap-ok: <how truncation is surfaced>` on its line or the line above, or sit in the shrink-only baseline.
# Usage: lint-unflagged-limit.sh [--root DIR] [--record]
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
RECORD=0
while [ $# -gt 0 ]; do
  case "$1" in
    --root) ROOT="$2"; shift 2 ;;
    --record) RECORD=1; shift ;;
    *) echo "lint-unflagged-limit: unknown arg $1" >&2; exit 2 ;;
  esac
done
BASELINE="$ROOT/scripts/ci/lint-unflagged-limit.baseline"
[ -d "$ROOT/internal/storage" ] || { echo "lint-unflagged-limit: no $ROOT/internal/storage" >&2; exit 2; }

# Emits "path<TAB>trimmed line" for every unannotated capped LIMIT.
found="$(cd "$ROOT" && find internal/storage internal/platform -name '*.go' ! -name '*_test.go' -print0 | sort -z | xargs -0 awk '
  FNR == 1 { prev = "" }
  {
    line = $0
    if (line !~ /^[ \t]*\/\// && match(line, /LIMIT[ \t]+[0-9]+/)) {
      n = substr(line, RSTART, RLENGTH); sub(/LIMIT[ \t]+/, "", n)
      if (n + 0 >= 100 && line !~ /cap-ok:/ && prev !~ /cap-ok:/) {
        t = line; gsub(/^[ \t]+|[ \t]+$/, "", t)
        print FILENAME "\t" t
      }
    }
    prev = line
  }')"

if [ "$RECORD" -eq 1 ]; then
  { echo "# lint-unflagged-limit debt: path<TAB>trimmed line. Shrink-only; annotate with cap-ok: or surface the cap, then delete the line."
    [ -n "$found" ] && printf '%s\n' "$found"; } > "$BASELINE"
  echo "lint-unflagged-limit: recorded $(printf '%s' "$found" | grep -c . ) site(s)"
  exit 0
fi

base="$( [ -f "$BASELINE" ] && grep -v '^#' "$BASELINE" | grep . )"
new="$(comm -23 <(printf '%s\n' "$found" | grep . | sort -u) <(printf '%s\n' "$base" | grep . | sort -u))"
stale="$(comm -13 <(printf '%s\n' "$found" | grep . | sort -u) <(printf '%s\n' "$base" | grep . | sort -u))"
rc=0
if [ -n "$new" ]; then
  echo "lint-unflagged-limit: capped LIMIT without a cap-ok: note (say how the caller surfaces truncation, or over-fetch and flag it):"
  printf '%s\n' "$new" | sed 's/^/  /'
  rc=1
fi
if [ -n "$stale" ]; then
  echo "lint-unflagged-limit: baseline entries no longer found; delete them from $BASELINE:"
  printf '%s\n' "$stale" | sed 's/^/  /'
  rc=1
fi
total="$(printf '%s' "$found" | grep -c .)"
[ "$rc" -eq 0 ] && echo "lint-unflagged-limit: ok ($total baselined site(s), 0 new)"
exit "$rc"
