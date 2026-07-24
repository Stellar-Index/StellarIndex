#!/usr/bin/env bash
# check-deploy-protection.sh — assert that a production deploy
# environment actually requires a human approval.
#
# Why (deploy-approval-gate, audit-2026-07-23):
#
#   .github/workflows/deploy.yml declares `environment: <region>`, which
#   LOOKS like a gate but enforces nothing on its own — an environment
#   with no `required_reviewers` protection rule runs unattended. The
#   reviewer list lives in GitHub repo settings, which no file in this
#   repo can set. What the repo CAN do is refuse to deploy when the gate
#   is missing, and say exactly how to fix it. That is this script: the
#   deploy job runs it BEFORE it touches SSH keys or production, and
#   .github/workflows/deploy-protection.yml runs it on its own so drift
#   is caught without waiting for a deploy.
#
# The input is a saved response from
#   GET /repos/{owner}/{repo}/environments/{environment_name}
# so the assertion logic is pure and testable (no network, no token).
#
# Usage:
#   gh api "repos/$REPO/environments/r1" > env.json
#   bash scripts/ci/check-deploy-protection.sh env.json r1
#
# Exit 0 = a required-reviewers rule with at least one reviewer exists.
# Exit 1 = gate missing / malformed input. Fail CLOSED, always.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: check-deploy-protection.sh <environment-json> <environment-name>" >&2
  exit 2
fi

JSON="$1"
ENV_NAME="$2"

if ! command -v jq >/dev/null 2>&1; then
  # Fail closed, but say WHY: without this the missing-tool case is
  # indistinguishable from "the API returned garbage", and an operator
  # chases a phantom tampering incident. jq is preinstalled on
  # GitHub-hosted runners.
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
       gh api "repos/<owner>/<repo>/environments/$ENV_NAME" > env.json
       bash scripts/ci/check-deploy-protection.sh env.json $ENV_NAME

Equivalent API form:

  gh api -X PUT "repos/<owner>/<repo>/environments/$ENV_NAME" \\
    -F 'reviewers[][type]=User' -F "reviewers[][id]=<numeric-user-id>"
EOF
  exit 1
fi

self_review="$(jq -r '
  [ .protection_rules[]? | select(.type == "required_reviewers") | .prevent_self_review ] | first // false
' "$JSON")"

echo "✅ deploy approval gate present on '$ENV_NAME': ${reviewer_count} required reviewer(s), prevent_self_review=${self_review}."
if [ "$self_review" != "true" ]; then
  # Deliberately NOT fatal: this is a single-operator project, and
  # demanding a second human would block every deploy. The gate's value
  # here is that an interactive approval by a designated reviewer is
  # required at all — a token that can dispatch the workflow still
  # cannot approve the deployment.
  echo "::notice::'$ENV_NAME' allows self-review; the approval is a deliberate interactive step, not a second pair of eyes. Enable 'Prevent self-review' once a second reviewer exists."
fi
