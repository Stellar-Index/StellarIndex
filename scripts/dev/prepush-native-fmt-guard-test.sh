#!/usr/bin/env bash
# prepush-native-fmt-guard-test.sh — fixture test for CA2-A38-correct-3.
#
# The native profile runs `make verify` (and its `make fmt`) inside a
# disposable scratch checkout, never the committed HEAD directly. If a
# formatter rewrites a file there, the container profile catches it
# (docker/verify/entrypoint.sh runs `git diff --exit-code --stat` right
# after `make verify`); the native profile did not, so it could print
# "ALL REQUIRED CHECKS PASSED" while the actual pushed commit stayed
# unformatted and failed CI's gofumpt check on main.
#
# This builds a throwaway repo carrying a real copy of prepush.sh, stubs
# every heavy dependency (doctor, the range-sensitive gates, the
# integration-required policy) so only the scratch-checkout + make-verify
# + diff-check shape is exercised, and points its `make verify` at a
# target that mimics a formatter: it rewrites a tracked file in place.
#
# Run: bash scripts/dev/prepush-native-fmt-guard-test.sh
set -uo pipefail

# `git init` honours an INHERITED GIT_DIR ahead of its own `-C`, and a git
# hook exports GIT_DIR/GIT_INDEX_FILE — without this a fixture's init
# re-initialises the REAL repository.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

cd "$(dirname "$0")/../.." || exit 1
PREPUSH="$PWD/scripts/dev/prepush.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
REPO="$TMP/repo"

mkdir -p "$REPO/scripts/dev" "$REPO/scripts/ci" "$REPO/.github/workflows"
cp "$PREPUSH" "$REPO/scripts/dev/prepush.sh"
chmod +x "$REPO/scripts/dev/prepush.sh"

cat > "$REPO/scripts/dev/doctor.sh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat > "$REPO/scripts/ci/lint-baseline-growth.sh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat > "$REPO/scripts/ci/lint-replay-plan.sh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat > "$REPO/scripts/ci/prepush-integration-required.sh" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
chmod +x "$REPO"/scripts/dev/doctor.sh "$REPO"/scripts/ci/*.sh

cat > "$REPO/.gitleaks.toml" <<'EOF'
title = "fixture"
EOF

cat > "$REPO/.github/workflows/dummy.yml" <<'EOF'
name: dummy
on: push
permissions: {}
jobs:
  noop:
    runs-on: ubuntu-latest
    permissions: {}
    steps:
      - run: "true"
EOF

# The Makefile's `verify` target stands in for `make fmt` rewriting a
# committed Go file in the scratch checkout — the exact failure shape
# the finding describes.
cat > "$REPO/Makefile" <<'EOF'
verify:
	echo "rewritten by formatter" >> tracked.go
EOF
printf 'package demo\n' > "$REPO/tracked.go"

(
  cd "$REPO" || exit 1
  git init -q .
  real_repo="$(pwd -P)"
  case "$(git rev-parse --absolute-git-dir)" in
    "$real_repo"/*) ;;
    *) echo "fixture escaped its directory — refusing to continue" >&2; exit 1 ;;
  esac
  git config user.email t@t.invalid
  git config user.name t
  git config commit.gpgsign false
  git add -A
  git commit -qm "base"
  printf 'package demo\n\nfunc More() {}\n' >> tracked.go
  git add -A
  git commit -qm "candidate: touch tracked.go"
)

OUT="$(cd "$REPO" && VERIFY_PROFILE=native VERIFY_INTEGRATION=never ./scripts/dev/prepush.sh 2>&1)"
RC=$?

pass=0
fail=0
expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [[ "$RC" -ne "$want_rc" ]]; then
    echo "FAIL: $name — rc=$RC want=$want_rc"
    echo "$OUT" | sed -n '1,20s/^/    | /p'
    fail=$((fail + 1))
    return
  fi
  if [[ -n "$want_sub" && "$OUT" != *"$want_sub"* ]]; then
    echo "FAIL: $name — output missing: $want_sub"
    echo "$OUT" | sed -n '1,20s/^/    | /p'
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

expect "native profile fails when make verify rewrites a committed file" 1 \
  "a formatter or generator changed committed HEAD"
if [[ "$OUT" == *"ALL REQUIRED CHECKS PASSED"* ]]; then
  echo "FAIL: native profile must not print ALL REQUIRED CHECKS PASSED after an undetected rewrite"
  fail=$((fail + 1))
else
  echo "ok: no false PASSED banner"
  pass=$((pass + 1))
fi

echo "prepush-native-fmt-guard-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
