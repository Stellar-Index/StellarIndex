#!/usr/bin/env bash
# lint-remote-tmp-staging-test.sh — prove lint-remote-tmp-staging.sh can go red.
# shellcheck disable=SC2016  # fixtures are literal workflow text; $TARGET must not expand here
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-remote-tmp-staging.sh"
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

fixture() { # <name> <run-line>
  printf 'jobs:\n  j:\n    steps:\n      - run: |\n          %s\n' "$2" > "$TMP/$1.yml"
  echo "$TMP/$1.yml"
}

check "the repo's workflows pass" ok

check "scp to \${TARGET}:/tmp/<name> is rejected" red \
  "$(fixture quoted 'scp -q a.sh "${TARGET}:/tmp/apply-rules.sh"')"
check "scp to host:/tmp/<dir>/ is rejected" red \
  "$(fixture bare 'scp -q r/*.yml root@r1:/tmp/rules.incoming/')"
check "rsync to host:/var/tmp/<name> is rejected" red \
  "$(fixture vartmp 'rsync -a x "$TARGET":/var/tmp/x')"
check "a runner-local /tmp path passes" ok \
  "$(fixture local 'cmd > /tmp/out.txt 2>&1')"
check "tar | ssh into mktemp -d passes" ok \
  "$(fixture mktemp "tar -cf - x | ssh \"\$TARGET\" 'd=\$(mktemp -d); tar -xf - -C \"\$d\"'")"
check "a missing path is rejected" red "$TMP/does-not-exist"

mkdir "$TMP/empty"
check "an empty directory is rejected, not a vacuous pass" red "$TMP/empty"

echo "lint-remote-tmp-staging-test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
