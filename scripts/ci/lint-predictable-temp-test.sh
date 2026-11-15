#!/usr/bin/env bash
# lint-predictable-temp-test.sh — prove lint-predictable-temp.sh can go red.
# shellcheck disable=SC2016  # fixtures are literal script text; nothing may expand here
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-predictable-temp.sh"
PASS=0; FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

check() { # <name> <expected: ok|red> <gate args...>
  local name="$1" want="$2" rc
  shift 2
  bash "$GATE" "$@" >/dev/null 2>&1; rc=$?
  if { [ "$want" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$want" = red ] && [ "$rc" -gt 0 ]; }; then
    printf '  ok   %s\n' "$name"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n' "$name" "$rc" "$want"; FAIL=$((FAIL+1))
  fi
}

fixture() { # <file-name> <line>
  printf '%s\n' "$2" > "$TMP/$1"
  echo "$TMP/$1"
}

check "the repo passes" ok

check 'tmp="$out.tmp.$$" is rejected' red "$(fixture a.sh 'tmp="$out.tmp.$$"')"
check 'a redirect to "${OUT}.$$" is rejected' red "$(fixture b.yml '        } > "${OUT}.$$" && mv "${OUT}.$$" "$OUT"')"
check 'Go path + ".tmp" is rejected' red "$(fixture c.go '	tmp := path + ".tmp"')"
check 'mktemp passes' ok "$(fixture d.sh 'tmp="$(mktemp "$out.tmp.XXXXXX")"')"
check 'a pid in a unit name passes' ok "$(fixture e.sh 'UNIT="heavy-${NAME}-$$"')"
check 'psql "$DSN" is rejected' red "$(fixture f.sh 'TIP=$(psql "$DSN" -tAc "SELECT 1")')"
check '"$PSQL" "$STELLARINDEX_POSTGRES_DSN" is rejected' red "$(fixture g.sh 'if ! "$PSQL" "$STELLARINDEX_POSTGRES_DSN" -tA <<SQL')"
check 'psql -X "$DSN" is rejected' red "$(fixture f2.sh 'psql -X -v ON_ERROR_STOP=1 "$DSN" <<SQL')"
check 'psql -d "$DSN" is rejected' red "$(fixture f3.sh 'psql -d "$DSN" -c "SELECT 1"')"
check 'psql --dbname="$DSN" is rejected' red "$(fixture f4.sh 'psql --dbname="$DSN" -c "SELECT 1"')"
check 'unquoted psql $DSN is rejected' red "$(fixture f5.sh 'psql $DSN -c "SELECT 1"')"
check '"$PG_BIN/psql" "$DSN" is rejected' red "$(fixture f6.sh '"$PG_BIN/psql" "$PG_DSN" -c "SELECT 1"')"
check 'psql "$DATABASE_URL" is rejected' red "$(fixture f7.sh 'psql "$DATABASE_URL" -c "SELECT 1"')"
check 'pg_dump "$DSN" is rejected' red "$(fixture f8.sh 'pg_dump "$DSN" > out.sql')"
check 'PGDSN="$DSN" psql-dsn passes' ok "$(fixture h.sh 'TIP=$(PGDSN="$DSN" psql-dsn -tAc "SELECT 1")')"
check "a missing path is rejected" red "$TMP/does-not-exist"

mkdir "$TMP/empty"
check "an empty directory is rejected, not a vacuous pass" red "$TMP/empty"

echo "lint-predictable-temp-test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
