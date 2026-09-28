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
  # A fresh, non-FAIL SLA proof report so this fixture clears the
  # SLA-proof gate (added by C04) and actually reaches `make prepush`,
  # which is what this test exercises.
  mkdir -p "$REPO/docs/operations"
  {
    echo "# SLA proof report — $(date -u +%Y-%m-%d)"
    echo ""
    echo "**Verdict: NOT PROVEN.**"
  } > "$REPO/docs/operations/sla-proof-$(date -u +%Y-%m-%d).md"
  git -C "$REPO" add CHANGELOG.md docs/operations
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

# _sla_fixture_setup <tmp> — build a bare origin + working repo with a
# non-empty CHANGELOG section for v0.0.1 and faked gh/make binaries under
# <tmp>/bin (gh always fails to force the fallback paths used elsewhere
# in this script; make succeeds unconditionally on `prepush`, because
# these tests pin the SLA-proof gate that runs BEFORE the prepush gate,
# not the prepush gate itself). Populates <tmp>/repo and <tmp>/origin.git.
_sla_fixture_setup() {
  local tmp="$1" fakebin repo origin

  unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
        GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

  fakebin="$tmp/bin"
  repo="$tmp/repo"
  origin="$tmp/origin.git"
  mkdir -p "$fakebin"

  cat > "$fakebin/gh" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
  chmod +x "$fakebin/gh"

  cat > "$fakebin/make" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == "prepush" ]]; then
  echo "ALL REQUIRED CHECKS PASSED"
  exit 0
fi
echo "unexpected make target: $*" >&2
exit 1
EOF
  chmod +x "$fakebin/make"

  git init --quiet --bare "$origin"
  git init --quiet -b main "$repo"
  git -C "$repo" config user.email test@example.com
  git -C "$repo" config user.name test
  git -C "$repo" remote add origin "$origin"

  cat > "$repo/CHANGELOG.md" <<'EOF'
# Changelog

## [Unreleased]

## [v0.0.1] — 2026-01-01
- initial cut
EOF
  mkdir -p "$repo/docs/operations"
  git -C "$repo" add CHANGELOG.md
  git -C "$repo" commit --quiet -m "seed"
  git -C "$repo" push --quiet origin main
}

# _sla_days_ago <n> — YYYY-MM-DD n days before today, portable across GNU
# and BSD/macOS date (same fallback idiom as the script's sla_to_epoch).
_sla_days_ago() {
  date -u -d "-$1 days" +%Y-%m-%d 2>/dev/null || date -u -v-"$1"d +%Y-%m-%d
}

# test_sla_proof_missing_blocks_cut pins the C04 must_fix: no
# docs/operations/sla-proof-<date>.md report at all refuses the cut.
test_sla_proof_missing_blocks_cut() {
  local TMP out status
  TMP="$(mktemp -d)"
  TMP="$(cd "$TMP" && pwd -P)"
  trap 'rm -rf "$TMP"' RETURN
  _sla_fixture_setup "$TMP"

  out="$(cd "$TMP/repo" && PATH="$TMP/bin:$PATH" GIT_TERMINAL_PROMPT=0 \
    bash "$SCRIPT" v0.0.1 --dry-run </dev/null 2>&1)"
  status=$?

  if [[ "$status" -eq 0 ]] || ! grep -q 'no docs/operations/sla-proof-<YYYY-MM-DD>.md report found' <<<"$out"; then
    echo "FAIL: missing SLA proof did not refuse the cut (status=$status)" >&2
    echo "--- output ---" >&2
    echo "$out" >&2
    return 1
  fi
  echo "PASS: cut-release.sh refuses with no SLA proof report"
}

# test_sla_proof_stale_blocks_cut pins the C04 must_fix: a proof older
# than SLA_PROOF_MAX_AGE_DAYS (default 45) refuses the cut even though a
# report exists and its verdict is not FAIL.
test_sla_proof_stale_blocks_cut() {
  local TMP out status stale_date
  TMP="$(mktemp -d)"
  TMP="$(cd "$TMP" && pwd -P)"
  trap 'rm -rf "$TMP"' RETURN
  _sla_fixture_setup "$TMP"

  stale_date="$(_sla_days_ago 100)"
  {
    echo "# SLA proof report — ${stale_date}"
    echo ""
    echo "**Verdict: NOT PROVEN.**"
  } > "$TMP/repo/docs/operations/sla-proof-${stale_date}.md"
  git -C "$TMP/repo" add docs/operations
  git -C "$TMP/repo" commit --quiet -m "stale proof"
  git -C "$TMP/repo" push --quiet origin main

  out="$(cd "$TMP/repo" && PATH="$TMP/bin:$PATH" GIT_TERMINAL_PROMPT=0 \
    bash "$SCRIPT" v0.0.1 --dry-run </dev/null 2>&1)"
  status=$?

  if [[ "$status" -eq 0 ]] || ! grep -q 'is [0-9]\+ day(s) old (max' <<<"$out"; then
    echo "FAIL: a ${stale_date} (100-day-old) SLA proof did not refuse the cut (status=$status)" >&2
    echo "--- output ---" >&2
    echo "$out" >&2
    return 1
  fi
  echo "PASS: cut-release.sh refuses with a stale SLA proof report"
}

# test_sla_proof_fail_verdict_blocks_cut pins the C04 must_fix: a fresh
# report recording Verdict: FAIL refuses the cut — a release must not
# ship on a known, measured SLA breach.
test_sla_proof_fail_verdict_blocks_cut() {
  local TMP out status today
  TMP="$(mktemp -d)"
  TMP="$(cd "$TMP" && pwd -P)"
  trap 'rm -rf "$TMP"' RETURN
  _sla_fixture_setup "$TMP"

  today="$(date -u +%Y-%m-%d)"
  {
    echo "# SLA proof report — ${today}"
    echo ""
    echo "**Verdict: FAIL.**"
  } > "$TMP/repo/docs/operations/sla-proof-${today}.md"
  git -C "$TMP/repo" add docs/operations
  git -C "$TMP/repo" commit --quiet -m "fail proof"
  git -C "$TMP/repo" push --quiet origin main

  out="$(cd "$TMP/repo" && PATH="$TMP/bin:$PATH" GIT_TERMINAL_PROMPT=0 \
    bash "$SCRIPT" v0.0.1 --dry-run </dev/null 2>&1)"
  status=$?

  if [[ "$status" -eq 0 ]] || ! grep -q 'records Verdict: FAIL' <<<"$out"; then
    echo "FAIL: a fresh Verdict: FAIL SLA proof did not refuse the cut (status=$status)" >&2
    echo "--- output ---" >&2
    echo "$out" >&2
    return 1
  fi
  echo "PASS: cut-release.sh refuses on a fresh Verdict: FAIL SLA proof"
}

# test_sla_proof_not_proven_fresh_passes pins the C04 must_fix's other
# half: NOT PROVEN is a real report about a real measurement gap, not a
# breach, and a fresh one must let the cut proceed to the prepush gate.
test_sla_proof_not_proven_fresh_passes() {
  local TMP out status today
  TMP="$(mktemp -d)"
  TMP="$(cd "$TMP" && pwd -P)"
  trap 'rm -rf "$TMP"' RETURN
  _sla_fixture_setup "$TMP"

  today="$(date -u +%Y-%m-%d)"
  {
    echo "# SLA proof report — ${today}"
    echo ""
    echo "**Verdict: NOT PROVEN.**"
  } > "$TMP/repo/docs/operations/sla-proof-${today}.md"
  git -C "$TMP/repo" add docs/operations
  git -C "$TMP/repo" commit --quiet -m "not proven proof"
  git -C "$TMP/repo" push --quiet origin main

  out="$(cd "$TMP/repo" && PATH="$TMP/bin:$PATH" GIT_TERMINAL_PROMPT=0 \
    bash "$SCRIPT" v0.0.1 --dry-run </dev/null 2>&1)"
  status=$?

  if [[ "$status" -ne 0 ]] || ! grep -q '\[--dry-run\] would run:' <<<"$out"; then
    echo "FAIL: a fresh Verdict: NOT PROVEN SLA proof wrongly refused the cut (status=$status)" >&2
    echo "--- output ---" >&2
    echo "$out" >&2
    return 1
  fi
  echo "PASS: cut-release.sh proceeds past a fresh Verdict: NOT PROVEN SLA proof"
}

overall_fail=0
test_tags_verified_commit || overall_fail=1
test_sla_proof_missing_blocks_cut || overall_fail=1
test_sla_proof_stale_blocks_cut || overall_fail=1
test_sla_proof_fail_verdict_blocks_cut || overall_fail=1
test_sla_proof_not_proven_fresh_passes || overall_fail=1
test_mktemp_template_is_portable || overall_fail=1
exit "$overall_fail"
