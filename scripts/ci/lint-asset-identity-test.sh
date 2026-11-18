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
run 1 "== 'native' (ClickHouse)" q.sql "SELECT 1 FROM t WHERE asset == 'native';"
run 1 "reversed operands"        q.sql "SELECT 1 FROM t WHERE 'native' = base_asset;"
run 1 "one-element IN"           q.sql "SELECT 1 FROM t WHERE quote_asset IN ('native');"
run 1 "one-element NOT IN"       q.sql "SELECT 1 FROM t WHERE quote_asset NOT IN ( 'native' );"
run 1 "lowercase ilike"          q.sql "SELECT 1 FROM t WHERE asset ilike 'usdc-%';"
run 1 "starts_with code"         q.sql "SELECT 1 FROM t WHERE starts_with(asset, 'USDC-');"
run 1 "HasPrefix code"           x.go "ok := strings.HasPrefix(id, \"USDC-\")"
run 1 "asset_code = code"        q.sql "SELECT 1 FROM t WHERE asset_code = 'USDC';"
run 1 "code = code reversed"     q.sql "SELECT 1 FROM t WHERE 'USDC' = a.code;"
run 1 "= 'crypto:XLM'"           q.sql "SELECT 1 FROM t WHERE base_asset = 'crypto:XLM';"
run 0 "multi-element IN"         q.sql "SELECT 1 FROM t WHERE quote_asset IN ('native', 'crypto:XLM', \$3);"
run 0 "error_code literal"       q.sql "SELECT 1 FROM t WHERE error_code = 'E42';"
run 0 "Go fiat code compare"     x.go "if quote.Code == \"USD\" { return }"
run 0 "HasPrefix lowercase tag"  x.go "ok := strings.HasPrefix(name, \"x-\")"
run 0 "single bound parameter"   q.sql "SELECT 1 FROM t WHERE base_asset = \$1;"
run 0 "alias array bind"         x.go "const q = \`SELECT 1 WHERE counterpart = ANY(\$4) OR counterpart LIKE 'fiat:%'\`"
run 0 "marked line"              x.go "const q = \`WHERE asset = 'native'\` // asset-identity: single-form ok (storage partition)"
run 0 "test file"                x_test.go "const q = \`WHERE base_asset = 'native'\`"
run 0 "markdown prose"           doc.md "never write WHERE base_asset = 'native'"
run 0 "baselined row"            x.go "	WHERE asset = 'native' AND rank <= 100" "x.go|WHERE asset = 'native' AND rank <= 100|reason"
run 1 "stale baseline row"       x.go "const ok = 1" "x.go|WHERE asset = 'native'|reason"
# A grep that could not run is an error (exit 2), never a clean pass.
norepo=$(mktemp -d)
rc=0
env -u GIT_DIR -u GIT_INDEX_FILE -u GIT_WORK_TREE GIT_CEILING_DIRECTORIES="$(dirname "$norepo")" LINT_ROOT="$norepo" \
  LINT_BASELINE="$norepo/baseline" "$lint" >/dev/null 2>&1 </dev/null || rc=$?
if [ "$rc" -ne 2 ]; then echo "FAIL: grep error is not clean (exit $rc, want 2)"; fail=1; else echo "ok: grep error is not clean"; fi
rm -rf "$norepo"
exit "$fail"
