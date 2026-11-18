#!/usr/bin/env bash
# Self-test for lint-asset-identity.sh: a bare 'native' comparison and a code-only LIKE are caught;
# an alias-array bind, a marked line, a test file, a baselined row and prose stay quiet; a stale
# baseline row fails.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
lint="$here/lint-asset-identity.sh"
fail=0
run() { # run <want-exit> <label> <file> <content> [baseline-content]
  local tmp; tmp=$(mktemp -d)
  (cd "$tmp" && env -u GIT_DIR -u GIT_INDEX_FILE -u GIT_WORK_TREE git init -q . && mkdir -p "$(dirname "$3")" && printf '%s\n' "$4" >"$3" \
    && env -u GIT_DIR -u GIT_INDEX_FILE -u GIT_WORK_TREE git add "$3")
  printf '%s\n' "${5:-}" >"$tmp/baseline"
  local rc=0
  env -u GIT_DIR -u GIT_INDEX_FILE -u GIT_WORK_TREE LINT_ROOT="$tmp" LINT_BASELINE="$tmp/baseline" "$lint" >/dev/null 2>&1 </dev/null || rc=$?
  if [ "$rc" -ne "$1" ]; then echo "FAIL: $2 (exit $rc, want $1)"; fail=1; else echo "ok: $2"; fi
  rm -rf "$tmp"
}
run 1 "counterpart = 'native'"   x.go "const q = \`SELECT 1 WHERE counterpart = 'native'\`"
run 1 "base_asset = 'native'"    x.go "const q = \`SELECT 1 FROM trades WHERE base_asset = 'native'\`"
run 1 "asset != 'native' in SQL" q.sql "SELECT 1 FROM t WHERE asset != 'native';"
run 1 "code-only LIKE"           x.go "const q = \`SELECT 1 WHERE counterpart LIKE 'USDC-%'\`"
run 0 "alias array bind"         x.go "const q = \`SELECT 1 WHERE counterpart = ANY(\$4) OR counterpart LIKE 'fiat:%'\`"
run 0 "marked line"              x.go "const q = \`WHERE asset = 'native'\` // asset-identity: single-form ok (storage partition)"
run 0 "test file"                x_test.go "const q = \`WHERE base_asset = 'native'\`"
run 0 "markdown prose"           doc.md "never write WHERE base_asset = 'native'"
run 0 "baselined row"            x.go "	WHERE asset = 'native' AND rank <= 100" "x.go|WHERE asset = 'native' AND rank <= 100|reason"
run 1 "stale baseline row"       x.go "const ok = 1" "x.go|WHERE asset = 'native'|reason"
exit "$fail"
