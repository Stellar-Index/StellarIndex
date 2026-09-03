#!/usr/bin/env bash
# lint-actions-pinning-test.sh — fixture tests for the SHA-pinning gate
# (scripts/ci/lint-actions-pinning.sh, F-1216).
#
# The gate spent months passing everything. Its hard-fail arm read
# `git diff origin/main -- .github/workflows/*.yml`, which is empty on a
# push-to-main checkout (HEAD *is* origin/main) — and direct-to-main is
# how every commit lands here until 1.0. All three cases below went
# GREEN under that shape, including the unpinned one. A supply-chain
# guard that cannot be observed failing is not a guard, so its verdicts
# are pinned here rather than assumed:
#
#   - an unpinned third-party action (bare `@` ref, or none) is CAUGHT;
#   - a tag-pinned third-party action is CAUGHT;
#   - a SHA-pinned third-party action passes;
#   - a version comment after the SHA does not confuse the match;
#   - a short/over-long hex ref is not mistaken for a SHA;
#   - actions/* and github/* stay exempt (GitHub's own trust boundary);
#   - `.yaml` is scanned as well as `.yml`;
#   - prose containing "causes: sshd" is not read as a `uses:` key;
#   - an empty root, and a root whose workflows declare no steps, FAIL
#     rather than passing vacuously;
#   - the repo's own workflow tree is clean.
#
# Run: bash scripts/ci/lint-actions-pinning-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-actions-pinning.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
check() { # check <desc> <want-exit> <root>
  local desc="$1" want="$2" root="$3" got
  bash "$LINT" "$root" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc (exit $got, want $want)"
    fail=$((fail + 1))
  fi
}

mk() { # mk <dir> <file> <steps-yaml>
  mkdir -p "$TMP/$1"
  cat > "$TMP/$1/$2" <<YML
name: fixture
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
$3
YML
}

SHA=3d3c42e5aac5ba805825da76410c181273ba90b1

echo "lint-actions-pinning-test: pinning verdicts"

mk unpinned wf.yml '      - uses: peter-evans/create-pull-request'
check "an unpinned third-party action is caught" 1 "$TMP/unpinned"

mk tagged wf.yml '      - uses: peter-evans/create-pull-request@v6'
check "a tag-pinned third-party action is caught" 1 "$TMP/tagged"

mk branch wf.yml '      - uses: peter-evans/create-pull-request@main'
check "a branch-pinned third-party action is caught" 1 "$TMP/branch"

mk pinned wf.yml "      - uses: peter-evans/create-pull-request@$SHA  # v6"
check "a SHA-pinned third-party action passes" 0 "$TMP/pinned"

mk nocomment wf.yml "      - uses: peter-evans/create-pull-request@$SHA"
check "a SHA pin without a version comment passes" 0 "$TMP/nocomment"

mk short wf.yml "      - uses: peter-evans/create-pull-request@${SHA:0:12}"
check "an abbreviated hex ref is not a SHA pin" 1 "$TMP/short"

mk long wf.yml "      - uses: peter-evans/create-pull-request@${SHA}ab  # not a sha"
check "an over-long hex ref is not a SHA pin" 1 "$TMP/long"

echo "lint-actions-pinning-test: scope"

mk firstparty wf.yml '      - uses: actions/checkout@v7
      - uses: github/codeql-action/analyze@v3'
check "actions/* and github/* stay exempt" 0 "$TMP/firstparty"

mk ext wf.yaml '      - uses: peter-evans/create-pull-request@v6'
check ".yaml is scanned too, not only .yml" 1 "$TMP/ext"

# The gate once hard-failed a PR because an error message's "Usual
# causes: sshd ..." parsed as the action `sshd`; the anchoring that
# fixed it must survive the rewrite.
mk prose wf.yml "      - uses: peter-evans/create-pull-request@$SHA  # v6
      - run: echo 'Usual causes: sshd is down, or the host rotated keys'"
check "prose ending in causes: is not read as a uses: key" 0 "$TMP/prose"

echo "lint-actions-pinning-test: vacuity"

mkdir -p "$TMP/empty"
check "a root with no workflow files FAILS rather than passing vacuously" 1 "$TMP/empty"

mkdir -p "$TMP/nosteps"
printf 'name: fixture\non: [push]\n' > "$TMP/nosteps/wf.yml"
check "a root whose workflows declare no uses: FAILS rather than passing vacuously" 1 "$TMP/nosteps"

echo "lint-actions-pinning-test: the real tree"
check "the repo's own workflows are fully SHA-pinned" 0 ".github/workflows"

echo "lint-actions-pinning-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
