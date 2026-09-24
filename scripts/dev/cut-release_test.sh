#!/usr/bin/env bash
# cut-release_test.sh — regression test for CA2-A38-harden-2.
#
# Pins: cut-release.sh must tag the commit `make prepush` actually
# verified, not whatever `main` resolves to after the ~20-minute gate
# returns. Simulates a concurrent session committing directly to `main`
# WHILE the gate runs (this repo routinely runs several agents/sessions
# against one worktree with direct-to-main commits) via a faked `make`
# that advances `main` before reporting success. Before the fix,
# `git tag "$TAG"` was bare and tagged whatever HEAD was at that point,
# so the tag landed on a commit `make prepush` never verified.
#
# Run: bash scripts/dev/cut-release_test.sh
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/dev/cut-release.sh"

TMP="$(mktemp -d)"
TMP="$(cd "$TMP" && pwd -P)" # resolve symlinks (e.g. macOS /tmp -> /private/tmp)
trap 'rm -rf "$TMP"' EXIT

FAKEBIN="$TMP/bin"
mkdir -p "$FAKEBIN"

# gh: always fail, forcing the git-fetch/ls-remote fallback paths, so
# this test needs no network access or gh auth.
cat > "$FAKEBIN/gh" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
chmod +x "$FAKEBIN/gh"

# make: on `prepush`, land a concurrent commit on `main` WHILE the gate
# is "running", then report success — exactly the race the finding
# describes. Any other target fails loudly so an unexpected call is
# never silently accepted as a pass.
cat > "$FAKEBIN/make" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == "prepush" ]]; then
  git commit --allow-empty -q -m "concurrent commit landed during prepush"
  echo "ALL REQUIRED CHECKS PASSED"
  exit 0
fi
echo "unexpected make target: $*" >&2
exit 1
EOF
chmod +x "$FAKEBIN/make"

unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

REPO="$TMP/repo"
ORIGIN="$TMP/origin.git"
git init --quiet --bare "$ORIGIN"
case "$(git -C "$ORIGIN" rev-parse --absolute-git-dir)" in
  "$ORIGIN"/*|"$ORIGIN") ;;
  *) echo "fixture escaped its directory — refusing to continue" >&2; exit 1 ;;
esac
git init --quiet -b main "$REPO"
case "$(git -C "$REPO" rev-parse --absolute-git-dir)" in
  "$REPO"/*) ;;
  *) echo "fixture escaped its directory — refusing to continue" >&2; exit 1 ;;
esac
git -C "$REPO" config user.email test@example.com
git -C "$REPO" config user.name test
git -C "$REPO" remote add origin "$ORIGIN"

cat > "$REPO/CHANGELOG.md" <<'EOF'
# Changelog

## [Unreleased]

## [v0.0.1] — 2026-01-01
- initial cut
EOF
git -C "$REPO" add CHANGELOG.md
git -C "$REPO" commit --quiet -m "seed"
git -C "$REPO" push --quiet origin main

verified_sha="$(git -C "$REPO" rev-parse main)"

(
  cd "$REPO" && \
  PATH="$FAKEBIN:$PATH" GIT_TERMINAL_PROMPT=0 \
    bash "$SCRIPT" v0.0.1 --yes </dev/null >"$TMP/out.log" 2>&1
)
status=$?

post_sha="$(git -C "$REPO" rev-parse main)"

fail=0
if [[ "$post_sha" == "$verified_sha" ]]; then
  echo "FAIL: fixture bug — concurrent commit never landed" >&2
  fail=1
fi

if git -C "$REPO" rev-parse v0.0.1 >/dev/null 2>&1; then
  tagged_sha="$(git -C "$REPO" rev-parse v0.0.1)"
  if [[ "$tagged_sha" != "$verified_sha" ]]; then
    echo "FAIL: v0.0.1 tags ${tagged_sha}, the UNVERIFIED post-gate commit, not the prepush-verified ${verified_sha}" >&2
    fail=1
  fi
  if [[ "$status" -eq 0 ]]; then
    echo "FAIL: script exited 0 and tagged an unverified commit" >&2
    fail=1
  fi
else
  if [[ "$status" -eq 0 ]]; then
    echo "FAIL: script exited 0 but created no tag" >&2
    fail=1
  fi
fi

if [[ "$fail" -eq 0 ]]; then
  echo "PASS: cut-release.sh refused to tag the unverified post-gate commit"
else
  echo "--- script output ---" >&2
  cat "$TMP/out.log" >&2
fi
exit "$fail"
