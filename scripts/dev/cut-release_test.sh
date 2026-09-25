#!/usr/bin/env bash
# cut-release_test.sh — regression tests for cut-release.sh.
#
# Two independent pins:
#  1. CA2-A38-harden-2: cut-release.sh must tag the commit `make prepush`
#     actually verified, not whatever `main` resolves to after the
#     ~20-minute gate returns. Simulates a concurrent session committing
#     directly to `main` WHILE the gate runs (this repo routinely runs
#     several agents/sessions against one worktree with direct-to-main
#     commits) via a faked `make` that advances `main` before reporting
#     success. Before the fix, `git tag "$TAG"` was bare and tagged
#     whatever HEAD was at that point, so the tag landed on a commit
#     `make prepush` never verified.
#  2. CA2-A38-correct-5: cut-release.sh's `mktemp -t` invocation must carry
#     a portable XXXXXX template. `mktemp -t <prefix>` with no XXXXXX
#     suffix is accepted by BSD/macOS mktemp (which synthesizes its own
#     template) but rejected by GNU coreutils mktemp on Linux with "too
#     few X's in template". Under cut-release.sh's `set -euo pipefail`
#     that abort happens before `make prepush` ever runs, so a release
#     could only ever be cut from macOS. This emulates GNU mktemp's
#     template check locally (no Docker) and pins the script's actual
#     mktemp invocation against it.
#
# Run: bash scripts/dev/cut-release_test.sh
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/dev/cut-release.sh"

# test_tags_verified_commit pins CA2-A38-harden-2.
test_tags_verified_commit() {
  local TMP FAKEBIN REPO ORIGIN verified_sha post_sha tagged_sha status fail

  TMP="$(mktemp -d)"
  TMP="$(cd "$TMP" && pwd -P)" # resolve symlinks (e.g. macOS /tmp -> /private/tmp)
  trap 'rm -rf "$TMP"' RETURN

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
    *) echo "fixture escaped its directory — refusing to continue" >&2; return 1 ;;
  esac
  git init --quiet -b main "$REPO"
  case "$(git -C "$REPO" rev-parse --absolute-git-dir)" in
    "$REPO"/*) ;;
    *) echo "fixture escaped its directory — refusing to continue" >&2; return 1 ;;
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
  return "$fail"
}

# test_mktemp_template_is_portable pins CA2-A38-correct-5.
test_mktemp_template_is_portable() {
  local mktemp_matches mktemp_line fakebin

  mktemp_matches="$(grep -oE 'mktemp -t [A-Za-z0-9.-]+' "$SCRIPT")"
  mktemp_line="${mktemp_matches%%$'\n'*}"
  if [[ -z "$mktemp_line" ]]; then
    echo "FAIL: could not find a 'mktemp -t ...' invocation in scripts/dev/cut-release.sh" >&2
    return 1
  fi

  # A fake GNU-coreutils-style mktemp: rejects any -t template lacking a
  # trailing run of X's, exactly as GNU mktemp does on Linux.
  fakebin="$(mktemp -d)"
  trap 'rm -rf "$fakebin"' RETURN
  cat > "$fakebin/mktemp" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "-t" ]]; then
  template="$2"
  if [[ "$template" != *XXXXXX ]]; then
    echo "mktemp: too few X's in template '$template'" >&2
    exit 1
  fi
fi
exec /usr/bin/mktemp "$@"
EOF
  chmod +x "$fakebin/mktemp"

  if PATH="$fakebin:$PATH" bash -c "set -euo pipefail; $mktemp_line" >/dev/null 2>&1; then
    echo "PASS: '$mktemp_line' has a portable GNU-safe template"
    return 0
  fi
  echo "FAIL: '$mktemp_line' has no XXXXXX suffix — fails under GNU coreutils mktemp (Linux), aborting cut-release.sh under set -e before make prepush runs" >&2
  return 1
}

overall_fail=0
test_tags_verified_commit || overall_fail=1
test_mktemp_template_is_portable || overall_fail=1
exit "$overall_fail"
