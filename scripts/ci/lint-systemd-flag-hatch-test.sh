#!/usr/bin/env bash
# lint-systemd-flag-hatch-test — proves lint-systemd-flag-hatch.sh fires on
# the #1231 shape (brace-form hatch in a direct-exec ExecStart), passes the
# bare form and the sh -c form, refuses a vacuous tree, and passes the repo.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-systemd-flag-hatch.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

# unit <case> <file> <ExecStart body> — write a one-unit fixture tree.
unit() {
  mkdir -p "$TMP/$1/configs/systemd"
  printf '[Service]\nEnvironment=EXTRA_FLAGS=\nExecStart=%s\n' "$3" >"$TMP/$1/configs/systemd/$2"
}

# expect <case> <want-exit> <label>
expect() {
  bash "$LINT" "$TMP/$1" >"$TMP/$1.out" 2>&1
  local rc=$?
  if [ "$rc" -eq "$2" ]; then ok "$3"; else bad "$3 (exit $rc, want $2)"; sed 's/^/      /' "$TMP/$1.out"; fi
}

unit brace x.service.j2 "/usr/local/bin/stellarindex-ops issuer-flags \\
  -config \${CONFIG_PATH} \\
  \${EXTRA_FLAGS}"
expect brace 1 "direct-exec \${EXTRA_FLAGS} is rejected (one argv token)"

unit wrapped x.service.j2 "/usr/local/sbin/run-heavy-job.sh job /usr/local/bin/stellarindex-ops job -write \${EXTRA_FLAGS}"
expect wrapped 1 "run-heavy-job.sh does not re-split: brace form still rejected"

unit other x.service "/usr/bin/foo \${WORKER_ARGS}"
expect other 1 "any *_ARGS hatch in brace form is rejected, not only EXTRA_FLAGS"

unit bare x.service.j2 "/usr/local/bin/stellarindex-ops issuer-flags \\
  -config \${CONFIG_PATH} \\
  \$EXTRA_FLAGS"
expect bare 0 "direct-exec bare \$EXTRA_FLAGS word-splits"

unit shell x.service.j2 "/bin/sh -c '/usr/local/bin/stellarindex-ops supply snapshot \\
  -write \\
  \${EXTRA_FLAGS}'"
unit shell y.service "/usr/bin/foo \$EXTRA_FLAGS"
expect shell 0 "sh -c unit is exempt (the shell re-splits)"

unit none x.service '/usr/bin/true'
expect none 1 "a tree with no hatch fails instead of passing vacuously"

bash "$LINT" >"$TMP/repo.out" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then ok "repository units pass"; else bad "repository units fail"; sed 's/^/      /' "$TMP/repo.out"; fi

echo
echo "lint-systemd-flag-hatch-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
