#!/usr/bin/env bash
# lint-numeric-mask.sh — no to_char with a numeric format mask. Postgres renders a value wider
# than a fixed-width mask such as 'FM999999990.00' as '#' characters, so a served number silently
# becomes "#########.##". Render numbers with ROUND(...)::text (see changePctText) instead.
#
# Scans non-test .go and every .sql under internal/, cmd/, pkg/, deploy/ and migrations/. For each
# to_char( call it takes the last top-level argument; a single-quoted literal made only of numeric
# mask elements (9 0 . , D G S PR MI, optional FM) with at least one 9 or 0 fails. Date/time masks
# ('YYYY-MM-DD', 'HH24:MI'), non-literal masks and whole-line -- or // comments pass.
set -euo pipefail

root="${LINT_ROOT:-$(git rev-parse --show-toplevel)}"

fail() {
  echo "lint-numeric-mask: FAIL — $*" >&2
  exit 2
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

files=()
for d in internal cmd pkg deploy migrations; do
  [ -d "$root/$d" ] || continue
  while IFS= read -r f; do
    files+=("$f")
  done < <(find "$root/$d" -type f \( -name '*.sql' -o \( -name '*.go' ! -name '*_test.go' \) \) -print | LC_ALL=C sort | xargs grep -il 'to_char' || true)
done
n_files="${#files[@]}"
[ "$n_files" -gt 0 ] || fail "no .go/.sql file under $root mentions to_char; the scan matched nothing, so it proves nothing."

status=0
# shellcheck disable=SC2016 # awk program, not shell expansion
LC_ALL=C awk -v rootdir="$root/" -v countfile="$tmp/count" '
function numeric(m,    u) {
  u = toupper(m)
  gsub(/FM|PR|MI/, "", u)
  return u ~ /^[90.,DGS ]+$/ && u ~ /[90]/
}
function scan(buf, file,    lb, pos, rest, off, i, c, depth, q, last, arg, line, n) {
  lb = tolower(buf)
  off = 0
  while (match(substr(lb, off + 1), /to_char[ \t\n]*\(/)) {
    pos = off + RSTART + RLENGTH  # first char after "("
    off = pos - 1
    depth = 1; q = 0; last = pos; n = length(buf)
    for (i = pos; i <= n; i++) {
      c = substr(buf, i, 1)
      if (c == "\047") { q = !q; continue }
      if (q) continue
      if (c == "(") depth++
      else if (c == ")") { if (--depth == 0) break }
      else if (c == "," && depth == 1) last = i + 1
    }
    rest = substr(buf, 1, pos)
    line = 1 + gsub(/\n/, "\n", rest)
    if (depth != 0) {
      bad++
      printf "%s:%d: to_char( call has no closing parenthesis; cannot check its mask\n", file, line > "/dev/stderr"
      return
    }
    checked++
    arg = substr(buf, last, i - last)
    gsub(/^[ \t\n]+|[ \t\n]+$/, "", arg)
    sub(/::[a-z]+$/, "", arg)
    if (arg ~ /^\047[^\047]*\047$/ && numeric(substr(arg, 2, length(arg) - 2))) {
      bad++
      printf "%s:%d: to_char numeric mask %s overflows to #; render with ROUND(...)::text\n", file, line, arg > "/dev/stderr"
    }
  }
}
FNR == 1 { if (NR > 1) scan(buf, file); buf = ""; file = FILENAME; sub("^" rootdir, "", file) }
# Whole-line comments are blanked (line numbers kept): prose naming a mask is not a call.
{ buf = buf (($0 ~ /^[ \t]*(--|\/\/)/) ? "" : $0) "\n" }
END { if (NR > 0) scan(buf, file); printf "%d %d\n", checked + 0, bad + 0 > countfile }
' "${files[@]}" || status=$?

[ "$status" -eq 0 ] && [ -s "$tmp/count" ] || fail "the scan did not complete (awk exit $status)."
read -r checked bad <"$tmp/count"
[ "$checked" -gt 0 ] || fail "0 to_char calls parsed in $n_files file(s); the scan matched nothing, so it proves nothing."
if [ "$bad" -ne 0 ]; then
  echo "lint-numeric-mask: FAIL — $bad of $checked to_char call(s) in $n_files file(s) use a numeric mask or could not be parsed." >&2
  exit 1
fi
echo "lint-numeric-mask: OK — $checked to_char call(s) across $n_files file(s) use no numeric mask."
