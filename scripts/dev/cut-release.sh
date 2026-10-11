#!/usr/bin/env bash
# cut-release.sh — guard-rail script for cutting a SemVer binary release.
#
# Usage: scripts/dev/cut-release.sh vX.Y.Z [--dry-run] [--yes]
#   --dry-run  run every gate, print what would happen, never tag/push
#   --yes      skip the interactive "Proceed?" confirmation (automation)
#
# A non-TTY invocation (CI, backgrounded shell, agent harness) MUST pass --yes
# or --dry-run, else it exits 2 before the slow verify gate. Otherwise `read`
# hits EOF at the prompt and aborts, and a caller piping through `| tail`
# masks the non-zero exit, reporting a "release cut" with no tag on origin.
#
# Steps, in order:
#   1. Tag matches SemVer (vX.Y.Z[-pre][+build]).
#   2. Preconditions on main, clean tree, in sync with origin/main, evaluated
#      as a set with every failure named (one at a time costs a round trip each).
#   3. Refuse non-TTY without --yes/--dry-run.
#   4. Tag exists neither locally nor on origin.
#   5. CHANGELOG.md has a non-empty `## [vX.Y.Z] — YYYY-MM-DD` section
#      (release.yml's auto-notes read it; empty means an empty release page).
#   5b. Newest docs/operations/sla-proof-YYYY-MM-DD.md exists, is within
#      SLA_PROOF_MAX_AGE_DAYS (default 45) and did not record `Verdict: FAIL`
#      (`NOT PROVEN` passes).
#   6. `make prepush` (the push clearance; container profile with Docker,
#      native otherwise).
#   7. Print the plan, then bail (--dry-run) or confirm (unless --yes), then
#      `git tag <tag>` and `git push origin <tag>`.
#
# release.yml fires on the tag push and produces the GitHub Release
# (cross-compiled binaries, SHA256SUMS, sigstore bundle); it does NOT build or
# push container images. Runbook: docs/operations/release-process.md.

set -euo pipefail

TAG="${1:-}"
DRY_RUN=0
ASSUME_YES=0

usage() {
  echo "Usage: $0 vX.Y.Z [--dry-run] [--yes]" >&2
  echo "  --dry-run  run every gate, print the plan, never tag/push" >&2
  echo "  --yes      skip the confirmation prompt (required for non-TTY/automation)" >&2
}

if [[ -z "$TAG" ]]; then
  usage
  exit 2
fi

# Flags may appear in any order after the tag.
for arg in "${@:2}"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    --yes)     ASSUME_YES=1 ;;
    *)
      echo "ERR: unknown argument '$arg'" >&2
      usage
      exit 2
      ;;
  esac
done

# Step 1 — tag shape
if ! [[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?(\+[A-Za-z0-9.-]+)?$ ]]; then
  echo "ERR: '$TAG' does not match SemVer vX.Y.Z[-prerelease][+build]" >&2
  exit 1
fi

cd "$(git rev-parse --show-toplevel)"

# Step 2 — the three repository preconditions, together. Nothing slow, nothing
# that writes, no prompt before this; the first output line names the failure.
preconditions=()

branch="$(git branch --show-current)"
if [[ "$branch" != "main" ]]; then
  preconditions+=("not on main (currently on '${branch:-<detached HEAD>}') — switch with 'git checkout main'")
fi

dirty="$(git status --porcelain)"
if [[ -n "$dirty" ]]; then
  preconditions+=("working tree has uncommitted changes — commit or discard them")
fi

# Read the remote tip via the gh API, not `git fetch`: works with SSH-keyed
# and gh-credential-only environments alike.
local_sha=$(git rev-parse main 2>/dev/null || echo unknown)
if command -v gh >/dev/null 2>&1; then
  remote_sha=$(gh api "repos/{owner}/{repo}/commits/main" --jq '.sha' 2>/dev/null || true)
  if [[ -z "$remote_sha" ]]; then
    echo "WARN: could not read origin/main via gh; falling back to git fetch" >&2
    git fetch --quiet origin main 2>/dev/null || true
    remote_sha=$(git rev-parse origin/main 2>/dev/null || echo unknown)
  fi
else
  git fetch --quiet origin main
  remote_sha=$(git rev-parse origin/main)
fi
if [[ "$local_sha" != "$remote_sha" ]]; then
  preconditions+=("main is not in sync with origin/main (local ${local_sha}, remote ${remote_sha}) — run 'git pull --ff-only origin main'")
fi

if (( ${#preconditions[@]} > 0 )); then
  echo "ERR: ${preconditions[0]}" >&2
  for (( i = 1; i < ${#preconditions[@]}; i++ )); do
    echo "ERR: ${preconditions[$i]}" >&2
  done
  if [[ -n "$dirty" ]]; then
    echo "" >&2
    git status --short >&2
  fi
  echo "" >&2
  echo "${#preconditions[@]} precondition(s) failed; nothing was tagged or pushed." >&2
  exit 1
fi

# Step 3 — no terminal to answer the step-7 prompt and no instruction: refuse,
# or `read` hits EOF and a `| tail` pipeline hides the failure. --dry-run never
# prompts, so it is exempt. Runs after the repo preconditions so a caller on
# the wrong branch hears about the branch first.
if [[ ! -t 0 && "$ASSUME_YES" -eq 0 && "$DRY_RUN" -eq 0 ]]; then
  echo "ERR: stdin is not a TTY and neither --yes nor --dry-run was given." >&2
  echo "     The confirmation prompt cannot be answered; refusing to guess." >&2
  echo "     Pass --yes for automation (or --dry-run to only run the gates)." >&2
  exit 2
fi

# Step 4 — tag doesn't already exist
if git rev-parse "$TAG" >/dev/null 2>&1; then
  echo "ERR: tag '$TAG' already exists locally" >&2
  exit 1
fi
# gh API for the remote-tag check too, same SSH-vs-HTTPS reason as step 2.
if command -v gh >/dev/null 2>&1; then
  if gh api "repos/{owner}/{repo}/git/ref/tags/$TAG" >/dev/null 2>&1; then
    echo "ERR: tag '$TAG' already exists on origin" >&2
    exit 1
  fi
elif git ls-remote --exit-code --tags origin "refs/tags/$TAG" >/dev/null 2>&1; then
  echo "ERR: tag '$TAG' already exists on origin" >&2
  exit 1
fi

# Step 5 — CHANGELOG section exists and is non-empty. awk index() rather than
# a regex so dots/dashes in the tag (v0.5.0-rc.1) are literal.
changelog_section=$(awk -v marker="## [$TAG]" '
  index($0, marker) == 1 { capture = 1; next }
  capture && /^## \[/    { exit }
  capture                { print }
' CHANGELOG.md)
if [[ -z "$(printf '%s' "$changelog_section" | tr -d '[:space:]')" ]]; then
  echo "ERR: no non-empty '## [$TAG] — YYYY-MM-DD' section in CHANGELOG.md" >&2
  echo "" >&2
  echo "Cut a release-prep PR first that:" >&2
  echo "  1. Replaces '## [Unreleased]' with '## [$TAG] — $(date -u +%Y-%m-%d)'" >&2
  echo "  2. Adds a fresh '## [Unreleased]' block above it" >&2
  echo "" >&2
  echo "See docs/operations/release-process.md §Cut step 2." >&2
  exit 1
fi

# Step 5b — SLA-proof freshness and verdict. Refuses the cut if the newest
# dated report (not sla-proof-procedure.md, the recipe) is missing, older than
# the window, or recorded a breach. `NOT PROVEN` is a measurement gap and
# passes; `FAIL` is a measured breach that must not ship silently. Same glob
# and date idiom as scripts/ci/check-sla-evidence.sh, deliberately not called:
# its K6/prom readiness gating is a CI-scheduling question, not a cut-safety one.
sla_proof_max_age_days="${SLA_PROOF_MAX_AGE_DAYS:-45}"
newest_sla_proof=""
newest_sla_date=""
for f in docs/operations/sla-proof-[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9].md; do
  [[ -e "$f" ]] || continue
  d="$(basename "$f")"
  d="${d#sla-proof-}"
  d="${d%.md}"
  # ISO-8601 dates sort lexicographically, so > is a date comparison here.
  if [[ -z "$newest_sla_date" || "$d" > "$newest_sla_date" ]]; then
    newest_sla_date="$d"
    newest_sla_proof="$f"
  fi
done

if [[ -z "$newest_sla_proof" ]]; then
  echo "ERR: no docs/operations/sla-proof-<YYYY-MM-DD>.md report found — cut one first (docs/operations/sla-proof-procedure.md)." >&2
  exit 1
fi

# Portable ISO date -> epoch (GNU and BSD date differ).
sla_to_epoch() {
  date -u -d "$1" +%s 2>/dev/null || date -u -j -f "%Y-%m-%d" "$1" +%s 2>/dev/null || echo 0
}
sla_proof_epoch="$(sla_to_epoch "$newest_sla_date")"
if [[ "$sla_proof_epoch" -eq 0 ]]; then
  echo "ERR: ${newest_sla_proof} has an unparseable date '${newest_sla_date}'." >&2
  exit 1
fi
sla_proof_age_days=$(( ($(date -u +%s) - sla_proof_epoch) / 86400 ))
if (( sla_proof_age_days > sla_proof_max_age_days )); then
  echo "ERR: newest SLA proof ${newest_sla_proof} is ${sla_proof_age_days} day(s) old (max ${sla_proof_max_age_days}) — cut a fresh one first." >&2
  exit 1
fi
if grep -q '^\*\*Verdict: FAIL\.\*\*' "$newest_sla_proof"; then
  echo "ERR: newest SLA proof ${newest_sla_proof} records Verdict: FAIL — the SLA was measured and breached; do not cut a release on a known breach." >&2
  exit 1
fi
echo "  OK (${newest_sla_proof}, ${sla_proof_age_days}d old)"

# Step 6 — verify gate: `make prepush`, which verifies the committed HEAD in a
# disposable checkout (pinned Linux container, plus Docker integration shards
# when needed; native without Docker). Bare verify.sh is not the gate: on macOS
# the migrations-sync self-test needs GNU tar, so it ends `VERIFY INCOMPLETE`.
prepush_log="$(mktemp -t cut-release-prepush.XXXXXX)"
echo "→ Running make prepush (the push clearance; this takes ~20 minutes)..."
if ! make prepush >"$prepush_log" 2>&1 ||
  ! grep -q 'ALL REQUIRED CHECKS PASSED' "$prepush_log"; then
  echo "ERR: make prepush FAILED — the release is not cut." >&2
  echo "     Full log: $prepush_log" >&2
  # sigpipe-ok: tail reads the file to completion, no upstream writer.
  tail -n 20 "$prepush_log" >&2
  exit 1
fi
rm -f "$prepush_log"
echo "  OK"

# Step 6b — reconfirm the verified commit is still what "main" points at.
# prepush verifies a snapshot, not the live worktree, and main can move during
# the ~20-minute run; otherwise step 7 would tag a commit never prepushed.
post_sha="$(git rev-parse main 2>/dev/null || echo unknown)"
if [[ "$post_sha" != "$local_sha" ]]; then
  echo "ERR: main moved during 'make prepush' (verified ${local_sha}, now ${post_sha})." >&2
  echo "     Re-run this script against the new commit; nothing was tagged or pushed." >&2
  exit 1
fi

# Step 7 — go / no-go
echo ""
echo "Ready to cut release $TAG"
echo "  branch:        $branch ($local_sha)"
echo "  tag:           $TAG"
# The CHANGELOG section can be many KiB; `head` would EPIPE the writer under
# pipefail, so sed reads to completion.
echo "  CHANGELOG:     $(printf '%s' "$changelog_section" | sed -n '1,3p' | sed 's/^/    /')"
echo ""

if [[ "$DRY_RUN" -eq 1 ]]; then
  echo "[--dry-run] would run:"
  echo "  git tag $TAG"
  echo "  git push origin $TAG"
  echo ""
  echo "Then release.yml would fire and produce a GitHub Release at:"
  echo "  https://github.com/Stellar-Index/StellarIndex/releases/tag/$TAG"
  exit 0
fi

if [[ "$ASSUME_YES" -eq 1 ]]; then
  echo "[--yes] skipping confirmation prompt."
else
  read -r -p "Proceed with tag + push? [y/N] " ans
  if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "Aborted."
    exit 1
  fi
fi

git tag "$TAG" "$local_sha"
# Push to an explicit URL so it works with SSH-keyed and gh-credential-only
# hosts alike.
origin_url=$(git remote get-url origin 2>/dev/null || echo "")
case "$origin_url" in
  git@github.com:*)
    https_url="https://github.com/${origin_url#git@github.com:}"
    git push "$https_url" "$TAG"
    ;;
  https://*)
    git push "$origin_url" "$TAG"
    ;;
  *)
    git push origin "$TAG"
    ;;
esac

echo ""
echo "Tag $TAG pushed. release.yml will fire shortly."
echo "Track progress: gh run list --workflow release.yml --limit 5"
echo "Release page:   https://github.com/Stellar-Index/StellarIndex/releases/tag/$TAG"
