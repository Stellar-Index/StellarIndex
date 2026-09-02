#!/usr/bin/env bash
# lint-shell-sigpipe — refuse `… | head` inside a pipefail shell script.
#
# THE BUG CLASS (#475). Under `set -o pipefail`, a pipeline ending in
# `head -n N` is a coin flip:
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
# ESCAPE HATCH: put `# sigpipe-ok: <reason>` on the line, or anywhere in
# the comment block directly above it, when the producer provably writes
# less than a pipe buffer
# (a `pgrep` result, a single grepped line). State the reason; "it's
# fine" is not one.
set -uo pipefail

roots=("${@:-configs/ansible/roles/archival-node/files scripts/ops scripts/dev}")
# shellcheck disable=SC2206
read -r -a roots <<<"${roots[*]}"

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
    printf 'lint-shell-sigpipe: %s:%s pipes into head under pipefail — write to a file, then head the file\n' "$f" "$line"
    printf '    %s\n' "$body"
    fail=$((fail + 1))
  done < <(grep -nE '\|[[:space:]]*head([[:space:]]|$)' "$f" | cut -d: -f1 | sed 's/$/:/')
done < <(find "${roots[@]}" -type f -name '*.sh' 2>/dev/null | sort)

if [ "$checked" -eq 0 ]; then
  echo "lint-shell-sigpipe: FAIL — no pipefail scripts found under ${roots[*]}; the gate would be vacuous" >&2
  exit 1
fi
if [ "$fail" -gt 0 ]; then
  echo "lint-shell-sigpipe: FAIL — $fail pipe-into-head site(s) in $checked pipefail script(s)" >&2
  exit 1
fi
echo "lint-shell-sigpipe: OK — $checked pipefail script(s), no unguarded pipe-into-head"
