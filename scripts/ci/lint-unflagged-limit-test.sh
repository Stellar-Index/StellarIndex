#!/usr/bin/env bash
# shellcheck disable=SC2016 # fixtures are literal Go source with backticks and $1
# Prove each lint-unflagged-limit check can fail, against a fixture tree outside any git checkout.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
LINT="$PWD/scripts/ci/lint-unflagged-limit.sh"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
PASS=0; FAIL=0

reset() {
  rm -rf "$T/r"; mkdir -p "$T/r/internal/storage/pg" "$T/r/internal/platform" "$T/r/scripts/ci"
  : > "$T/r/scripts/ci/lint-unflagged-limit.baseline"
}
src() { printf '%b\n' "$1" > "$T/r/internal/storage/pg/${2:-a.go}"; }
check() { # check <name> <ok|red>
  local rc; bash "$LINT" --root "$T/r" >"$T/out" 2>&1; rc=$?
  if { [ "$2" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$2" = red ] && [ "$rc" -ne 0 ]; }; then
    printf '  ok   %s\n' "$1"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n' "$1" "$rc" "$2"; sed 's/^/       /' "$T/out"; FAIL=$((FAIL+1))
  fi
}

reset; src 'const q = `SELECT a FROM t LIMIT 5000`';                 check "unannotated LIMIT 5000 is rejected" red
reset; src 'const q = `SELECT a FROM t LIMIT 50`';                   check "LIMIT under 100 passes" ok
reset; src 'const q = `SELECT a FROM t LIMIT $1`';                   check "bound LIMIT passes" ok
reset; src 'const q = `SELECT a FROM t LIMIT 1 BY a`';               check "LIMIT 1 BY passes" ok
reset; src '// cap-ok: caller over-fetches and sets truncated\nconst q = `SELECT a FROM t LIMIT 500`'; check "cap-ok on the line above passes" ok
reset; src 'q := `SELECT a FROM t LIMIT 500` // cap-ok: one row per id';  check "cap-ok on the same line passes" ok
reset; src '// cap-ok: x\n\nconst q = `SELECT a FROM t LIMIT 500`';  check "cap-ok two lines above is rejected" red
reset; src 'const q = `SELECT a FROM t LIMIT 500`' a_test.go;        check "test files are ignored" ok
reset; src '// the roster LIMIT 5000 is counted elsewhere';           check "comment lines are ignored" ok
reset; printf 'const q = `SELECT a FROM t LIMIT 500`\n' > "$T/r/internal/platform/b.go"; check "internal/platform is scanned" red
reset; src 'const q = `SELECT a FROM t LIMIT 500`'; bash "$LINT" --root "$T/r" --record >/dev/null; check "baselined site passes" ok
src 'const q = `SELECT a FROM t LIMIT 500`\nconst r = `SELECT b FROM t LIMIT 900`'; check "new site beside a baselined one is rejected" red
reset; printf 'internal/storage/pg/a.go\tconst q = `SELECT a FROM t LIMIT 500`\n' > "$T/r/scripts/ci/lint-unflagged-limit.baseline"; src 'package pg'; check "stale baseline entry is rejected" red

echo "lint-unflagged-limit self-test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
