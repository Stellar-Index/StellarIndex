#!/usr/bin/env bash
# assert-deploy-gate.sh — the one step every workflow job that holds a
# production credential runs before it reads that credential.
#
# Fetches the environment's protection config and (for a custom branch
# policy) its branch patterns, then hands both to
# check-deploy-protection.sh. scripts/ci/lint-deploy-credentials.py
# fails CI when a job reads DEPLOY_SSH_PRIVATE_KEY, the ansible vault,
# the r1 inventory or CLOUDFLARE_API_TOKEN without declaring an
# `environment:` and running this script first.
#
# Modes:
#   default    reviewers + main-only branch policy. DEPLOY_APPROVAL_RELAXED
#              (with an unexpired DEPLOY_APPROVAL_RELAXED_UNTIL, see
#              check-deploy-relax.sh) waives the REVIEWER half only; the
#              branch policy is never relaxed.
#   --observe  main-only branch policy only, for scheduled read-only jobs
#              that cannot wait on a human.
#
# Usage:
#   GH_TOKEN=... REPO=owner/repo [APPROVAL_RELAXED=... APPROVAL_RELAXED_UNTIL=...] \
#     bash scripts/ci/assert-deploy-gate.sh [--observe] <environment>
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

check_args=()
if [ "${1:-}" = "--observe" ]; then
  check_args=(--observe)
  shift
fi
if [ "$#" -ne 1 ]; then
  echo "usage: assert-deploy-gate.sh [--observe] <environment>" >&2
  exit 2
fi
ENV_NAME="$1"
: "${REPO:?REPO (owner/repo) must be set}"

if [ "${#check_args[@]}" -eq 0 ]; then
  relax_rc=0
  bash "$HERE/check-deploy-relax.sh" "$ENV_NAME" || relax_rc=$?
  case "$relax_rc" in
    0) check_args=(--observe) ;;
    2) ;;
    *) exit 1 ;;
  esac
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

unreadable() {
  echo "::error::Could not read $1 for environment '${ENV_NAME}'. Refusing to run on an unverified gate. If the default GITHUB_TOKEN lacks access, set the DEPLOY_PROTECTION_TOKEN secret to a read-only PAT and re-run." >&2
  exit 1
}

gh api "repos/${REPO}/environments/${ENV_NAME}" > "$WORK/env.json" ||
  unreadable "the protection config"

policies=()
if jq -e '.deployment_branch_policy.custom_branch_policies == true' "$WORK/env.json" >/dev/null 2>&1; then
  gh api "repos/${REPO}/environments/${ENV_NAME}/deployment-branch-policies?per_page=100" > "$WORK/policies.json" ||
    unreadable "the deployment branch policies"
  policies=("$WORK/policies.json")
fi

bash "$HERE/check-deploy-protection.sh" ${check_args[@]+"${check_args[@]}"} \
  "$WORK/env.json" "$ENV_NAME" ${policies[@]+"${policies[@]}"}
