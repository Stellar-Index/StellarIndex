#!/usr/bin/env bash
# lint-shell-sigpipe — refuse `… | head` inside a pipefail shell script.
#
# THE BUG CLASS (#475). Under `set -o pipefail`, a pipeline whose LAST
# stage stops reading early is a coin flip. `head -n N` is the obvious
# form, but any early-exit consumer does it: `awk '...{exit}'`,
# `sed '...q'`, `grep -m N`, `grep -q`. The first version of this lint
# matched only `head`, and an independent review found `awk ... exit` five
# lines from the fix it shipped with — a lint that catches one spelling of a
# class licenses the others. The next version named `grep -m N` and not
# `grep -q`, which is the spelling the tree actually writes: 44 sites over
# the four roots, among them the substring assertion at the heart of ten
# gate self-tests, where `if ! … | grep -q "$want"` reports a substring
# MISSING from output that contains it. Example of the obvious form:
#
#     mc ls … | sort | head -n 4 > out      # looks fine, fails ~1 run in 3
#
# `head` exits the moment it has N lines and closes the pipe. systemd's
# default IgnoreSIGPIPE=yes turns the resulting signal into a plain EPIPE,
# so the upstream process does not die silently — it PRINTS
# "write failed: 'standard output': Broken pipe" and exits non-zero.
# pipefail promotes that to the pipeline's status, and (with `set -e`) it
# kills the script. It only bites when the producer writes more than the
# 64 KiB pipe buffer before `head` is done, which is why it presents as a
# random, unreproducible failure: three of r1's galexie-archive-fill runs,
# the last on 2026-09-02 18:19:35 UTC, exited 2 exactly this way.
#
# THE FIX is always the same shape — land the output, then slice it:
#
#     mc ls … | sort > /tmp/x.txt
#     head -n 4 /tmp/x.txt > out
#
# A consumer that reads to EOF is as safe and usually shorter: `sed -n 1p`,
# `sed -n '1,12p'`, `awk 'NR==1'` with no `exit`, or slicing a captured
# value with "${var%%$'\n'*}" / "${var:0:200}".
#
# For a match test the landing place is a value, and a here-string feeds it
# with no pipe at all — bash writes the whole string before grep starts, via
# a temp file once it outgrows a pipe buffer:
#
#     grep -qF -- "$want" <<<"$OUT"
#     grep -q 'GNU tar' <<<"$(tar --version 2>/dev/null)"
#
# ESCAPE HATCH: put `# sigpipe-ok: <reason>` on the line, or anywhere in
# the comment block directly above it, when the producer provably writes
# less than a pipe buffer
# (a `pgrep` result, a single grepped line). State the reason; "it's
# fine" is not one.
set -uo pipefail

# scripts/ci is a default root because the gate scripts and their self-tests
# are themselves pipefail shell, and the lint never looked at its own
# directory: a `printf … | head -1` in check-public-dataset-test.sh took down
# a full local verify with "printf: write error: Broken pipe" on 2026-09-03,
# while this gate reported OK over the three roots it did scan.
roots=("${@:-configs/ansible/roles/archival-node/files scripts/ops scripts/dev scripts/ci}")
# shellcheck disable=SC2206
read -r -a roots <<<"${roots[*]}"

# The early-exit consumer set, as one ERE:
#
#   head            stops at N lines
#   awk … exit      stops at the first match
#   sed … q         stops at the first match
#   grep -m N       stops at N matches
#   grep -q/-l/-L   stops at the FIRST match — the same shape, and the one
#                   the tree actually writes. `seq 1 400000 | grep -q 1`
#                   exits 141 under pipefail: the match SUCCEEDED and the
#                   pipeline still reports failure, so `if ! … | grep -q x`
#                   silently inverts on any producer past 64 KiB. -L reads
#                   to EOF under BSD grep but stops at the first match under
#                   GNU grep, which is what CI and r1 run.
#
# Two anchors keep the match honest. A stage only counts when a REAL pipe
# feeds it — `[^|]\|` keeps `cmd || grep -q x` out, where grep reads a file
# and there is no upstream to kill. And the flag is a whole token in THIS
# stage: `[^|&;()]*` stops the scan at the next `&&`, `;` or `)` so that a
# later `&& ! grep -q x` on the same line is not read as this pipe's
# consumer. The cluster form (`-qE`, `-Fxq`, `-qxF`) is matched too.
early_exit_re='(^|[^|])\|[[:space:]]*(head([[:space:]]|$)|awk[^|]*\<exit\>|sed[^|]*\<q\>|grep[^|&;()]*[[:space:]]-([A-Za-z]*[qlL][A-Za-z]*([[:space:]]|$)|m[[:space:]]*[0-9]|-(quiet|silent|files-with-matches|files-without-match|max-count)([=[:space:]]|$)))'

fail=0
checked=0
while IFS= read -r f; do
  [ -f "$f" ] || continue
  grep -qE '^set .*(pipefail|-e)' "$f" || continue   # only pipefail/-e scripts can be killed by it
  checked=$((checked + 1))
  while IFS=: read -r line _; do
    [ -n "$line" ] || continue
    body=$(sed -n "${line}p" "$f")
    # Look back over the whole contiguous comment block above the line, so
    # the marker can sit anywhere in a multi-line justification.
    prev=""
    back=$((line - 1))
    while [ "$back" -ge 1 ]; do
      pl=$(sed -n "${back}p" "$f")
      case "$(printf '%s' "$pl" | sed 's/^[[:space:]]*//')" in
        '#'*) prev="$prev$pl"; back=$((back - 1)) ;;
        *) break ;;
      esac
    done
    # A comment ABOUT the pattern is not an instance of it — this lint's
    # own explanatory comments, and the fixed sites', quote the bad shape.
    case "$(printf '%s' "$body" | sed 's/^[[:space:]]*//')" in
      '#'*) continue ;;
    esac
    case "$body$prev" in
      *sigpipe-ok:*) continue ;;
    esac
    printf 'lint-shell-sigpipe: %s:%s pipes into an EARLY-EXIT consumer (head / awk exit / sed q / grep -q, -l, -L, -m) under pipefail — land the output in a file or variable first, then slice it\n' "$f" "$line"
    printf '    %s\n' "$body"
    fail=$((fail + 1))
  done < <(grep -nE "$early_exit_re" "$f" | cut -d: -f1 | sed 's/$/:/')
done < <(find "${roots[@]}" -type f -name '*.sh' 2>/dev/null | sort)

if [ "$checked" -eq 0 ]; then
  echo "lint-shell-sigpipe: FAIL — no pipefail scripts found under ${roots[*]}; the gate would be vacuous" >&2
  exit 1
fi
if [ "$fail" -gt 0 ]; then
  echo "lint-shell-sigpipe: FAIL — $fail early-exit-consumer site(s) in $checked pipefail script(s)" >&2
  exit 1
fi
echo "lint-shell-sigpipe: OK — $checked pipefail script(s), no unguarded pipe into an early-exit consumer"
