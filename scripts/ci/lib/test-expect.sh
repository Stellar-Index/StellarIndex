# shellcheck shell=bash
# test-expect.sh — shared `expect()` assertion for the migration-lint
# fixture tests (lint-migration-compat-test.sh, lint-migration-immutability-test.sh).
#
# Callers set RC/OUT from their own `run()` (the two scripts' fixture
# setups differ) and declare `pass=0`/`fail=0` before sourcing this.
#
# Source: . "$(dirname "$0")/lib/test-expect.sh"

# expect <name> <want-rc> <want-substring-or-empty>
expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_sub" ] && ! grep -q -- "$want_sub" <<<"$OUT"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}
