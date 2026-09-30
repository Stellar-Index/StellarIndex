#!/usr/bin/env bash
# lint-remote-tmp-staging.sh — no workflow may copy to a fixed /tmp path on a
# remote host (`scp … host:/tmp/name`). A predictable name in a shared,
# world-writable directory can be pre-planted by any local user on the target
# before a privileged step runs it. Stage into `mktemp -d` in the same ssh
# session instead (see the "Apply Prometheus rules" step in deploy.yml).
#
# Usage: lint-remote-tmp-staging.sh [FILE_OR_DIR ...]  (default .github/workflows)
set -euo pipefail
cd "$(dirname "$0")/../.."

roots=("$@")
[ "${#roots[@]}" -gt 0 ] || roots=(.github/workflows)

files=()
for r in "${roots[@]}"; do
  if [ -d "$r" ]; then
    while IFS= read -r f; do files+=("$f"); done < <(find "$r" -type f \( -name '*.yml' -o -name '*.yaml' \) | sort)
  elif [ -f "$r" ]; then
    files+=("$r")
  else
    echo "lint-remote-tmp-staging: FAIL — not found: $r" >&2
    exit 1
  fi
done
if [ "${#files[@]}" -eq 0 ]; then
  echo "lint-remote-tmp-staging: FAIL — no workflow files under: ${roots[*]}" >&2
  exit 1
fi

# host:/tmp/… is the scp/rsync remote-path form; a runner-local /tmp path has
# no `:` in front of it.
if hits=$(grep -nE '[A-Za-z0-9_}"'"'"']:/(var/)?tmp/' "${files[@]}" /dev/null); then
  echo "lint-remote-tmp-staging: FAIL — remote copy to a fixed /tmp path (stage into a per-run mktemp -d instead):" >&2
  echo "$hits" >&2
  exit 1
fi
echo "lint-remote-tmp-staging: OK (${#files[@]} file(s))"
