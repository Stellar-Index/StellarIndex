#!/usr/bin/env bash
# lint-predictable-temp.sh — no host script or Go writer may stage output in a
# temp file whose name is predictable (`"$out.tmp.$$"`, `path + ".tmp"`).
# Several run as root and write into a directory a service user owns; that
# user can pre-plant the name as a symlink and have root truncate its target.
# Use `mktemp "$out.tmp.XXXXXX"` in shell and internal/atomicfile in Go.
#
# Usage: lint-predictable-temp.sh [FILE_OR_DIR ...]
#        (default configs/ansible deploy scripts/ops scripts/dev internal cmd)
set -euo pipefail
cd "$(dirname "$0")/../.."

roots=("$@")
[ "${#roots[@]}" -gt 0 ] || roots=(configs/ansible deploy scripts/ops scripts/dev internal cmd)

files=()
for r in "${roots[@]}"; do
  if [ -d "$r" ]; then
    while IFS= read -r f; do files+=("$f"); done < <(find "$r" -type f \
      \( -name '*.sh' -o -name '*.j2' -o -name '*.yml' -o -name '*.yaml' -o -name '*.go' \) \
      ! -name '*-test.sh' ! -name '*_test.go' ! -path 'internal/atomicfile/*' | sort)
  elif [ -f "$r" ]; then
    files+=("$r")
  else
    echo "lint-predictable-temp: FAIL — not found: $r" >&2
    exit 1
  fi
done
if [ "${#files[@]}" -eq 0 ]; then
  echo "lint-predictable-temp: FAIL — no files under: ${roots[*]}" >&2
  exit 1
fi

# Shell: a name ending in the shell's pid, as an assignment or a redirect target.
# Go: a temp path built by appending a fixed ".tmp".
gofiles=(); shfiles=()
for f in "${files[@]}"; do
  case "$f" in *.go) gofiles+=("$f") ;; *) shfiles+=("$f") ;; esac
done
hits="$(
  grep -nE '(\.tmp\.\$\$|[>=] *"\$\{?[A-Za-z_]+\}?\.\$\$")' ${shfiles[@]+"${shfiles[@]}"} /dev/null || true
  grep -nE '\+ *"\.tmp"' ${gofiles[@]+"${gofiles[@]}"} /dev/null || true
)"
if [ -n "$hits" ]; then
  echo "lint-predictable-temp: FAIL — predictable temp-file name (use mktemp \"\$out.tmp.XXXXXX\" / internal/atomicfile):" >&2
  echo "$hits" >&2
  exit 1
fi
echo "lint-predictable-temp: OK (${#files[@]} file(s))"
