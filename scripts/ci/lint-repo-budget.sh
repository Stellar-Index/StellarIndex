#!/usr/bin/env bash
# lint-repo-budget.sh — regrowth tripwires for three kinds of maintenance debt
# that no other gate watches:
#
#   dated-md   A tracked *.md whose basename carries a -YYYY-MM date is a
#              point-in-time snapshot that goes stale while still being read.
#              Existing ones are baselined; docs/operations/sla-proof-
#              YYYY-MM-DD.md is exempt (the weekly SLA job writes one, and
#              check-sla-evidence.sh requires that filename shape).
#   go-lines   A hand-written .go file over 2,000 lines must be baselined, and
#              may not grow past its baselined ceiling. _test.go files and
#              files whose first line is a "Code generated ... DO NOT EDIT."
#              marker are out of scope.
#   comments   An ADDED Go comment may not carry a date or a review-ticket id:
#              both belong in the commit message and rot in the source.
#              Diff-only, so no existing line can fail. ADR-/SEP-/CAP-/RFC-/
#              ISO-/SHA-/UTF- references and the Go layout 2006-01-02 pass.
#
# A stale baseline entry (file gone, or no longer over the limit) also fails,
# so the baseline only shrinks; lint-baseline-growth.sh guards it growing.
#
# Usage:
#   scripts/ci/lint-repo-budget.sh           comments diffed over ${BASE_SHA:-origin/main}...HEAD
#   scripts/ci/lint-repo-budget.sh --staged  comments diffed over the index
#   scripts/ci/lint-repo-budget.sh --write   regenerate the baseline from the tree
#
# Runs against the git work tree it is invoked from. Exit 0 clean, 1 on a
# violation, 2 on a usage or discovery error.
set -euo pipefail

top="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "lint-repo-budget: not inside a git work tree" >&2; exit 2; }
cd "$top"

BASELINE="scripts/ci/lint-repo-budget.baseline"
GO_MAX=2000
ZERO_SHA="0000000000000000000000000000000000000000"

mode=range
write=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --staged) mode=staged ;;
    --write) write=1 ;;
    *) echo "lint-repo-budget: unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

git ls-files -- '*.md' | awk '
  { n = split($0, p, "/") }
  p[n] !~ /-20[0-9][0-9]-[01][0-9]/ { next }
  /^docs\/operations\/sla-proof-[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]\.md$/ { next }
  { print }' > "$tmp/md"

# "<lines> <path>" per hand-written non-test .go file, in one awk pass.
# shellcheck disable=SC2016 # an awk program, not a shell string
git ls-files -z -- '*.go' ':(exclude)*_test.go' | xargs -0 awk '
  function flush() { if (f != "" && !gen) print n, f; f = "" }
  FNR == 1 { flush(); f = FILENAME; gen = ($0 ~ /^\/\/ Code generated .* DO NOT EDIT\.$/) }
  { n = FNR }
  END { flush() }' > "$tmp/go"

if [ ! -s "$tmp/go" ]; then
  echo "lint-repo-budget: no hand-written Go files found — refusing to pass vacuously" >&2
  exit 2
fi

if [ "$write" -eq 1 ]; then
  {
    echo "# Regenerate with: scripts/ci/lint-repo-budget.sh --write"
    echo "# dated-md <path> | go-lines <path> <max-lines>"
    {
      awk '{ print "dated-md " $0 }' "$tmp/md"
      awk -v max="$GO_MAX" '$1 > max { print "go-lines " $2 " " $1 }' "$tmp/go"
    } | LC_ALL=C sort
  } > "$BASELINE"
  echo "lint-repo-budget: wrote ${BASELINE} ($(grep -vc '^#' "$BASELINE") entries)"
  exit 0
fi

grep -vE '^[[:space:]]*(#|$)' "$BASELINE" > "$tmp/baseline" 2>/dev/null || true

problems=0
tree_out="$(awk -v max="$GO_MAX" -v bl="$BASELINE" '
  FILENAME == ARGV[1] {
    if ($1 == "dated-md") md[$2] = 1
    else if ($1 == "go-lines") gl[$2] = $3
    next
  }
  FILENAME == ARGV[2] {
    seen_md[$0] = 1
    if (!($0 in md))
      print "dated-md " $0 ": a dated filename is a snapshot that goes stale; fold it into a living doc, or add it to " bl " with --write and a Baseline-Growth trailer"
    next
  }
  {
    seen_go[$2] = 1
    if ($2 in gl) {
      if ($1 + 0 <= max)
        print "stale go-lines " $2 ": " $1 " lines, no longer over the " max "-line limit; run --write to drop it"
      else if ($1 + 0 > gl[$2] + 0)
        print "go-lines " $2 ": " $1 " lines, over its baselined ceiling of " gl[$2] "; split the file rather than grow it"
    } else if ($1 + 0 > max) {
      print "go-lines " $2 ": " $1 " lines, over the " max "-line limit; split the file, or baseline it with --write and a Baseline-Growth trailer"
    }
  }
  END {
    for (p in md) if (!(p in seen_md))
      print "stale dated-md " p ": no longer a tracked dated doc; run --write to drop it"
    for (p in gl) if (!(p in seen_go))
      print "stale go-lines " p ": no longer a hand-written .go file; run --write to drop it"
  }' "$tmp/baseline" "$tmp/md" "$tmp/go")"
if [ -n "$tree_out" ]; then
  while IFS= read -r line; do
    echo "lint-repo-budget: ${line}" >&2
    problems=$((problems + 1))
  done <<<"$tree_out"
fi

# ── Added comments ──────────────────────────────────────────────────────────
scope=""
if [ "$mode" = staged ] && git rev-parse -q --verify MERGE_HEAD >/dev/null 2>&1; then
  # The index minus HEAD is the whole incoming parent, not this author's lines; CI diffs base...HEAD.
  echo "lint-repo-budget: comment check skipped — merge in progress (CI checks base...HEAD)"
  : > "$tmp/diff"
elif [ "$mode" = staged ]; then
  git diff --cached -U0 --no-color --no-ext-diff --src-prefix=a/ --dst-prefix=b/ -- '*.go' > "$tmp/diff"
  scope="the index"
else
  base="${BASE_SHA:-origin/main}"
  if [ "$base" = "$ZERO_SHA" ] || ! git rev-parse -q --verify "${base}^{commit}" >/dev/null 2>&1; then
    echo "lint-repo-budget: comment check skipped — base '${base}' does not resolve (set BASE_SHA, or fetch origin/main)"
    : > "$tmp/diff"
  else
    git diff -U0 --no-color --no-ext-diff --src-prefix=a/ --dst-prefix=b/ "${base}...HEAD" -- '*.go' > "$tmp/diff"
    scope="${base}...HEAD"
  fi
fi

comment_out="$(awk '
  function check(text,   s, c, why) {
    s = " " text
    if (!match(s, /[ \t]\/\//)) return
    c = " " substr(s, RSTART + RLENGTH) " "
    gsub(/(ADR|SEP|CAP|RFC|ISO|SHA|UTF)-[A-Za-z0-9]+/, " ", c)
    gsub(/2006-01-02/, " ", c)
    if (c ~ /[^A-Za-z0-9_]202[4-9]-[01][0-9][^A-Za-z0-9_]/) why = "a date"
    else if (c ~ /[^A-Za-z0-9_](F|CS|Q|T|CO|RLT|RSWP|CMA|CMB|HIS|DOC|YDC|DRY)-[0-9][0-9][0-9]?[0-9]?[^A-Za-z0-9_]/) why = "a ticket id"
    else return
    sub(/^[ \t]+/, "", text)
    print file ":" ln ": added comment carries " why "; put history and ids in the commit message and keep the comment to why: " text
  }
  /^diff --git / { hdr = 1; next }
  hdr && /^\+\+\+ / { file = substr($0, 7); next }
  /^@@ / { hdr = 0; match($0, /\+[0-9]+/); ln = substr($0, RSTART + 1, RLENGTH - 1) - 1; next }
  hdr { next }
  /^\+/ { ln++; added++; check(substr($0, 2)) }
  END { print "#added " added + 0 }' "$tmp/diff")"
added=0
while IFS= read -r line; do
  case "$line" in
    "#added "*) added="${line#\#added }" ;;
    "") ;;
    *) echo "lint-repo-budget: ${line}" >&2; problems=$((problems + 1)) ;;
  esac
done <<<"$comment_out"

n_md="$(grep -c '^dated-md ' "$tmp/baseline" || true)"
n_gl="$(grep -c '^go-lines ' "$tmp/baseline" || true)"
n_go="$(wc -l < "$tmp/go" | tr -d ' ')"
summary="${n_go} Go file(s) sized (${n_gl} baselined), ${n_md} dated doc(s) baselined, ${added} added Go line(s) scanned${scope:+ over ${scope}}"
if [ "$problems" -gt 0 ]; then
  echo "lint-repo-budget: FAIL — ${problems} violation(s); ${summary}" >&2
  exit 1
fi
echo "lint-repo-budget: OK — ${summary}"
