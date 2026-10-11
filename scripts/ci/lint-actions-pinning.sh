#!/usr/bin/env bash
# lint-actions-pinning.sh — third-party GitHub Actions must be pinned to a
# 40-hex commit SHA (with a version comment).
#
# A compromised tag on a third-party action repo would otherwise run arbitrary
# code on every CI run. `actions/*` and `github/*` are GitHub's own (mutable,
# but one trust boundary) and exempt.
#
# Scans the whole workflow tree, not a diff: a diff-only arm
# (`git diff origin/main -- .github/workflows/*.yml`) is empty on a
# push-to-main checkout, where HEAD *is* origin/main, so an unpinned action
# would pass. A tree scan is true on every event.
#
# Workflow: Dependabot (.github/dependabot.yml) opens the bump PR; the
# reviewer resolves the tag with `gh api repos/<owner>/<repo>/commits/<tag>
# --jq .sha` and writes the SHA with the version as a trailing comment; this
# gate refuses the merge until that is done.
#
# Not enforced here: the repo-level setting (allowed_actions=selected,
# require_sha_pinning). It is unset on this repo
# (`gh api repos/<owner>/<repo>/actions/permissions` shows "all" / false);
# see docs/operations/audit-remediation-operator-actions.md.
#
# Usage:
#   bash scripts/ci/lint-actions-pinning.sh              # .github/workflows
#   bash scripts/ci/lint-actions-pinning.sh <dir>...     # explicit roots

set -euo pipefail

cd "$(dirname "$0")/../.."

ROOTS=("${@:-.github/workflows}")

# A pin is a SHA when it's 40 hex chars after the `@`.
SHA_RE='@[0-9a-f]{40}([[:space:]]|$)'

# Both extensions: GitHub honours `.yaml` too, and reading only `.yml` can be
# bypassed by file name alone.
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
    # Anchor on a YAML key boundary: an unanchored `uses:` matched the tail of
    # prose ("Usual causes: sshd ...") and parsed `sshd` as an action.
    if ! [[ "$body" =~ (^|[[:space:]]|-)uses:[[:space:]]+([^[:space:]]+) ]]; then
      continue
    fi
    ref="${BASH_REMATCH[2]}"
    USES=$((USES + 1))
    # actions/* and github/* are GitHub's own namespaces, exempt by design.
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
