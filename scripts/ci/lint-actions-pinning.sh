#!/usr/bin/env bash
# lint-actions-pinning.sh — enforce SHA-pinning policy for third-
# party GitHub Actions.
#
# F-1216 (codex audit-2026-05-12): without SHA-pinning, a
# compromised tag on a third-party action repo can land arbitrary
# code on every CI run. `actions/*` is hosted by GitHub itself
# (still mutable but a single trust boundary); everything else
# must be pinned by SHA + version comment.
#
# The gate enumerates `uses:` lines across the workflow tree and
# fails when a third-party one is not pinned to a 40-hex commit SHA.
#
# WHY THE WHOLE TREE AND NOT A DIFF. The hard-fail arm used to look
# only at `git diff origin/main -- .github/workflows/*.yml`, so it
# judged the tree against the ref the tree was standing on: on a
# push-to-main checkout HEAD *is* origin/main, the diff is empty and
# the arm never evaluated. With this project pushing direct to main
# until 1.0 (which bypasses ci.yml's PR-only matrix in the same
# stroke), that made the supply-chain guard inert for every commit
# that actually landed — a commit introducing an unpinned action
# passed it. Scanning the tree is one grep per workflow file and is
# true on every event, so there is no diff base to get wrong.
#
# Workflow:
#   1. Dependabot (already configured for github-actions, see
#      .github/dependabot.yml) opens PRs against third-party actions
#      when the upstream cuts a new version.
#   2. The operator reviewing the Dependabot PR resolves the tag to a
#      commit SHA via `gh api repos/<owner>/<repo>/commits/<tag>
#      --jq .sha` and updates the `uses:` line to that SHA (with the
#      version as a trailing comment).
#   3. This gate refuses the merge until that step has happened.
#
# Repository policy half of F-1216 (allowed_actions=selected,
# require_sha_pinning) is configured via the GitHub admin UI; this
# script enforces the workflow-side discipline.
#
# Usage:
#   bash scripts/ci/lint-actions-pinning.sh              # .github/workflows
#   bash scripts/ci/lint-actions-pinning.sh <dir>...     # explicit roots

set -euo pipefail

cd "$(dirname "$0")/../.."

ROOTS=("${@:-.github/workflows}")

# A pin is a SHA when it's 40 hex chars after the `@`.
SHA_RE='@[0-9a-f]{40}([[:space:]]|$)'

# Both extensions: GitHub honours `.yaml` too, and a gate that reads
# only `.yml` can be stepped around by the file name alone.
FILES=()
while IFS= read -r f; do
  FILES+=("$f")
done < <(find "${ROOTS[@]}" -type f \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null | sort)

if [[ "${#FILES[@]}" -eq 0 ]]; then
  echo "lint-actions-pinning: FAIL — no workflow files under ${ROOTS[*]}; the gate would be vacuous" >&2
  exit 1
fi

USES=0
FAIL=0

for f in "${FILES[@]}"; do
  while IFS= read -r hit; do
    lineno="${hit%%:*}"
    body="${hit#*:}"
    # Anchor on a YAML key boundary. An unanchored `uses:` also matches
    # the tail of ordinary prose — "Usual causes: sshd ..." inside an
    # error message parsed as the action `sshd` and hard-failed the PR
    # that introduced it. A `uses:` key is always at the start of a line
    # or a list item.
    if ! [[ "$body" =~ (^|[[:space:]]|-)uses:[[:space:]]+([^[:space:]]+) ]]; then
      continue
    fi
    ref="${BASH_REMATCH[2]}"
    USES=$((USES + 1))
    # actions/* and github/* are GitHub's own namespaces — one trust
    # boundary, exempted deliberately. Everything else is third-party.
    short="${ref%@*}"
    if [[ "$short" == actions/* || "$short" == github/* ]]; then
      continue
    fi
    if [[ "$ref" =~ $SHA_RE ]]; then
      continue
    fi
    echo "lint-actions-pinning: $f:$lineno third-party action is not SHA-pinned: $ref"
    FAIL=$((FAIL + 1))
  done < <(grep -nE 'uses:[[:space:]]+' "$f" || true)
done

if [[ "$USES" -eq 0 ]]; then
  echo "lint-actions-pinning: FAIL — no \`uses:\` lines across ${#FILES[@]} workflow file(s); the gate would be vacuous" >&2
  exit 1
fi

if [[ "$FAIL" -gt 0 ]]; then
  echo
  echo "lint-actions-pinning: FAIL — $FAIL third-party action(s) not SHA-pinned. Resolve each tag:"
  echo "   gh api repos/<owner>/<repo>/commits/<tag> --jq .sha"
  echo "   then update: uses: vendor/action@<sha>  # <tag>"
  exit 1
fi

echo "lint-actions-pinning: OK — $USES uses: line(s) in ${#FILES[@]} workflow file(s), every third-party action SHA-pinned"
