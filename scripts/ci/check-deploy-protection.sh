#!/usr/bin/env bash
# check-deploy-protection.sh — assert that a production deploy environment
# requires a human approval AND admits only `main`.
#
# Why: `environment: <region>` in .github/workflows/deploy.yml looks like a
# gate but enforces nothing alone; an environment with no `required_reviewers`
# rule runs unattended, and the reviewer list lives in GitHub settings that no
# repo file can set. The repo can refuse to deploy when the gate is missing and
# say how to fix it. The deploy job runs this BEFORE it touches SSH keys or
# production; .github/workflows/deploy-protection.yml runs it alone to catch
# drift between deploys.
#
# Reviewers alone are half a gate: with `deployment_branch_policy: null` a
# dispatch from ANY branch runs that branch's own workflow and playbook, so an
# approval clicks through code nobody reviewed. The environment must also carry
# a custom branch policy whose only pattern is `main`, the precondition for
# moving production credentials into environment-scoped secrets.
#
# Inputs are saved responses from
#   GET /repos/{owner}/{repo}/environments/{environment_name}
#   GET /repos/{owner}/{repo}/environments/{environment_name}/deployment-branch-policies
# so the logic is pure and testable (no network, no token);
# scripts/ci/assert-deploy-gate.sh does the fetching.
#
# Usage:
#   bash scripts/ci/check-deploy-protection.sh [--observe] env.json r1 [policies.json]
#
#   --observe  skip the reviewer requirement (scheduled read-only jobs cannot
#              wait on a human); the branch policy still holds.
#
# Exit 0 = gate present. Exit 1 = gate missing / malformed input. Fail CLOSED.
set -euo pipefail

MODE=deploy
if [ "${1:-}" = "--observe" ]; then
  MODE=observe
  shift
fi

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
  echo "usage: check-deploy-protection.sh [--observe] <environment-json> <environment-name> [<branch-policies-json>]" >&2
  exit 2
fi

JSON="$1"
ENV_NAME="$2"
POLICIES="${3:-}"

if ! command -v jq >/dev/null 2>&1; then
  # Fail closed, but say WHY: otherwise a missing tool reads as "the API
  # returned garbage" and an operator chases phantom tampering. jq is
  # preinstalled on GitHub-hosted runners.
  echo "::error::deploy approval gate NOT VERIFIED — jq is not installed on this runner." >&2
  exit 1
fi

if [ ! -s "$JSON" ]; then
  echo "::error::deploy approval gate NOT VERIFIED — '$JSON' is missing or empty." >&2
  echo "  The environments API response could not be read; refusing to deploy on an unverified gate." >&2
  exit 1
fi

if ! jq -e . "$JSON" >/dev/null 2>&1; then
  echo "::error::deploy approval gate NOT VERIFIED — '$JSON' is not valid JSON." >&2
  exit 1
fi

got_name="$(jq -r '.name // ""' "$JSON")"
if [ "$got_name" != "$ENV_NAME" ]; then
  echo "::error::deploy approval gate NOT VERIFIED — response describes environment '${got_name:-<none>}', expected '$ENV_NAME'." >&2
  exit 1
fi

# A PUT replaces the environment's whole protection config, so the form
# always carries both halves; an omitted half is reset to "none".
api_form() {
  local reviewers=""
  if [ "$MODE" = deploy ]; then
    reviewers="
    -F 'reviewers[][type]=User' -F 'reviewers[][id]=<numeric-user-id>' \\"
  fi
  cat <<EOF
Equivalent API form:

  gh api -X PUT "repos/<owner>/<repo>/environments/$ENV_NAME" \\$reviewers
    -F 'deployment_branch_policy[protected_branches]=false' \\
    -F 'deployment_branch_policy[custom_branch_policies]=true'
  gh api -X POST "repos/<owner>/<repo>/environments/$ENV_NAME/deployment-branch-policies" \\
    -f name=main -f type=branch
EOF
}

assert_reviewers() {
  reviewer_count="$(jq -r '
    [ .protection_rules[]? | select(.type == "required_reviewers") | .reviewers[]? ] | length
  ' "$JSON")"

  if [ "${reviewer_count:-0}" -lt 1 ]; then
    cat >&2 <<EOF
::error::deploy approval gate MISSING on environment '$ENV_NAME' — refusing to deploy.

deploy.yml declares \`environment: $ENV_NAME\`, but that environment has no
\`required_reviewers\` protection rule, so the declaration gates nothing: any
workflow_dispatch (or anything able to trigger one) reaches production with no
human approval.

This cannot be fixed from inside the repository. An operator must, once:

  1. GitHub → Settings → Environments → $ENV_NAME → Deployment protection rules
  2. Tick "Required reviewers" and add at least one reviewer (a user or team
     with write access), then Save.
  3. Re-run this check:
       REPO=<owner>/<repo> bash scripts/ci/assert-deploy-gate.sh $ENV_NAME

$(api_form)
EOF
    exit 1
  fi

  self_review="$(jq -r '
    [ .protection_rules[]? | select(.type == "required_reviewers") | .prevent_self_review ] | first // false
  ' "$JSON")"
}

branch_policy_fail() {
  local what="$1" detail="$2"
  cat >&2 <<EOF
::error::deploy branch policy $what on environment '$ENV_NAME' — refusing to run.

$detail

Only a custom policy whose single pattern is the branch \`main\` can be
verified from here. An operator must, once:

  1. GitHub → Settings → Environments → $ENV_NAME → Deployment branches and tags
  2. Choose "Selected branches and tags", add the branch rule \`main\`, and
     remove every other rule, then Save.

$(api_form)
EOF
  exit 1
}

assert_branch_policy() {
  if ! jq -e '.deployment_branch_policy | type == "object"' "$JSON" >/dev/null; then
    branch_policy_fail MISSING "deployment_branch_policy is null: a dispatch from ANY branch runs that branch's own workflow and playbook with this environment's approval and secrets."
  fi
  if ! jq -e '.deployment_branch_policy.custom_branch_policies == true' "$JSON" >/dev/null; then
    branch_policy_fail "NOT main-only" "The policy is \"protected branches\", which admits every protected branch; which branches those are is not visible here."
  fi
  if [ -z "$POLICIES" ] || [ ! -s "$POLICIES" ] || ! jq -e '.branch_policies | type == "array"' "$POLICIES" >/dev/null 2>&1; then
    branch_policy_fail "NOT VERIFIED" "The environment uses custom branch policies, but the deployment-branch-policies response was not supplied or is not readable."
  fi
  if ! jq -e '.total_count == (.branch_policies | length)' "$POLICIES" >/dev/null; then
    branch_policy_fail "NOT VERIFIED" "The deployment-branch-policies response is a partial page (total_count exceeds the entries returned)."
  fi
  local admitted
  admitted="$(jq -r '[ .branch_policies[] | "\(.type // "branch"):\(.name)" ] | join(", ")' "$POLICIES")"
  if [ "$admitted" != "branch:main" ]; then
    branch_policy_fail "NOT main-only" "The custom policy admits [${admitted:-nothing}], not exactly [branch:main]."
  fi
}

if [ "$MODE" = deploy ]; then
  assert_reviewers
fi
assert_branch_policy

if [ "$MODE" = observe ]; then
  echo "✅ '$ENV_NAME' is main-only (observe mode: no reviewer required)."
  exit 0
fi

echo "✅ deploy approval gate present on '$ENV_NAME': ${reviewer_count} required reviewer(s), prevent_self_review=${self_review}, main-only branch policy."
if [ "$self_review" != "true" ]; then
  # Not fatal: a single-operator project would have every deploy blocked by a
  # second-human demand. The value is that an interactive approval by a
  # designated reviewer is required at all; a token that can dispatch the
  # workflow still cannot approve the deployment.
  echo "::notice::'$ENV_NAME' allows self-review; the approval is a deliberate interactive step, not a second pair of eyes. Enable 'Prevent self-review' once a second reviewer exists."
fi
