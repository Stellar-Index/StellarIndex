#!/usr/bin/env bash
# lint-ops-write-flag.sh — a write-gated stellarindex-ops command that docs/operations tells an
# operator to run must carry -write: without it the command only previews, so the step silently
# does nothing. The gated set is parsed from the gate() entries of subcommandClasses in
# cmd/stellarindex-ops/write_gate_class_test.go, the map TestSubcommandWriteGateMatchesItsClass
# holds every command to (it also proves each gated command accepts -write and -dry-run).
#
# Scans fenced code (joining `\` continuations) and inline code spans. An invocation passes when it
# has no arguments (a bare mention), carries -write, is -h/-help/--help, or uses a read-only mode
# listed in RO below.
# A deliberate preview says so with the command's own -dry-run flag; prose does not count.
set -euo pipefail

root="${LINT_ROOT:-$(git rev-parse --show-toplevel)}"
classes="$root/cmd/stellarindex-ops/write_gate_class_test.go"
docs_dir="$root/docs/operations"

fail() {
  echo "lint-ops-write-flag: FAIL — $*" >&2
  exit 2
}

[ -f "$classes" ] || fail "$classes not found; the gated command set cannot be read."
[ -d "$docs_dir" ] || fail "$docs_dir not found."
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
sed -n 's/^[[:space:]]*"\([^"]*\)":[[:space:]]*gate(),.*$/\1/p' "$classes" >"$tmp/gated"
n_gated="$(wc -l <"$tmp/gated" | tr -d ' ')"
[ "$n_gated" -gt 0 ] || fail "no gate() entries parsed from $classes; the map's shape changed."

files=()
while IFS= read -r f; do
  files+=("$f")
done < <(find "$docs_dir" -type f -name '*.md' | LC_ALL=C sort)
n_files="${#files[@]}"
[ "$n_files" -gt 0 ] || fail "no markdown under $docs_dir."

status=0
# shellcheck disable=SC2016 # awk program, not shell expansion
awk -v gatedfile="$tmp/gated" -v rootdir="$root/" -v countfile="$tmp/count" '
BEGIN {
  while ((getline g < gatedfile) > 0) G[g] = 1
  OK["-write"] = 1; OK["--write"] = 1; OK["-write=true"] = 1; OK["--write=true"] = 1
  OK["-dry-run"] = 1; OK["--dry-run"] = 1; OK["-dry-run=true"] = 1; OK["--dry-run=true"] = 1
  OK["-h"] = 1; OK["-help"] = 1; OK["--help"] = 1
  # Read-only modes that exit before the gate matters (their flag help says so).
  RO["trades-cagg-refresh -size"] = 1; RO["freeze-unfreeze -list"] = 1; RO["source-exclude -list"] = 1
  checked = 0; bad = 0
}
FNR == 1 { fence = 0; pend = ""; file = FILENAME; sub("^" rootdir, "", file) }
function check(s, where,    rest, cut, n, t, i, key, start, ok) {
  while (match(s, /stellarindex-ops[ \t]+/)) {
    rest = substr(s, RSTART + RLENGTH)
    s = rest
    # <a|b> placeholders and quoted values hold | and ; that are not shell operators; a quote left
    # unpaired closes a wrapper such as ssh host '...'.
    gsub(/<[^<>]*>/, "X", rest)
    gsub(/"[^"]*"/, "Q", rest)
    gsub(/\047[^\047]*\047/, "Q", rest)
    cut = match(rest, /[|;&`#)"\047]/)
    if (cut) rest = substr(rest, 1, cut - 1)
    n = split(rest, t, /[ \t]+/)
    key = ""
    if (n >= 2 && ((t[1] " " t[2]) in G)) { key = t[1] " " t[2]; start = 3 }
    else if (n >= 1 && (t[1] in G)) { key = t[1]; start = 2 }
    if (key == "" || start > n || t[start] == "") continue
    checked++
    ok = 0
    for (i = start; i <= n; i++) if ((t[i] in OK) || ((key " " t[i]) in RO)) { ok = 1; break }
    if (!ok) {
      bad++
      printf "%s: stellarindex-ops %s has arguments but neither -write nor -dry-run: %s\n", where, key, rest > "/dev/stderr"
    }
  }
}
/^[ \t]*```/ { if (pend != "") { check(pend, pwhere); pend = "" } fence = !fence; next }
fence {
  l = $0
  if (pend == "") pwhere = file ":" FNR
  if (l ~ /\\[ \t]*$/) { sub(/\\[ \t]*$/, " ", l); pend = pend l; next }
  check(pend l, pwhere)
  pend = ""
  next
}
{
  m = split($0, part, "`")
  for (i = 2; i < m; i += 2) check(part[i], file ":" FNR)
}
END { printf "%d %d\n", checked, bad > countfile }
' "${files[@]}" || status=$?

[ "$status" -eq 0 ] && [ -s "$tmp/count" ] || fail "the docs scan did not complete (awk exit $status)."
read -r checked bad <"$tmp/count"
[ "$checked" -gt 0 ] || fail "0 gated invocations found in $n_files file(s); the scan matched nothing, so it proves nothing."
if [ "$bad" -ne 0 ]; then
  echo "lint-ops-write-flag: FAIL — $bad of $checked gated invocation(s) in $n_files file(s) lack -write or -dry-run ($n_gated gated subcommand(s)). Add -write where the step mutates, -dry-run where it previews." >&2
  exit 1
fi
echo "lint-ops-write-flag: OK — $checked gated invocation(s) across $n_files docs/operations file(s) carry -write, -dry-run or a read-only mode; $n_gated gated subcommand(s) from subcommandClasses."
